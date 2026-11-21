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

function integerSetting(name, fallback, minimum, maximum) {
  const raw = process.env[name];
  const value = raw === undefined ? fallback : Number(raw);
  if (!Number.isInteger(value) || value < minimum || value > maximum) {
    throw new Error(`${name} must be an integer from ${minimum} to ${maximum}`);
  }
  return value;
}

const connectConcurrency = integerSetting('WEBTRANSPORT_CONNECT_CONCURRENCY', 12, 1, 64);
const connectIntervalMs = integerSetting('WEBTRANSPORT_CONNECT_INTERVAL_MS', 0, 0, 1000);
const failoverMode = process.env.WEBTRANSPORT_FAILOVER_MODE === 'true';
const failoverControlURL = process.env.FAILOVER_CONTROL_URL ? new URL(process.env.FAILOVER_CONTROL_URL) : null;
const failoverControlToken = process.env.FAILOVER_CONTROL_TOKEN || '';
const failoverTriggerMode = process.env.FAILOVER_TRIGGER_MODE || 'harness';
const failoverOwnerIndex = integerSetting('FAILOVER_OWNER_INDEX', 0, 0, 1);
const failoverSettleMs = integerSetting('FAILOVER_SETTLE_MS', 90000, 30000, 120000);
if (failoverMode && (!failoverControlURL || !failoverControlToken)) {
  throw new Error('WEBTRANSPORT_FAILOVER_MODE requires FAILOVER_CONTROL_URL and FAILOVER_CONTROL_TOKEN');
}
if (!['harness', 'external'].includes(failoverTriggerMode)) {
  throw new Error('FAILOVER_TRIGGER_MODE must be harness or external');
}

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
const baseURL = configuredURL('CAPACITY_PAGE_URL', `http://127.0.0.1:${process.env.VITE_A}/e2e/webtransport-capacity.html`, ['http:', 'https:']);
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

async function failoverControl(path, method = 'GET') {
  if (!failoverMode || !failoverControlURL) return null;
  const response = await fetch(new URL(path, failoverControlURL), {
    method,
    headers: { 'X-Failover-Token': failoverControlToken },
    signal: AbortSignal.timeout(5000),
  });
  if (!response.ok) throw new Error(`failover control ${method} ${path} returned HTTP ${response.status}`);
  return response.json();
}

function rendezvousScore(documentID, nodeID, incarnation) {
  const parts = [documentID, nodeID, incarnation].map(value => {
    const length = Buffer.alloc(4);
    length.writeUInt32BE(Buffer.byteLength(value));
    return Buffer.concat([length, Buffer.from(value)]);
  });
  return crypto.createHash('sha256').update(Buffer.concat(parts)).digest();
}

