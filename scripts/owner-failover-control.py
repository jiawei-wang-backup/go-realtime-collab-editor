#!/usr/bin/env python3
"""Small, single-use control plane for the LAN Owner failover drill."""
from __future__ import annotations

import argparse
import http.server
import json
import os
import signal
import threading
import time


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument('--bind', required=True)
    parser.add_argument('--port', type=int, required=True)
    parser.add_argument('--allowed-ip', required=True)
    parser.add_argument('--token', required=True)
    parser.add_argument('--owner-pid', type=int, required=True)
    args = parser.parse_args()

    state = {
        'ready': False,
        'triggered': False,
        'triggered_at': None,
        'owner_pid': args.owner_pid,
    }
    lock = threading.Lock()

    def authorized(handler: http.server.BaseHTTPRequestHandler) -> bool:
        return handler.client_address[0] == args.allowed_ip and handler.headers.get('X-Failover-Token') == args.token

    class Handler(http.server.BaseHTTPRequestHandler):
        def send_json(self, status: int, payload: dict) -> None:
            body = json.dumps(payload, separators=(',', ':')).encode()
            self.send_response(status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Cache-Control', 'no-store')
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self) -> None:
            if not authorized(self):
                self.send_json(403, {'error': 'forbidden'})
                return
            if self.path not in ('/ready', '/state'):
                self.send_json(404, {'error': 'not_found'})
                return
            with lock:
                self.send_json(200, dict(state))

        def do_POST(self) -> None:
            if not authorized(self):
                self.send_json(403, {'error': 'forbidden'})
                return
            if self.path == '/ready':
                with lock:
                    if state['ready']:
                        self.send_json(409, {'error': 'already_ready'})
                        return
                    state['ready'] = True
                    self.send_json(200, dict(state))
                return
            if self.path == '/trigger':
                with lock:
                    if not state['ready']:
                        self.send_json(409, {'error': 'not_ready'})
                        return
                    if state['triggered']:
                        self.send_json(409, {'error': 'already_triggered'})
                        return
                    state['triggered'] = True
                    state['triggered_at'] = time.time()
                try:
                    os.kill(args.owner_pid, signal.SIGKILL)
                except ProcessLookupError:
                    self.send_json(409, {'error': 'owner_not_running'})
                    return
                self.send_json(200, dict(state))
                return
            self.send_json(404, {'error': 'not_found'})

        def log_message(self, fmt: str, *values: object) -> None:
            print(f'{self.client_address[0]} {fmt % values}', flush=True)

    server = http.server.ThreadingHTTPServer((args.bind, args.port), Handler)
    print(json.dumps({'ready_url': f'http://{args.bind}:{args.port}/ready', 'state_url': f'http://{args.bind}:{args.port}/state', 'trigger_url': f'http://{args.bind}:{args.port}/trigger'}), flush=True)
    try:
        server.serve_forever()
    finally:
        server.server_close()


if __name__ == '__main__':
    main()
