#!/bin/bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
SUFFIX=$(openssl rand -hex 6)
TEMP_DIR=$(mktemp -d "/tmp/collab-phase111-${SUFFIX}.XXXXXX")
REPORT="/tmp/collab-phase111-${SUFFIX}.log"
NETWORK="collab-phase111-network-${SUFFIX}"
MYSQL_CONTAINER="collab-phase111-mysql-${SUFFIX}"
REDIS_CONTAINER="collab-phase111-redis-${SUFFIX}"
ETCD_CONTAINER="collab-phase111-etcd-${SUFFIX}"
PIDS=()
CONTAINERS=()
LAN_FIXTURE_MODE=${LAN_FIXTURE_MODE:-false}
LAN_MAC_IP=${LAN_MAC_IP:-}
LAN_WINDOWS_IP=${LAN_WINDOWS_IP:-}
OWNER_FAILOVER_DRILL=${OWNER_FAILOVER_DRILL:-false}

if [[ "$LAN_FIXTURE_MODE" != true && "$LAN_FIXTURE_MODE" != false ]]; then
  printf 'LAN_FIXTURE_MODE must be true or false\n' >&2
  exit 64
fi
if [[ "$OWNER_FAILOVER_DRILL" != true && "$OWNER_FAILOVER_DRILL" != false ]]; then
  printf 'OWNER_FAILOVER_DRILL must be true or false\n' >&2
  exit 64
fi
if [[ "$LAN_FIXTURE_MODE" == true ]]; then
  python3 - "$LAN_MAC_IP" "$LAN_WINDOWS_IP" <<'PY'
import ipaddress, sys
for value in sys.argv[1:]:
    address = ipaddress.ip_address(value)
    if not address.is_private or address.version != 4:
        raise SystemExit(f"LAN fixture requires private IPv4 addresses, got {value!r}")
PY
  if ! ifconfig | grep -Fq "inet $LAN_MAC_IP "; then
    printf 'LAN_MAC_IP is not assigned to this Mac: %s\n' "$LAN_MAC_IP" >&2
    exit 64
  fi
  PORT_BIND_ADDRESS=0.0.0.0
else
  PORT_BIND_ADDRESS=127.0.0.1
fi

