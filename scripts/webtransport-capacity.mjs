import crypto from 'node:crypto';
import fs from 'node:fs/promises';
import { createRequire } from 'node:module';
import os from 'node:os';
import path from 'node:path';
import { performance } from 'node:perf_hooks';
const require = createRequire(new URL('../frontend/package.json', import.meta.url));
const { chromium } = require('playwright');

const userCount = Number(process.env.WEBTRANSPORT_LOAD_USERS);
if (!Number.isInteger(userCount) || userCount < 50 || userCount > 2000) {
  throw new Error('WEBTRANSPORT_LOAD_USERS must be an integer from 50 to 2000');
}
const hotUserCount = Math.floor(userCount * 0.24);
const documentCount = 20;
const editCount = 30;
const closeDeadline = timeout => new Promise(resolve => setTimeout(() => resolve(false), timeout));

function configuredURL(name, fallback, allowedProtocols) {
  const value = process.env[name] || fallback;
  const url = new URL(value);
  if (!allowedProtocols.includes(url.protocol) || url.username || url.password || url.hash) {
    throw new Error(`${name} must be an HTTP(S) URL without credentials or a fragment`);
  }
  return url;
}

const restURLs = [
  configuredURL('REST_A_URL', `http://127.0.0.1:${process.env.REST_A}`, ['http:', 'https:']),
  configuredURL('REST_B_URL', `http://127.0.0.1:${process.env.REST_B}`, ['http:', 'https:']),
];
for (const url of restURLs) {
  if (url.pathname !== '/' || url.search) throw new Error('REST URLs must contain only an origin');
}
const endpoints = [
  configuredURL('WT_A_URL', `https://127.0.0.1:${process.env.WT_A}/api/webtransport`, ['https:']),
  configuredURL('WT_B_URL', `https://127.0.0.1:${process.env.WT_B}/api/webtransport`, ['https:']),
];
for (const url of endpoints) {
  if (url.pathname !== '/api/webtransport' || url.search) throw new Error('WebTransport URLs must use /api/webtransport');
}
const baseURL = configuredURL('CAPACITY_PAGE_URL', `http://127.0.0.1:${process.env.VITE_A}/auth`, ['http:', 'https:']);
if (baseURL.protocol === 'http:' && !['127.0.0.1', 'localhost', '[::1]'].includes(baseURL.hostname)) {
  throw new Error('the browser page must use HTTPS or a local secure context');
}

function testBrowserProfile(server) {
  const argument = server.process().spawnargs.find(value => value.startsWith('--user-data-dir='));
  const profile = argument?.slice('--user-data-dir='.length);
  if (!profile || path.dirname(profile) !== os.tmpdir() || !path.basename(profile).startsWith('playwright_chromiumdev_profile-')) {
    throw new Error('could not identify test-owned Chrome profile');
  }
  return profile;
}

