#!/bin/bash
set -euo pipefail
umask 077

usage() {
  printf 'Usage: bash scripts/run-webtransport-benchmark.sh {preflight|smoke|50|256|512|1000|2000}\n'
  printf 'Run one isolated local WebTransport check and keep logs outside Git.\n'
}

case "${1:-}" in
  -h|--help) usage; exit 0 ;;
  preflight|smoke|50|256|512|1000|2000) stage=$1 ;;
  *) usage >&2; exit 64 ;;
esac
[[ $# -eq 1 ]] || { usage >&2; exit 64; }

fail() { printf 'PREFLIGHT BLOCKED: %s\n' "$*" >&2; exit 1; }
cd "$(cd "$(dirname "$0")/.." && pwd)"

# 不继承应用、数据库、浏览器地址等环境变量；仅使用本轮隔离环境。
local_env=(env -i "PATH=$PATH" "HOME=$HOME" "TMPDIR=${TMPDIR:-/tmp}")
for tool in git go node npm docker openssl python3 curl tee; do
  command -v "$tool" >/dev/null || fail "missing command: $tool"
done
[[ -x /opt/homebrew/opt/openssl@3/bin/openssl ]] || fail 'Homebrew OpenSSL 3 is missing at the path required by the current fixture'
chrome_app='/Applications/Google Chrome.app'
if [[ ! -d "$chrome_app" ]]; then chrome_app="$HOME/Applications/Google Chrome.app"; fi
[[ -d "$chrome_app" ]] || fail 'Google Chrome is missing'
[[ -x frontend/node_modules/.bin/vite && -x frontend/node_modules/.bin/playwright ]] || fail 'frontend dependencies missing (cd frontend && npm ci)'
node -e 'const [major, minor] = process.versions.node.split(".").map(Number); process.exit(major > 24 || (major === 24 && minor >= 15) ? 0 : 1)' || fail 'Node.js >= 24.15.0 is required'

for path in "$PWD" /tmp "${TMPDIR:-/tmp}"; do
  free_kib=$(df -Pk "$path" | awk 'END { print $4 }')
  [[ "$free_kib" =~ ^[0-9]+$ && "$free_kib" -ge 8388608 ]] || fail "need at least 8 GiB free at $path"
done

endpoint=$("${local_env[@]}" docker context inspect --format '{{.Endpoints.docker.Host}}') || fail 'cannot inspect Docker context'
[[ "$endpoint" == unix://* ]] || fail 'Docker context must use a local Unix socket'
"${local_env[@]}" docker info >/dev/null || fail 'Docker is not running'
for image in mysql:8.4.13 redis:7.4.5-alpine quay.io/coreos/etcd:v3.5.17; do
  "${local_env[@]}" docker image inspect "$image" >/dev/null 2>&1 || fail "local image missing: $image"
done

# 固定已提交版本。允许本手册和封装脚本尚未提交，但不可带业务改动跑基线。
changed=$(git status --porcelain --untracked-files=all -- . \
  ':(exclude)docs/operations/webtransport-capacity-runbook.md' \
  ':(exclude)scripts/run-webtransport-benchmark.sh')
[[ -z "$changed" ]] || fail 'tracked or untracked project changes outside the benchmark runbook and wrapper'

printf 'PREFLIGHT: PASS\nCommit: %s\nStage: %s\n' "$(git rev-parse HEAD)" "$stage"
if [[ "$stage" == preflight ]]; then exit 0; fi

run_dir=$(mktemp -d "${TMPDIR:-/tmp}/collab-webtransport-run.XXXXXX")
printf 'Evidence directory: %s\n' "$run_dir"
{
  date -u '+Started (UTC): %Y-%m-%dT%H:%M:%SZ'
  git rev-parse HEAD
  uname -sm
  "${local_env[@]}" go version
  node --version
  printf 'Chrome: '
  /usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$chrome_app/Contents/Info.plist"
  "${local_env[@]}" docker version --format 'Docker engine: {{.Server.Version}}'
  df -h "$PWD" /tmp "${TMPDIR:-/tmp}"
} >"$run_dir/environment.txt"

if [[ "$stage" != smoke ]]; then local_env+=("WEBTRANSPORT_LOAD_USERS=$stage"); fi
if "${local_env[@]}" /bin/bash --noprofile --norc scripts/test-phase-11-distributed.sh 2>&1 | tee "$run_dir/test.log"; then
  pipeline=("${PIPESTATUS[@]}")
else
  pipeline=("${PIPESTATUS[@]}")
fi
status=${pipeline[0]}
if [[ "$status" -eq 0 ]]; then status=${pipeline[1]}; fi
{
  printf 'Stage: %s\nRunner exit: %s\nLog writer exit: %s\n' "$stage" "${pipeline[0]}" "${pipeline[1]}"
  date -u '+Finished (UTC): %Y-%m-%dT%H:%M:%SZ'
} | tee "$run_dir/result.txt"

run_id=$(awk '/^TEST RUN: [a-f0-9]+$/ { print $3; exit }' "$run_dir/test.log")
if [[ "$run_id" =~ ^[a-f0-9]{12}$ ]]; then
  printf 'Fixture report: /tmp/collab-phase111-%s.log\n' "$run_id"
  printf 'Remaining test containers and networks (expected empty):\n'
  if left=$({ "${local_env[@]}" docker ps -aq --filter "label=com.collab-editor.test-run=$run_id" &&
    "${local_env[@]}" docker network ls -q --filter "label=com.collab-editor.test-run=$run_id"; }); then
    printf '%s\n' "$left"
    if [[ -n "$left" && "$status" -eq 0 ]]; then status=2; fi
  else
    if [[ "$status" -eq 0 ]]; then status=2; fi
  fi
fi
printf 'Wrapper exit: %s\n' "$status" | tee -a "$run_dir/result.txt"
exit "$status"