log() { printf '%s\n' "$*" | tee -a "$REPORT"; }
random_port() {
  python3 - "$PORT_BIND_ADDRESS" <<'PY'
import socket, sys
bind_address = sys.argv[1]
while True:
    tcp = socket.socket()
    tcp.bind((bind_address, 0))
    port = tcp.getsockname()[1]
    udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        udp.bind((bind_address, port))
        print(port)
        break
    except OSError:
        pass
    finally:
        udp.close()
        tcp.close()
PY
}
stop_pid() {
  local pid=$1
  if [[ "$pid" =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null || true
    for _ in {1..50}; do kill -0 "$pid" 2>/dev/null || return 0; sleep 0.1; done
    kill -9 "$pid" 2>/dev/null || true
  fi
}
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  if [[ $status -ne 0 ]]; then
    for log_file in "$TEMP_DIR"/*.log; do
      [[ -f "$log_file" ]] || continue
      printf '\n===== %s (tail) =====\n' "$(basename "$log_file")" >> "$REPORT"
      tail -n 120 "$log_file" >> "$REPORT"
    done
  fi
  if [[ -f "$TEMP_DIR/restart-pids" ]]; then
    while IFS= read -r entry; do stop_pid "$entry"; done < "$TEMP_DIR/restart-pids"
  fi
  for pid in "${PIDS[@]:-}"; do stop_pid "$pid"; done
  for container in "${CONTAINERS[@]:-}"; do
    if [[ $(docker inspect -f '{{ index .Config.Labels "com.collab-editor.test-run" }}' "$container" 2>/dev/null) == "$SUFFIX" ]]; then
      docker rm -f -v "$container" >/dev/null 2>&1 || true
    fi
  done
  if [[ $(docker network inspect -f '{{ index .Labels "com.collab-editor.test-run" }}' "$NETWORK" 2>/dev/null) == "$SUFFIX" ]]; then
    docker network rm "$NETWORK" >/dev/null 2>&1 || true
  fi
  chmod -R u+rwX "$TEMP_DIR" 2>/dev/null || true
  rm -rf "$TEMP_DIR"
  if [[ $status -eq 0 ]]; then log 'CLEANUP: PASS'; else log "CLEANUP: PASS (test exit $status)"; fi
  exit "$status"
}
trap cleanup EXIT INT TERM

wait_for() {
  local description=$1
  shift
  for _ in {1..120}; do
    if "$@" >/dev/null 2>&1; then log "$description: READY"; return 0; fi
    sleep 0.5
  done
  log "$description: FAILED"
  return 1
}

write_env() {
  local file=$1
  shift
  : > "$file"
  chmod 600 "$file"
  for item in "$@"; do printf '%q\n' "$item" >> "$file"; done
}

issue_certificate() {
  local ca_cert=$1 ca_key=$2 name=$3 extension=$4 usage=$5
  openssl ecparam -name prime256v1 -genkey -noout -out "$TEMP_DIR/$name-key.pem" >/dev/null 2>&1
  openssl req -new -sha256 -key "$TEMP_DIR/$name-key.pem" -out "$TEMP_DIR/$name.csr" -subj "/CN=$name" >/dev/null 2>&1
  openssl x509 -req -sha256 -days 10 -in "$TEMP_DIR/$name.csr" -CA "$ca_cert" -CAkey "$ca_key" -CAcreateserial \
    -out "$TEMP_DIR/$name.pem" -extfile <(printf 'basicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=%s\nsubjectAltName=%s\n' "$usage" "$extension") >/dev/null 2>&1
  chmod 600 "$TEMP_DIR/$name-key.pem"
}

cd "$ROOT"
docker info >/dev/null
log "TEST RUN: $SUFFIX"
log "TEMP RESOURCES: $TEMP_DIR"

MYSQL_PORT=$(random_port)
REDIS_PORT=$(random_port)
ETCD_PORT=$(random_port)
REST_A=$(random_port); REST_B=$(random_port)
WT_A=$(random_port); WT_B=$(random_port)
QUIC_A=$(random_port); QUIC_B=$(random_port)
RPC_A=$(random_port); RPC_B=$(random_port)
ADMIN_A=$(random_port); ADMIN_B=$(random_port)
VITE_A=$(random_port); VITE_B=$(random_port)
if [[ "$LAN_FIXTURE_MODE" == true ]]; then CA_DOWNLOAD_PORT=$(random_port); fi
MYSQL_ROOT_PASSWORD=$(openssl rand -hex 24)
MYSQL_PASSWORD=$(openssl rand -hex 24)
REDIS_PASSWORD=$(openssl rand -hex 24)
JWT_SECRET=$(openssl rand -hex 32)
REPLAY_KEY=$(openssl rand -base64 32 | tr -d '\n')

openssl ecparam -name prime256v1 -genkey -noout -out "$TEMP_DIR/etcd-ca-key.pem" >/dev/null 2>&1
openssl req -new -x509 -sha256 -days 10 -key "$TEMP_DIR/etcd-ca-key.pem" -out "$TEMP_DIR/etcd-ca.pem" -subj '/CN=phase111-etcd-ca' >/dev/null 2>&1
issue_certificate "$TEMP_DIR/etcd-ca.pem" "$TEMP_DIR/etcd-ca-key.pem" etcd-server 'DNS:localhost,IP:127.0.0.1' 'serverAuth,clientAuth'
issue_certificate "$TEMP_DIR/etcd-ca.pem" "$TEMP_DIR/etcd-ca-key.pem" etcd-client 'DNS:localhost,IP:127.0.0.1' 'clientAuth'
cp "$TEMP_DIR/etcd-ca.pem" "$TEMP_DIR/ca.pem"
cp "$TEMP_DIR/etcd-client.pem" "$TEMP_DIR/client.pem"
cp "$TEMP_DIR/etcd-client-key.pem" "$TEMP_DIR/client-key.pem"

openssl ecparam -name prime256v1 -genkey -noout -out "$TEMP_DIR/internal-ca-key.pem" >/dev/null 2>&1
openssl req -new -x509 -sha256 -days 10 -key "$TEMP_DIR/internal-ca-key.pem" -out "$TEMP_DIR/internal-ca.pem" -subj '/CN=phase111-internal-ca' >/dev/null 2>&1
issue_certificate "$TEMP_DIR/internal-ca.pem" "$TEMP_DIR/internal-ca-key.pem" node-a 'URI:spiffe://collab-editor/node/node-a/incarnation/process-a,DNS:localhost,IP:127.0.0.1' 'serverAuth,clientAuth'
issue_certificate "$TEMP_DIR/internal-ca.pem" "$TEMP_DIR/internal-ca-key.pem" node-b 'URI:spiffe://collab-editor/node/node-b/incarnation/process-b,DNS:localhost,IP:127.0.0.1' 'serverAuth,clientAuth'
issue_certificate "$TEMP_DIR/internal-ca.pem" "$TEMP_DIR/internal-ca-key.pem" node-a2 'URI:spiffe://collab-editor/node/node-a/incarnation/process-a2,DNS:localhost,IP:127.0.0.1' 'serverAuth,clientAuth'
issue_certificate "$TEMP_DIR/internal-ca.pem" "$TEMP_DIR/internal-ca-key.pem" node-b2 'URI:spiffe://collab-editor/node/node-b/incarnation/process-b2,DNS:localhost,IP:127.0.0.1' 'serverAuth,clientAuth'

openssl ecparam -name prime256v1 -genkey -noout -out "$TEMP_DIR/transport-ca-key.pem" >/dev/null 2>&1
openssl req -new -x509 -sha256 -days 10 -key "$TEMP_DIR/transport-ca-key.pem" -out "$TEMP_DIR/transport-ca.pem" -subj '/CN=phase111-transport-ca' >/dev/null 2>&1
issue_certificate "$TEMP_DIR/transport-ca.pem" "$TEMP_DIR/transport-ca-key.pem" transport 'DNS:localhost,IP:127.0.0.1' 'serverAuth'

OPENSSL3=/opt/homebrew/opt/openssl@3/bin/openssl
if [[ ! -x "$OPENSSL3" ]]; then
  log 'OpenSSL 3 is required for the browser-pinned WebTransport certificate'
  exit 1
fi
if [[ "$LAN_FIXTURE_MODE" == true ]]; then
  "$OPENSSL3" req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -sha256 -nodes \
    -keyout "$TEMP_DIR/lan-root-ca-key.pem" -out "$TEMP_DIR/lan-root-ca.pem" -days 2 \
    -subj "/CN=collab-lan-test-$SUFFIX" \
    -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' \
    -addext 'keyUsage=critical,keyCertSign,cRLSign' >/dev/null 2>&1
  "$OPENSSL3" req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -sha256 -nodes \
    -keyout "$TEMP_DIR/webtransport-key.pem" -out "$TEMP_DIR/webtransport.csr" \
    -subj "/CN=$LAN_MAC_IP" >/dev/null 2>&1
  "$OPENSSL3" x509 -req -sha256 -days 1 -in "$TEMP_DIR/webtransport.csr" \
    -CA "$TEMP_DIR/lan-root-ca.pem" -CAkey "$TEMP_DIR/lan-root-ca-key.pem" -CAcreateserial \
    -out "$TEMP_DIR/webtransport.pem" -extfile <(printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName=IP:%s,DNS:localhost,IP:127.0.0.1\n' "$LAN_MAC_IP") >/dev/null 2>&1
else
  "$OPENSSL3" req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -sha256 -nodes \
    -keyout "$TEMP_DIR/webtransport-key.pem" -out "$TEMP_DIR/webtransport.pem" \
    -days 13 -subj '/CN=localhost' \
    -addext 'subjectAltName=DNS:localhost,IP:127.0.0.1,IP:::1' \
    -addext 'keyUsage=digitalSignature' \
    -addext 'extendedKeyUsage=serverAuth' >/dev/null 2>&1
fi
chmod 600 "$TEMP_DIR"/*-key.pem
WT_HASH=$("$OPENSSL3" x509 -in "$TEMP_DIR/webtransport.pem" -outform DER | "$OPENSSL3" dgst -sha256 -binary | "$OPENSSL3" base64 -A)

docker network create --label "com.collab-editor.test-run=$SUFFIX" "$NETWORK" >/dev/null
# The Go service applies and records migrations when it starts.
docker run -d --name "$MYSQL_CONTAINER" --network "$NETWORK" --label "com.collab-editor.test-run=$SUFFIX" \
  -p "127.0.0.1:$MYSQL_PORT:3306" \
  -e "MYSQL_ROOT_PASSWORD=$MYSQL_ROOT_PASSWORD" -e MYSQL_DATABASE=collab_editor -e MYSQL_USER=collab -e "MYSQL_PASSWORD=$MYSQL_PASSWORD" \
  mysql:8.4.13 >/dev/null
CONTAINERS+=("$MYSQL_CONTAINER")
docker run -d --name "$REDIS_CONTAINER" --network "$NETWORK" --label "com.collab-editor.test-run=$SUFFIX" \
  -p "127.0.0.1:$REDIS_PORT:6379" redis:7.4.5-alpine redis-server --save '' --appendonly no --requirepass "$REDIS_PASSWORD" >/dev/null
CONTAINERS+=("$REDIS_CONTAINER")
docker run -d --name "$ETCD_CONTAINER" --network "$NETWORK" --label "com.collab-editor.test-run=$SUFFIX" \
  -p "127.0.0.1:$ETCD_PORT:2379" -v "$TEMP_DIR:/certs:ro" quay.io/coreos/etcd:v3.5.17 \
  /usr/local/bin/etcd --name phase111 --data-dir=/tmp/etcd-data \
  --listen-client-urls=https://0.0.0.0:2379 --advertise-client-urls=https://127.0.0.1:2379 \
  --cert-file=/certs/etcd-server.pem --key-file=/certs/etcd-server-key.pem --trusted-ca-file=/certs/etcd-ca.pem --client-cert-auth=true >/dev/null
CONTAINERS+=("$ETCD_CONTAINER")

wait_for MYSQL docker exec "$MYSQL_CONTAINER" sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysqladmin ping -h 127.0.0.1 -u root --silent'
wait_for REDIS docker exec "$REDIS_CONTAINER" redis-cli -a "$REDIS_PASSWORD" ping
wait_for ETCD docker exec "$ETCD_CONTAINER" etcdctl --endpoints=https://127.0.0.1:2379 --cacert=/certs/ca.pem --cert=/certs/client.pem --key=/certs/client-key.pem endpoint health

go build -o "$TEMP_DIR/collab-server" ./cmd/service

COMMON=(
  "APP_ENV=development" "STORAGE_DRIVER=mysql" "MYSQL_DSN=collab:${MYSQL_PASSWORD}@tcp(127.0.0.1:${MYSQL_PORT})/collab_editor?charset=utf8mb4&parseTime=true&loc=UTC"
  "AUTH_MODE=jwt" "JWT_SECRET=$JWT_SECRET" "JWT_ISSUER=phase111" "JWT_AUDIENCE=phase111" "JWT_TTL=15m" "REFRESH_TOKEN_TTL=1h" "REFRESH_REPLAY_TTL=10m" "REFRESH_REPLAY_ENCRYPTION_KEY=$REPLAY_KEY"
  "REDIS_ADDR=127.0.0.1:${REDIS_PORT}" "REDIS_PASSWORD=$REDIS_PASSWORD" "REDIS_DB=0"
  "QUIC_TLS_CERT_FILE=$TEMP_DIR/transport.pem" "QUIC_TLS_KEY_FILE=$TEMP_DIR/transport-key.pem" "QUIC_CA_FILE=$TEMP_DIR/transport-ca.pem"
  "WEBTRANSPORT_ENABLED=true" "WEBTRANSPORT_TLS_CERT_FILE=$TEMP_DIR/webtransport.pem" "WEBTRANSPORT_TLS_KEY_FILE=$TEMP_DIR/webtransport-key.pem"
  "CLUSTER_MODE=cluster" "ETCD_ENDPOINTS=https://127.0.0.1:${ETCD_PORT}" "ETCD_CA_FILE=$TEMP_DIR/etcd-ca.pem" "ETCD_CERT_FILE=$TEMP_DIR/etcd-client.pem" "ETCD_KEY_FILE=$TEMP_DIR/etcd-client-key.pem"
  "ETCD_LEASE_TTL=3s" "ETCD_DIAL_TIMEOUT=3s" "ETCD_REQUEST_TIMEOUT=3s" "INTERNAL_RPC_CA_FILE=$TEMP_DIR/internal-ca.pem" "INTERNAL_RPC_TIMEOUT=3s"
  "MYSQL_PING_TIMEOUT=5s" "MYSQL_QUERY_TIMEOUT=5s" "LOGIN_RATE_LIMIT=1000" "REFRESH_RATE_LIMIT=1000" "INTERNAL_RPC_RATE_LIMIT=1000"
)
if [[ "$LAN_FIXTURE_MODE" == true ]]; then
  PUBLIC_HOST=$LAN_MAC_IP
  COMMON+=(
    "CORS_ALLOWED_ORIGINS=https://${LAN_MAC_IP}:${VITE_A},https://${LAN_MAC_IP}:${VITE_B}"
    "REST_TLS_CERT_FILE=$TEMP_DIR/webtransport.pem"
    "REST_TLS_KEY_FILE=$TEMP_DIR/webtransport-key.pem"
  )
else
  PUBLIC_HOST=127.0.0.1
  COMMON+=("CORS_ALLOWED_ORIGINS=http://127.0.0.1:${VITE_A},http://127.0.0.1:${VITE_B}")
fi
write_env "$TEMP_DIR/node-a.env" "${COMMON[@]}" "PORT=$REST_A" "WEBTRANSPORT_ADDR=$PUBLIC_HOST:$WT_A" "QUIC_ADDR=127.0.0.1:$QUIC_A" "ADMIN_ADDR=127.0.0.1:$ADMIN_A" "NODE_ID=node-a" "NODE_INCARNATION=process-a" "NODE_QUIC_ENDPOINT=127.0.0.1:$QUIC_A" "NODE_HTTP_ENDPOINT=https://127.0.0.1:$RPC_A" "NODE_BROWSER_ENDPOINT=https://$PUBLIC_HOST:$WT_A" "INTERNAL_RPC_ADDR=127.0.0.1:$RPC_A" "INTERNAL_RPC_CERT_FILE=$TEMP_DIR/node-a.pem" "INTERNAL_RPC_KEY_FILE=$TEMP_DIR/node-a-key.pem"
write_env "$TEMP_DIR/node-b.env" "${COMMON[@]}" "PORT=$REST_B" "WEBTRANSPORT_ADDR=$PUBLIC_HOST:$WT_B" "QUIC_ADDR=127.0.0.1:$QUIC_B" "ADMIN_ADDR=127.0.0.1:$ADMIN_B" "NODE_ID=node-b" "NODE_INCARNATION=process-b" "NODE_QUIC_ENDPOINT=127.0.0.1:$QUIC_B" "NODE_HTTP_ENDPOINT=https://127.0.0.1:$RPC_B" "NODE_BROWSER_ENDPOINT=https://$PUBLIC_HOST:$WT_B" "INTERNAL_RPC_ADDR=127.0.0.1:$RPC_B" "INTERNAL_RPC_CERT_FILE=$TEMP_DIR/node-b.pem" "INTERNAL_RPC_KEY_FILE=$TEMP_DIR/node-b-key.pem"
write_env "$TEMP_DIR/node-a2.env" "${COMMON[@]}" "PORT=$REST_A" "WEBTRANSPORT_ADDR=$PUBLIC_HOST:$WT_A" "QUIC_ADDR=127.0.0.1:$QUIC_A" "ADMIN_ADDR=127.0.0.1:$ADMIN_A" "NODE_ID=node-a" "NODE_INCARNATION=process-a2" "NODE_QUIC_ENDPOINT=127.0.0.1:$QUIC_A" "NODE_HTTP_ENDPOINT=https://127.0.0.1:$RPC_A" "NODE_BROWSER_ENDPOINT=https://$PUBLIC_HOST:$WT_A" "INTERNAL_RPC_ADDR=127.0.0.1:$RPC_A" "INTERNAL_RPC_CERT_FILE=$TEMP_DIR/node-a2.pem" "INTERNAL_RPC_KEY_FILE=$TEMP_DIR/node-a2-key.pem"
write_env "$TEMP_DIR/node-b2.env" "${COMMON[@]}" "PORT=$REST_B" "WEBTRANSPORT_ADDR=$PUBLIC_HOST:$WT_B" "QUIC_ADDR=127.0.0.1:$QUIC_B" "ADMIN_ADDR=127.0.0.1:$ADMIN_B" "NODE_ID=node-b" "NODE_INCARNATION=process-b2" "NODE_QUIC_ENDPOINT=127.0.0.1:$QUIC_B" "NODE_HTTP_ENDPOINT=https://127.0.0.1:$RPC_B" "NODE_BROWSER_ENDPOINT=https://$PUBLIC_HOST:$WT_B" "INTERNAL_RPC_ADDR=127.0.0.1:$RPC_B" "INTERNAL_RPC_CERT_FILE=$TEMP_DIR/node-b2.pem" "INTERNAL_RPC_KEY_FILE=$TEMP_DIR/node-b2-key.pem"

start_server() {
  local env_file=$1 log_file=$2
  /bin/bash --noprofile --norc -c 'set -a; source "$1"; set +a; exec "$2"' _ "$env_file" "$TEMP_DIR/collab-server" >"$log_file" 2>&1 &
  PIDS+=("$!")
}
start_server "$TEMP_DIR/node-a.env" "$TEMP_DIR/node-a.log"
NODE_A_PID=$!
start_server "$TEMP_DIR/node-b.env" "$TEMP_DIR/node-b.log"
NODE_B_PID=$!
wait_for 'NODE A' curl -fsS "http://127.0.0.1:$ADMIN_A/readyz"
wait_for 'NODE B' curl -fsS "http://127.0.0.1:$ADMIN_B/readyz"

if [[ "$LAN_FIXTURE_MODE" == true ]]; then
  VITE_SCHEME=https
  REST_SCHEME=https
else
  VITE_SCHEME=http
  REST_SCHEME=http
fi
start_vite() {
  local rest_port=$1 primary_wt=$2 secondary_wt=$3 vite_port=$4 log_file=$5
  if [[ "$LAN_FIXTURE_MODE" == true ]]; then
    (cd frontend && exec env VITE_HTTPS_CERT_FILE="$TEMP_DIR/webtransport.pem" VITE_HTTPS_KEY_FILE="$TEMP_DIR/webtransport-key.pem" VITE_BACKEND_URL="$REST_SCHEME://$PUBLIC_HOST:$rest_port" VITE_REALTIME_TRANSPORT=webtransport VITE_WEBTRANSPORT_URLS="https://$PUBLIC_HOST:$primary_wt/api/webtransport,https://$PUBLIC_HOST:$secondary_wt/api/webtransport" VITE_WEBTRANSPORT_CERT_SHA256="$WT_HASH" ./node_modules/.bin/vite --host "$PUBLIC_HOST" --port "$vite_port") >"$log_file" 2>&1 &
  else
    (cd frontend && exec env VITE_BACKEND_URL="$REST_SCHEME://$PUBLIC_HOST:$rest_port" VITE_REALTIME_TRANSPORT=webtransport VITE_WEBTRANSPORT_URLS="https://$PUBLIC_HOST:$primary_wt/api/webtransport,https://$PUBLIC_HOST:$secondary_wt/api/webtransport" VITE_WEBTRANSPORT_CERT_SHA256="$WT_HASH" ./node_modules/.bin/vite --host "$PUBLIC_HOST" --port "$vite_port") >"$log_file" 2>&1 &
  fi
  PIDS+=("$!")
}
start_vite "$REST_A" "$WT_A" "$WT_B" "$VITE_A" "$TEMP_DIR/vite-a.log"
start_vite "$REST_B" "$WT_B" "$WT_A" "$VITE_B" "$TEMP_DIR/vite-b.log"
if [[ "$LAN_FIXTURE_MODE" == true ]]; then
  wait_for 'VITE A' curl --cacert "$TEMP_DIR/lan-root-ca.pem" -fsS "https://$PUBLIC_HOST:$VITE_A/e2e/webtransport-capacity.html"
  wait_for 'VITE B' curl --cacert "$TEMP_DIR/lan-root-ca.pem" -fsS "https://$PUBLIC_HOST:$VITE_B/e2e/webtransport-capacity.html"
else
  wait_for 'VITE A' curl -fsS "http://127.0.0.1:$VITE_A/e2e/webtransport-capacity.html"
  wait_for 'VITE B' curl -fsS "http://127.0.0.1:$VITE_B/e2e/webtransport-capacity.html"
fi

MEMBERS=$(docker exec "$ETCD_CONTAINER" etcdctl --endpoints=https://127.0.0.1:2379 --cacert=/certs/ca.pem --cert=/certs/client.pem --key=/certs/client-key.pem get /collab/v1/nodes/ --prefix --keys-only | grep -c '/collab/v1/nodes/' || true)
[[ "$MEMBERS" -eq 2 ]] || { log "ETCD MEMBERSHIP: expected 2, got $MEMBERS"; exit 1; }
log 'ETCD MEMBERSHIP: 2'

if [[ "$LAN_FIXTURE_MODE" == true ]]; then
  OWNER_FAILOVER_CONTROL_PORT=$(random_port)
  OWNER_FAILOVER_TOKEN=$(openssl rand -hex 24)
  WINDOWS_ENV_FILE="$TEMP_DIR/windows-env.ps1"
  cat > "$WINDOWS_ENV_FILE" <<EOF
\$env:REST_A_URL='https://${LAN_MAC_IP}:${REST_A}'
\$env:REST_B_URL='https://${LAN_MAC_IP}:${REST_B}'
\$env:WT_A_URL='https://${LAN_MAC_IP}:${WT_A}/api/webtransport'
\$env:WT_B_URL='https://${LAN_MAC_IP}:${WT_B}/api/webtransport'
\$env:CAPACITY_PAGE_URL='https://${LAN_MAC_IP}:${VITE_A}/e2e/webtransport-capacity.html'
\$env:WT_HASH='${WT_HASH}'
\$env:FAILOVER_CONTROL_URL='http://${LAN_MAC_IP}:${OWNER_FAILOVER_CONTROL_PORT}'
\$env:FAILOVER_CONTROL_TOKEN='${OWNER_FAILOVER_TOKEN}'
EOF
  chmod 600 "$WINDOWS_ENV_FILE"
  CA_SHA256=$(shasum -a 256 "$TEMP_DIR/lan-root-ca.pem" | awk '{print toupper($1)}')
  CA_SHA1=$("$OPENSSL3" x509 -in "$TEMP_DIR/lan-root-ca.pem" -noout -fingerprint -sha1 | cut -d= -f2 | tr -d ':')
  python3 -u - "$LAN_MAC_IP" "$CA_DOWNLOAD_PORT" "$LAN_WINDOWS_IP" "$TEMP_DIR/lan-root-ca.pem" "$WINDOWS_ENV_FILE" >"$TEMP_DIR/lan-download.log" 2>&1 <<'PY' &
import http.server
import pathlib
import sys

host, port, allowed_ip, ca_path, env_path = sys.argv[1:]
files = {
    "/rootCA.pem": (pathlib.Path(ca_path), "application/x-pem-file"),
    "/windows-env.ps1": (pathlib.Path(env_path), "text/plain; charset=utf-8"),
}

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.client_address[0] != allowed_ip:
            self.send_error(403)
            return
        item = files.get(self.path)
        if item is None:
            self.send_error(404)
            return
        path, content_type = item
        payload = path.read_bytes()
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, format, *args):
        print(f"{self.client_address[0]} {format % args}", flush=True)

server = http.server.ThreadingHTTPServer((host, int(port)), Handler)
server.serve_forever()
PY
  PIDS+=("$!")
  sleep 0.2
  kill -0 "$!" 2>/dev/null || { log 'LAN DOWNLOAD SERVER: FAILED'; exit 1; }

  python3 -u "$ROOT/scripts/owner-failover-control.py" \
    --bind "$LAN_MAC_IP" \
    --port "$OWNER_FAILOVER_CONTROL_PORT" \
    --allowed-ip "$LAN_WINDOWS_IP" \
    --token "$OWNER_FAILOVER_TOKEN" \
    --owner-pid "$NODE_A_PID" >"$TEMP_DIR/owner-failover-control.log" 2>&1 &
  PIDS+=("$!")
  sleep 0.2
  kill -0 "$!" 2>/dev/null || { log 'OWNER FAILOVER CONTROL: FAILED'; exit 1; }

  log 'LAN FIXTURE: READY'
  log "ROOT_CA_URL=http://${LAN_MAC_IP}:${CA_DOWNLOAD_PORT}/rootCA.pem"
  log "WINDOWS_ENV_URL=http://${LAN_MAC_IP}:${CA_DOWNLOAD_PORT}/windows-env.ps1"
  log "ROOT_CA_SHA256=$CA_SHA256"
  log "ROOT_CA_SHA1=$CA_SHA1"
  log "CAPACITY_PAGE_URL=https://${LAN_MAC_IP}:${VITE_A}/e2e/webtransport-capacity.html"
  log "OWNER_FAILOVER_READY_URL=http://${LAN_MAC_IP}:${OWNER_FAILOVER_CONTROL_PORT}/ready"
  log "OWNER_FAILOVER_STATE_URL=http://${LAN_MAC_IP}:${OWNER_FAILOVER_CONTROL_PORT}/state"
  log "OWNER_FAILOVER_TRIGGER_URL=http://${LAN_MAC_IP}:${OWNER_FAILOVER_CONTROL_PORT}/trigger"
  log 'OWNER_FAILOVER_TOKEN=REDACTED (download from WINDOWS_ENV_URL)'
  log "OWNER_FAILOVER_OWNER_PID=$NODE_A_PID"
  log "WINDOWS_LOAD_HOST=$LAN_WINDOWS_IP"
  log 'Press Ctrl+C on the Mac after the Windows run finishes.'
  while :; do
    for pid in "${PIDS[@]}"; do
      if [[ "$OWNER_FAILOVER_DRILL" == true && "$pid" == "$NODE_A_PID" ]]; then
        continue
      fi
      kill -0 "$pid" 2>/dev/null || { log "LAN FIXTURE PROCESS EXITED: $pid"; exit 1; }
    done
    sleep 2
  done
fi

export E2E_NODE_A_URL="http://127.0.0.1:$VITE_A" E2E_NODE_B_URL="http://127.0.0.1:$VITE_B"
export E2E_NODE_A_PID="$NODE_A_PID" E2E_NODE_B_PID="$NODE_B_PID"
export E2E_NODE_A_RESTART_ENV="$TEMP_DIR/node-a2.env" E2E_NODE_B_RESTART_ENV="$TEMP_DIR/node-b2.env"
export E2E_SERVER_BINARY="$TEMP_DIR/collab-server" E2E_PROJECT_ROOT="$ROOT" E2E_ETCD_CONTAINER="$ETCD_CONTAINER" E2E_RESTART_PID_FILE="$TEMP_DIR/restart-pids"
export E2E_BROWSER_CHANNEL=chrome

if [[ -n ${WEBTRANSPORT_FAILOVER_MODE:-} && "$LAN_FIXTURE_MODE" != true ]]; then
  LOCAL_CONTROL_PORT=$(random_port)
  LOCAL_CONTROL_TOKEN=$(openssl rand -hex 24)
  python3 -u "$ROOT/scripts/owner-failover-control.py" \
    --bind 127.0.0.1 \
    --port "$LOCAL_CONTROL_PORT" \
    --allowed-ip 127.0.0.1 \
    --token "$LOCAL_CONTROL_TOKEN" \
    --owner-pid "$NODE_A_PID" >"$TEMP_DIR/owner-failover-control.log" 2>&1 &
  PIDS+=("$!")
  sleep 0.2
  kill -0 "$!" 2>/dev/null || { log 'LOCAL OWNER FAILOVER CONTROL: FAILED'; exit 1; }
  export FAILOVER_CONTROL_URL="http://127.0.0.1:$LOCAL_CONTROL_PORT"
  export FAILOVER_CONTROL_TOKEN="$LOCAL_CONTROL_TOKEN"
fi

if [[ "${FIXTURE_ONLY:-false}" == true ]]; then
  log 'LOCAL FIXTURE: READY'
  log "REST_A_URL=$REST_SCHEME://127.0.0.1:$REST_A"
  log "REST_B_URL=$REST_SCHEME://127.0.0.1:$REST_B"
  log "WT_A_URL=https://127.0.0.1:$WT_A/api/webtransport"
  log "WT_B_URL=https://127.0.0.1:$WT_B/api/webtransport"
  log "CAPACITY_PAGE_URL=$VITE_SCHEME://127.0.0.1:$VITE_A/e2e/webtransport-capacity.html"
  log "WT_CA_FILE=$TEMP_DIR/webtransport.pem"
  log "WT_ORIGIN=$VITE_SCHEME://127.0.0.1:$VITE_A"
  log "NODE_A_PID=$NODE_A_PID"
  log "NODE_B_PID=$NODE_B_PID"
  log 'Press Ctrl+C after the external load client finishes.'
  while :; do
    for pid in "${PIDS[@]}"; do
      if [[ "${OWNER_FAILOVER_DRILL:-false}" == true && "$pid" == "$NODE_A_PID" ]]; then
        continue
      fi
      kill -0 "$pid" 2>/dev/null || { log "LOCAL FIXTURE PROCESS EXITED: $pid"; exit 1; }
    done
    sleep 2
  done
fi

if [[ -n ${WEBTRANSPORT_LOAD_USERS:-} ]]; then
  log "REAL CHROME WEBTRANSPORT CAPACITY ($WEBTRANSPORT_LOAD_USERS users): START"
  set +e
  (cd frontend && env REST_A="$REST_A" REST_B="$REST_B" WT_A="$WT_A" WT_B="$WT_B" VITE_A="$VITE_A" WT_HASH="$WT_HASH" \
    WEBTRANSPORT_LOAD_USERS="$WEBTRANSPORT_LOAD_USERS" \
    WEBTRANSPORT_CONNECT_CONCURRENCY="${WEBTRANSPORT_CONNECT_CONCURRENCY:-12}" \
    WEBTRANSPORT_CONNECT_INTERVAL_MS="${WEBTRANSPORT_CONNECT_INTERVAL_MS:-0}" \
    WEBTRANSPORT_FAILOVER_MODE="${WEBTRANSPORT_FAILOVER_MODE:-false}" \
    FAILOVER_TRIGGER_MODE="${FAILOVER_TRIGGER_MODE:-harness}" \
    FAILOVER_CONTROL_URL="${FAILOVER_CONTROL_URL:-}" \
    FAILOVER_CONTROL_TOKEN="${FAILOVER_CONTROL_TOKEN:-}" \
    node "$ROOT/scripts/webtransport-capacity.mjs") 2>&1 | tee -a "$REPORT"
  CAPACITY_STATUS=${PIPESTATUS[0]}
  set -e
  if [[ "$CAPACITY_STATUS" -ne 0 ]]; then
    log "REAL CHROME WEBTRANSPORT CAPACITY: FAIL (exit $CAPACITY_STATUS)"
    exit "$CAPACITY_STATUS"
  fi
  log 'REAL CHROME WEBTRANSPORT CAPACITY: PASS'
  exit 0
fi

log 'REAL CHROME DISTRIBUTED WEBTRANSPORT: START'
(cd frontend && npx playwright test e2e/distributed-webtransport.spec.ts --reporter=line) 2>&1 | tee -a "$REPORT"
grep -q '"event":"owner_redirect"' "$TEMP_DIR/node-a.log" "$TEMP_DIR/node-b.log" || { log 'OWNER REDIRECT LOG: MISSING'; exit 1; }
log 'OWNER REDIRECT LOG: PASS'
log 'REAL CHROME DISTRIBUTED WEBTRANSPORT: PASS'