async function request(baseURL, path, method, body, token) {
  const response = await fetch(new URL(path, baseURL), {
    method,
    signal: AbortSignal.timeout(15000),
    headers: { 'Content-Type': 'application/json', ...(token ? { Authorization: `Bearer ${token}` } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!response.ok) {
    throw new Error(`${method} ${path} returned HTTP ${response.status}`);
  }
  const text = await response.text();
  return text ? JSON.parse(text) : {};
}

async function provisionUsers() {
  const password = crypto.randomBytes(20).toString('hex');
  const prefix = `b${Date.now().toString(36)}`;
  const users = Array.from({ length: userCount });
  const started = performance.now();
  // The isolated service keeps its normal 1,000-per-minute registration limit.
  const preparationIntervalMs = userCount > 1000 ? 75 : 0;
  let next = 0;
  await Promise.all(Array.from({ length: 8 }, async () => {
    while (next < userCount) {
      const index = next++;
      const waitMs = started + index * preparationIntervalMs - performance.now();
      if (waitMs > 0) await new Promise(resolve => setTimeout(resolve, waitMs));
      const restURL = restURLs[index % restURLs.length];
      const body = { username: `${prefix}u${index}`, password };
      const { id } = await request(restURL, '/api/auth/register', 'POST', body);
      const { access_token: token } = await request(restURL, '/api/auth/login', 'POST', body);
      const doc = index < hotUserCount ? 0 : 1 + Math.floor((index - hotUserCount) * (documentCount - 1) / (userCount - hotUserCount));
      users[index] = { i: index, id, token, doc };
    }
  }));
  const docs = Array.from({ length: documentCount }, (_, index) => `wt${crypto.randomBytes(6).toString('hex')}${index}`);
  const permissionsStarted = performance.now();
  let permissionIndex = 0;
  for (const [index, doc] of docs.entries()) {
    const members = users.filter(user => user.doc === index);
    if (members.length === 0) continue;
    const owner = members[0];
    await request(restURLs[0], '/api/documents', 'POST', { id: doc, content: '' }, owner.token);
    for (const user of members.slice(1)) {
      const waitMs = permissionsStarted + permissionIndex++ * preparationIntervalMs - performance.now();
      if (waitMs > 0) await new Promise(resolve => setTimeout(resolve, waitMs));
      await request(restURLs[0], `/api/documents/${doc}/permissions/${user.id}`, 'PUT', { role: 'write' }, owner.token);
    }
  }
  console.log(JSON.stringify({ provisionedUsers: users.length, populatedDocuments: new Set(users.map(user => user.doc)).size }));
  return { users, docs };
}

const hash = process.env.WT_HASH;
const steps = [...new Set([50, 100, 256, 512, 1000, 1500, 2000, userCount].filter(step => step <= userCount))].sort((a, b) => a - b);
const results = [];
const servers = [];
const profiles = new Map();
const browsers = [];
const pages = [];
let status = 0;
try {
  const { users, docs } = await provisionUsers();
  // 250 sessions per Chrome repeatedly stalled browser shutdown on this host.
  const browserCount = Math.ceil(userCount / 125);
  for (let index = 0; index < browserCount; index++) {
    const server = await chromium.launchServer({ channel: 'chrome', headless: true });
    servers.push(server);
    profiles.set(server, testBrowserProfile(server));
  }
  for (const server of servers) {
    browsers.push(await chromium.connect(server.wsEndpoint()));
  }
  for (const browser of browsers) {
    const page = await browser.newPage();
    pages.push(page);
    await page.goto(baseURL.href);
  }
  for (const target of steps) {
    const previous = results.at(-1)?.target ?? 0;
    const batch = users.slice(previous, target);
    const assignments = pages.map(() => []);
    for (const user of batch) assignments[user.i % pages.length].push(user);
    const start = performance.now();
    const parts = await Promise.all(pages.map((page, index) => page.evaluate(async ({ group, docs, endpoints, hash }) => {
      const state = globalThis.__wtCapacity ??= { sessions: [], closed: 0, connected: 0, acks: {}, lastVersion: 0 };
      const digest = Uint8Array.from(atob(hash), character => character.charCodeAt(0));
      const options = { serverCertificateHashes: [{ algorithm: 'sha-256', value: digest }] };
      const encoder = new TextEncoder();
      const decoder = new TextDecoder();
      const timeout = (millis, reason) => new Promise((_, reject) => setTimeout(() => reject(new Error(reason)), millis));
      const connect = async (user, endpoint, hops = 0) => {
        if (hops > 3) throw new Error('redirect limit');
        const started = performance.now();
        const session = new WebTransport(endpoint, options);
        let joined = false;
        try {
          await Promise.race([session.ready, timeout(10000, 'WebTransport ready timeout')]);
          const stream = await Promise.race([session.createBidirectionalStream(), timeout(10000, 'stream timeout')]);
          const writer = stream.writable.getWriter();
          const reader = stream.readable.getReader();
          const send = message => {
            const payload = encoder.encode(JSON.stringify(message));
            const frame = new Uint8Array(payload.length + 4);
            new DataView(frame.buffer).setUint32(0, payload.length);
            frame.set(payload, 4);
            return writer.write(frame);
          };
          let resolveOutcome, rejectOutcome;
          const outcome = new Promise((resolve, reject) => { resolveOutcome = resolve; rejectOutcome = reject; });
          void (async () => {
            let pending = new Uint8Array(0);
            try {
              while (true) {
                const { value, done } = await reader.read();
                if (done) throw new Error('stream closed');
                const next = new Uint8Array(pending.length + value.length);
                next.set(pending); next.set(value, pending.length); pending = next;
                while (pending.length >= 4) {
                  const size = new DataView(pending.buffer, pending.byteOffset, pending.byteLength).getUint32(0);
                  if (!size || size > 1 << 20) throw new Error('invalid frame');
                  if (pending.length < size + 4) break;
                  const message = JSON.parse(decoder.decode(pending.slice(4, size + 4)));
                  pending = pending.slice(size + 4);
                  if (message.type === 'auth_ok') await send({ type: 'join', doc_id: docs[user.doc], last_version: 0 });
                  else if (message.type === 'owner_redirect') resolveOutcome({ redirect: message.endpoint });
                  else if (message.type === 'joined' || message.type === 'sync' || message.type === 'resync_required') {
                    joined = true; state.lastVersion = message.version ?? 0; resolveOutcome({ joined: true });
                  } else if (message.type === 'edit_applied') { const ack = state.acks[message.request_id]; if (ack) { ack(message); delete state.acks[message.request_id]; } state.lastVersion = Math.max(state.lastVersion, message.version ?? 0); } else if (message.type === 'error') rejectOutcome(new Error(message.code));
                }
              }
            } catch (error) { if (!joined) rejectOutcome(error); }
          })();
          await send({ type: 'auth', token: user.token });
          const answer = await Promise.race([outcome, timeout(12000, 'join timeout')]);
          if (answer.redirect) {
            session.close({ closeCode: 0, reason: 'redirect' });
            const next = new URL(answer.redirect);
            next.pathname = '/api/webtransport';
            return connect(user, next.toString(), hops + 1);
          }
          if (user.i === 0) state.editSender = send;
          state.sessions.push({ session, reader, writer });
          state.connected++;
          void session.closed.then(() => { state.closed++; }, () => { state.closed++; });
          return { latency: performance.now() - started, hops };
        } catch (error) {
          try { session.close({ closeCode: 0, reason: 'setup failed' }); } catch { /* closed */ }
          throw error;
        }
      };
      const success = [], failures = {}, initialFailures = {};
      let next = 0;
      await Promise.all(Array.from({ length: 12 }, async () => {
        while (next < group.length) {
          const user = group[next++];
          const firstAttemptAt = performance.now();
          try { success.push(await connect(user, endpoints[user.i % 2])); }
          catch (error) {
            const key = String(error);
            initialFailures[key] = (initialFailures[key] ?? 0) + 1;
            if (key.includes('Connection lost') || key.includes('session is closed')) {
              try {
                const recovered = await connect(user, endpoints[user.i % 2]);
                success.push({ ...recovered, latency: performance.now() - firstAttemptAt });
              } catch (retryError) {
                const retryKey = String(retryError);
                failures[retryKey] = (failures[retryKey] ?? 0) + 1;
              }
            } else {
              failures[key] = (failures[key] ?? 0) + 1;
            }
          }
        }
      }));
      return { success, failures, initialFailures, total: state.connected, closed: state.closed };
    }, { group: assignments[index], docs, endpoints: endpoints.map(url => url.href), hash })));
    const times = parts.flatMap(part => part.success.map(item => item.latency)).sort((a, b) => a - b);
    const failures = {}, initialFailures = {}; for (const part of parts) { for (const [reason, count] of Object.entries(part.failures)) failures[reason] = (failures[reason] ?? 0) + count; for (const [reason, count] of Object.entries(part.initialFailures)) initialFailures[reason] = (initialFailures[reason] ?? 0) + count; }
    await new Promise(resolve => setTimeout(resolve, 1500));
    const snapshots = await Promise.all(pages.map(page => page.evaluate(() => ({ total: globalThis.__wtCapacity.connected, closed: globalThis.__wtCapacity.closed }))));
    const total = snapshots.reduce((sum, part) => sum + part.total, 0);
    const closed = snapshots.reduce((sum, part) => sum + part.closed, 0);
    const result = { target, total, closed, newSuccess: times.length, initialFailures, failures,
      p95Ms: times.length ? Math.round(times[Math.ceil(times.length * 0.95) - 1]) : null,
      elapsedMs: Math.round(performance.now() - start) };
    results.push(result);
    console.log(JSON.stringify(result));
    if (Object.keys(failures).length || total !== target || closed) { status = 1; break; }
  }
  if (!status) {
    const editLatency = [];
    const online = [];
    for (let index = 0; index < editCount; index++) {
      const cycle = performance.now();
      const requestId = `hot-${index}`;
      const result = await pages[0].evaluate(async ({ requestId, docID }) => {
        const state = globalThis.__wtCapacity;
        const ack = new Promise(resolve => { state.acks[requestId] = resolve; });
        const started = performance.now();
        await state.editSender({ type: 'edit', request_id: requestId, doc_id: docID, base_version: state.lastVersion,
          operation: { kind: 'insert', position: 0, text: 'x' } });
        const message = await Promise.race([ack, new Promise((_, reject) => setTimeout(() => reject(new Error(`edit ack timeout ${requestId}`)), 5000))]);
        return { ms: performance.now() - started, version: message.version };
      }, { requestId, docID: docs[0] });
      editLatency.push(result.ms);
      if ((index + 1) % 10 === 0) {
        const totals = await Promise.all(pages.map(page => page.evaluate(() => ({ total: globalThis.__wtCapacity.connected, closed: globalThis.__wtCapacity.closed }))));
        const observation = { edits: index + 1, total: totals.reduce((n, item) => n + item.total, 0), closed: totals.reduce((n, item) => n + item.closed, 0), version: result.version };
        online.push(observation);
        console.log(JSON.stringify(observation));
        if (observation.closed || observation.total !== userCount || observation.version !== index + 1) { status = 1; break; }
      }
      await new Promise(resolve => setTimeout(resolve, Math.max(0, 1000 - (performance.now() - cycle))));
    }
    editLatency.sort((a,b) => a-b);
    const editResult = { completed: editLatency.length, p50Ms: Math.round(editLatency[Math.floor(editLatency.length * .5)] ?? 0),
      p95Ms: Math.round(editLatency[Math.ceil(editLatency.length * .95)-1] ?? 0), maxMs: Math.round(editLatency.at(-1) ?? 0), online };
    console.log(JSON.stringify({ editResult: { ...editResult, online: online.at(-1) } }));
    if (editLatency.length !== editCount) status = 1;
  }
} catch (error) {
  console.error(`capacity test failed: ${error}`);
  status = 1;
} finally {
  const sessionCloseRequests = await Promise.race([
    Promise.all(pages.map(page => page.evaluate(async () => {
      const sessions = globalThis.__wtCapacity?.sessions ?? [];
      for (let index = 0; index < sessions.length; index += 20) {
        for (const { session, reader, writer } of sessions.slice(index, index + 20)) {
          void reader.cancel('load complete').catch(() => {});
          void writer.close().catch(() => {});
          try { session.close({ closeCode: 0, reason: 'load complete' }); } catch { /* already closed */ }
        }
        await new Promise(resolve => setTimeout(resolve, 50));
      }
    }).then(() => true, () => false))).then(outcomes => outcomes.every(Boolean)), closeDeadline(30000)
  ]);
  const browsersDisconnected = await Promise.race([
    Promise.all(browsers.map(browser => browser.close().then(() => true, () => false))).then(outcomes => outcomes.every(Boolean)), closeDeadline(30000)
  ]);
  const serverShutdown = await Promise.all(servers.map(async server => {
    const graceful = await Promise.race([server.close().then(() => true, () => false), closeDeadline(10000)]);
    if (graceful) return { graceful: true, stopped: true };
    const killSettled = await Promise.race([server.kill().then(() => true, () => false), closeDeadline(30000)]);
    const child = server.process();
    const processExited = child.exitCode !== null || child.signalCode !== null;
    return { graceful: false, stopped: killSettled || processExited, killSettled, processExited };
  }));
  const profileCleanup = await Promise.all(servers.map(async (server, index) => {
    if (!serverShutdown[index].stopped || !profiles.has(server)) return false;
    try { await fs.rm(profiles.get(server), { recursive: true, force: true }); return true; }
    catch { return false; }
  }));
  const shutdown = {
    graceful: serverShutdown.filter(result => result.graceful).length,
    forced: serverShutdown.filter(result => !result.graceful && result.stopped).length,
    unresolved: serverShutdown.filter(result => !result.stopped).length,
    profilesCleaned: profileCleanup.filter(Boolean).length,
    browserProcesses: servers.length,
  };
  console.log(JSON.stringify({ sessionCloseRequests, browsersDisconnected, shutdown }));
  if (!sessionCloseRequests || !browsersDisconnected || shutdown.unresolved || shutdown.profilesCleaned !== servers.length) status = 2;
}
process.exit(status);