// Owner selection is rendezvous hashing over (document, node, incarnation). A
// normal document set splits roughly evenly between nodes, so a single-node
// failover drill must select documents that all rank the target node first.
function ownerPreferredDocuments(count) {
  const ownerNode = process.env.FAILOVER_OWNER_NODE_ID || 'node-a';
  const ownerIncarnation = process.env.FAILOVER_OWNER_INCARNATION || 'process-a';
  const peerNode = process.env.FAILOVER_PEER_NODE_ID || 'node-b';
  const peerIncarnation = process.env.FAILOVER_PEER_INCARNATION || 'process-b';
  const docs = [];
  for (let index = 0; docs.length < count && index < count * 100; index++) {
    const id = `wt${crypto.randomBytes(6).toString('hex')}${index}`;
    const ownerScore = rendezvousScore(id, ownerNode, ownerIncarnation);
    const peerScore = rendezvousScore(id, peerNode, peerIncarnation);
    if (Buffer.compare(ownerScore, peerScore) >= 0) docs.push(id);
  }
  if (docs.length < count) throw new Error('could not generate owner-preferred document IDs');
  return docs;
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
  const docs = failoverMode
    ? ownerPreferredDocuments(documentCount)
    : Array.from({ length: documentCount }, (_, index) => `wt${crypto.randomBytes(6).toString('hex')}${index}`);
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
  for (const [index, browser] of browsers.entries()) {
    const page = await browser.newPage();
    pages.push(page);
    const navigationDiagnostics = [];
    const recordNavigationDiagnostic = value => {
      if (navigationDiagnostics.length < 5) navigationDiagnostics.push(value);
    };
    page.on('requestfailed', request => {
      const url = new URL(request.url());
      recordNavigationDiagnostic(`request_failed:${url.origin === baseURL.origin ? url.pathname : url.origin}:${request.failure()?.errorText ?? 'unknown'}`);
    });
    page.on('response', response => {
      if (response.status() >= 400) {
        const url = new URL(response.url());
        recordNavigationDiagnostic(`http_${response.status()}:${url.origin === baseURL.origin ? url.pathname : url.origin}`);
      }
    });
    try {
      const response = await page.goto(baseURL.href, { waitUntil: 'domcontentloaded' });
      if (!response?.ok()) throw new Error(`capacity page returned HTTP ${response?.status() ?? 'unknown'}`);
    } catch (error) {
      throw new Error(`phase=page_navigation page_index=${index} diagnostics=${JSON.stringify(navigationDiagnostics)}: ${String(error)}`);
    }
  }
  for (const target of steps) {
    const previous = results.at(-1)?.target ?? 0;
    const batch = users.slice(previous, target);
    const assignments = pages.map(() => []);
    for (const user of batch) assignments[user.i % pages.length].push(user);
    const start = performance.now();
    const parts = await Promise.all(pages.map((page, index) => page.evaluate(async ({ group, docs, endpoints, hash, connectConcurrency, connectIntervalMs, failoverMode, failoverOwnerIndex }) => {
      const state = globalThis.__wtCapacity ??= { sessions: [], closed: 0, connected: 0, active: 0, disconnects: 0, reconnects: 0, reconnectFailures: 0, reconnectErrors: {}, reconnectLatencies: [], byEndpoint: {}, shuttingDown: false, failover: false, acks: {}, lastVersion: 0 };
      state.failover = failoverMode;
      const digest = Uint8Array.from(atob(hash), character => character.charCodeAt(0));
      const options = { serverCertificateHashes: [{ algorithm: 'sha-256', value: digest }] };
      const encoder = new TextEncoder();
      const decoder = new TextDecoder();
      const timeout = (millis, reason) => new Promise((_, reject) => setTimeout(() => reject(new Error(reason)), millis));
      let nextConnectAt = performance.now();
      const paceConnection = async () => {
        if (!connectIntervalMs) return;
        const scheduledAt = nextConnectAt;
        nextConnectAt += connectIntervalMs;
        const wait = scheduledAt - performance.now();
        if (wait > 0) await new Promise(resolve => setTimeout(resolve, wait));
      };
      const connect = async (user, endpoint, hops = 0, reconnecting = false) => {
        if (hops > 3) throw new Error('redirect limit');
        await paceConnection();
        const started = performance.now();
        const session = new WebTransport(endpoint, options);
        let joined = false;
        try {
          await Promise.race([session.ready, timeout(reconnecting ? 4000 : 10000, 'WebTransport ready timeout')]);
          const stream = await Promise.race([session.createBidirectionalStream(), timeout(reconnecting ? 4000 : 10000, 'stream timeout')]);
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
          const answer = await Promise.race([outcome, timeout(reconnecting ? 6000 : 12000, 'join timeout')]);
          if (answer.redirect) {
            session.close({ closeCode: 0, reason: 'redirect' });
            const next = new URL(answer.redirect);
            next.pathname = '/api/webtransport';
            if (reconnecting && next.href === endpoints[failoverOwnerIndex]) throw new Error('owner redirect target is unreachable');
            return connect(user, next.toString(), hops + 1, reconnecting);
          }
          if (user.i === 0) state.editSender = send;
          const entry = { session, reader, writer, user, endpoint };
          state.sessions.push(entry);
          if (!reconnecting) state.connected++;
          state.active++;
          state.byEndpoint[endpoint] = (state.byEndpoint[endpoint] ?? 0) + 1;
          if (reconnecting) state.reconnects++;
          const handleClose = () => {
            state.closed++;
            state.active = Math.max(0, state.active - 1);
            state.byEndpoint[endpoint] = Math.max(0, (state.byEndpoint[endpoint] ?? 0) - 1);
            if (!state.failover || state.shuttingDown) return;
            state.disconnects++;
            enqueueReconnect(user, performance.now());
          };
          void session.closed.then(handleClose, handleClose);
          return { latency: performance.now() - started, hops };
        } catch (error) {
          try { session.close({ closeCode: 0, reason: 'setup failed' }); } catch { /* closed */ }
          const port = new URL(endpoint).port;
          throw new Error(`hop=${hops} endpoint_port=${port}: ${String(error)}`);
        }
      };
      const initialEndpoint = user => failoverMode ? endpoints[0] : endpoints[user.i % 2];
      const reconnectQueue = [];
      let reconnectActive = 0;
      const enqueueReconnect = (user, disconnectedAt) => {
        reconnectQueue.push({ user, disconnectedAt });
        pumpReconnects();
      };
      const pumpReconnects = () => {
        if (state.shuttingDown) return;
        while (reconnectActive < 8 && reconnectQueue.length) {
          const job = reconnectQueue.shift();
          reconnectActive++;
          void (async () => {
            try {
              await new Promise(resolve => setTimeout(resolve, Math.random() * 300));
              await reconnect(job.user);
              state.reconnectLatencies.push(performance.now() - job.disconnectedAt);
            } catch (error) {
              state.reconnectFailures++;
              const reason = String(error);
              state.reconnectErrors[reason] = (state.reconnectErrors[reason] ?? 0) + 1;
            } finally {
              reconnectActive--;
              pumpReconnects();
            }
          })();
        }
      };
      const reconnect = async user => {
        let lastError;
        const peerEndpoint = endpoints[(failoverOwnerIndex + 1) % 2];
        const ownerEndpoint = endpoints[failoverOwnerIndex];
        for (let attempt = 0; attempt < 10; attempt++) {
          for (const candidate of [peerEndpoint, ownerEndpoint]) {
            if (state.shuttingDown) throw new Error('shutting down');
            try { return await connect(user, candidate, 0, true); }
            catch (error) { lastError = error; }
          }
          await new Promise(resolve => setTimeout(resolve, 400 + Math.min(1200, 200 * attempt)));
        }
        throw lastError ?? new Error('reconnect failed');
      };
      const success = [], failures = {}, initialFailures = {};
      let next = 0;
      const workers = Math.min(connectConcurrency, Math.max(group.length, 1));
      await Promise.all(Array.from({ length: workers }, async () => {
        while (next < group.length) {
          const user = group[next++];
          const firstAttemptAt = performance.now();
          try { success.push(await connect(user, initialEndpoint(user))); }
          catch (error) {
            const key = String(error);
            initialFailures[key] = (initialFailures[key] ?? 0) + 1;
            if (key.includes('Connection lost') || key.includes('session is closed')) {
              try {
                const recovered = await connect(user, initialEndpoint(user));
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
    }, { group: assignments[index], docs, endpoints: endpoints.map(url => url.href), hash, connectConcurrency, connectIntervalMs, failoverMode, failoverOwnerIndex })));
    const times = parts.flatMap(part => part.success.map(item => item.latency)).sort((a, b) => a - b);
    const failures = {}, initialFailures = {}; for (const part of parts) { for (const [reason, count] of Object.entries(part.failures)) failures[reason] = (failures[reason] ?? 0) + count; for (const [reason, count] of Object.entries(part.initialFailures)) initialFailures[reason] = (initialFailures[reason] ?? 0) + count; }
    await new Promise(resolve => setTimeout(resolve, 1500));
    const snapshots = await Promise.all(pages.map(page => page.evaluate(() => ({ total: globalThis.__wtCapacity.connected, closed: globalThis.__wtCapacity.closed }))));
    const total = snapshots.reduce((sum, part) => sum + part.total, 0);
    const closed = snapshots.reduce((sum, part) => sum + part.closed, 0);
    const result = { target, total, closed, newSuccess: times.length, connectConcurrency, connectIntervalMs, initialFailures, failures,
      p95Ms: times.length ? Math.round(times[Math.ceil(times.length * 0.95) - 1]) : null,
      elapsedMs: Math.round(performance.now() - start) };
    results.push(result);
    console.log(JSON.stringify(result));
    if (Object.keys(initialFailures).length || Object.keys(failures).length || total !== target || closed) { status = 1; break; }
  }
  let editVersionBase = 0;
  if (!status && failoverMode) {
    const readyStates = await Promise.all(pages.map(page => page.evaluate(() => ({ version: globalThis.__wtCapacity.lastVersion, active: globalThis.__wtCapacity.active, byEndpoint: globalThis.__wtCapacity.byEndpoint }))));
    const before = {
      version: readyStates[0]?.version ?? 0,
      active: readyStates.reduce((sum, item) => sum + item.active, 0),
      byEndpoint: readyStates.reduce((merged, item) => {
        for (const [endpoint, count] of Object.entries(item.byEndpoint ?? {})) merged[endpoint] = (merged[endpoint] ?? 0) + count;
        return merged;
      }, {}),
    };
    if (before.active !== userCount) throw new Error(`READY requires ${userCount} active sessions, got ${before.active}`);
    const ownerEndpoint = endpoints[0].href;
    const ownerSessions = before.byEndpoint[ownerEndpoint] ?? 0;
    if (ownerSessions !== userCount) throw new Error(`READY requires ${userCount} owner sessions, got ${ownerSessions}`);
    const ready = await failoverControl('/ready', 'POST');
    console.log(JSON.stringify({ failover: 'READY', version: before.version, active: before.active, ownerEndpoint, ownerSessions, byEndpoint: before.byEndpoint, ready }));
    if (failoverTriggerMode === 'harness') {
      await new Promise(resolve => setTimeout(resolve, 3000));
      const triggeredByHarness = await failoverControl('/trigger', 'POST');
      console.log(JSON.stringify({ failover: 'TRIGGER', source: 'harness', state: triggeredByHarness }));
    } else {
      const triggerDeadline = Date.now() + 300000;
      let triggered = false;
      while (Date.now() < triggerDeadline) {
        const controlState = await failoverControl('/state');
        if (controlState?.triggered) { triggered = true; break; }
        await new Promise(resolve => setTimeout(resolve, 1000));
      }
      if (!triggered) throw new Error('failover trigger timeout');
      console.log(JSON.stringify({ failover: 'TRIGGER', source: 'external' }));
    }
    const recoveryStarted = Date.now();
    let observedDisconnect = false;
    let stableSince = 0;
    let lastCounts = '';
    while (Date.now() - recoveryStarted < 180000) {
      const snapshots = await Promise.all(pages.map(page => page.evaluate(() => ({
        active: globalThis.__wtCapacity.active,
        disconnects: globalThis.__wtCapacity.disconnects,
        reconnects: globalThis.__wtCapacity.reconnects,
        reconnectFailures: globalThis.__wtCapacity.reconnectFailures,
      }))));
      const active = snapshots.reduce((sum, item) => sum + item.active, 0);
      const disconnects = snapshots.reduce((sum, item) => sum + item.disconnects, 0);
      const reconnects = snapshots.reduce((sum, item) => sum + item.reconnects, 0);
      const reconnectFailures = snapshots.reduce((sum, item) => sum + item.reconnectFailures, 0);
      if (disconnects > 0) observedDisconnect = true;
      const settled = Date.now() - recoveryStarted >= failoverSettleMs;
      const recovered = settled && observedDisconnect && active === userCount && reconnectFailures === 0 && reconnects >= 1 && reconnects === disconnects;
      const counts = `${active}/${disconnects}/${reconnects}/${reconnectFailures}`;
      if (recovered && counts === lastCounts) {
        if (Date.now() - stableSince >= 5000) break;
      } else {
        stableSince = Date.now();
        lastCounts = counts;
      }
      await new Promise(resolve => setTimeout(resolve, 500));
    }
    if (!observedDisconnect) throw new Error('failover did not disconnect any session');
    const recovery = await Promise.all(pages.map(page => page.evaluate(() => ({
      active: globalThis.__wtCapacity.active,
      disconnects: globalThis.__wtCapacity.disconnects,
      reconnects: globalThis.__wtCapacity.reconnects,
      reconnectFailures: globalThis.__wtCapacity.reconnectFailures,
      reconnectLatencies: globalThis.__wtCapacity.reconnectLatencies,
      reconnectErrors: globalThis.__wtCapacity.reconnectErrors,
      version: globalThis.__wtCapacity.lastVersion,
    }))));
    const active = recovery.reduce((sum, item) => sum + item.active, 0);
    const disconnects = recovery.reduce((sum, item) => sum + item.disconnects, 0);
    const reconnects = recovery.reduce((sum, item) => sum + item.reconnects, 0);
    const reconnectFailures = recovery.reduce((sum, item) => sum + item.reconnectFailures, 0);
    const latency = recovery.flatMap(item => item.reconnectLatencies).sort((a, b) => a - b);
    const reconnectErrors = {}; for (const item of recovery) for (const [reason, count] of Object.entries(item.reconnectErrors ?? {})) reconnectErrors[reason] = (reconnectErrors[reason] ?? 0) + count;
    const percentile = value => Math.round(latency[Math.min(latency.length - 1, Math.ceil(latency.length * value) - 1)] ?? 0);
    editVersionBase = recovery[0].version;
    const summary = {
      failover: 'RECOVERY',
      active,
      disconnects,
      reconnects,
      reconnectFailures,
      reconnectErrors,
      reconnectP50Ms: percentile(0.5),
      reconnectP95Ms: percentile(0.95),
      reconnectP99Ms: percentile(0.99),
      preFailoverVersion: before.version,
      recoveryVersion: editVersionBase,
      online: recovery.map(item => ({ active: item.active, disconnects: item.disconnects, reconnects: item.reconnects, reconnectFailures: item.reconnectFailures })),
    };
    console.log(JSON.stringify(summary));
    if (active !== userCount) status = 1;
    if (disconnects === 0 || reconnects === 0) status = 1;
    if (reconnectFailures) status = 1;
    if (editVersionBase < before.version) status = 1;
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
        const totals = await Promise.all(pages.map(page => page.evaluate(() => ({ total: globalThis.__wtCapacity.connected, active: globalThis.__wtCapacity.active, closed: globalThis.__wtCapacity.closed }))));
        const onlineCount = totals.reduce((n, item) => n + (failoverMode ? item.active : item.total), 0);
        const observation = { edits: index + 1, total: onlineCount, closed: totals.reduce((n, item) => n + item.closed, 0), version: result.version };
        online.push(observation);
        console.log(JSON.stringify(observation));
        const invalidOnline = failoverMode
          ? onlineCount !== userCount
          : observation.closed || onlineCount !== userCount;
        if (invalidOnline || observation.version !== editVersionBase + index + 1) { status = 1; break; }
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
      if (globalThis.__wtCapacity) globalThis.__wtCapacity.shuttingDown = true;
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
