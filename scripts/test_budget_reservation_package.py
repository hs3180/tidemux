#!/usr/bin/env python3
"""Verify packaged budget reservation behavior with local-only mocks.

Pass the extracted candidate binary with --binary. The test uses a temporary
config/ledger, a fake `security` command, and a loopback provider; it never reads
the user's Keychain or contacts a real provider.
"""
import argparse
import json
import os
from pathlib import Path
import socket
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.error import HTTPError
from urllib.request import Request, urlopen

GATEWAY_KEY = "local-gateway-key"
PROVIDER_KEY = "local-provider-key"
REQUEST_SENTINELS = (
    "package_pre_dispatch_request_sentinel",
    "package_followup_request_sentinel",
)
RESPONSE_SENTINEL = "package_followup_response_sentinel"


class Server(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True


class UpstreamHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def do_GET(self):
        body = {"object": "list", "data": [{"id": "custom-model", "object": "model"}]}
        self._write(200, body)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length)
        self.server.posts.append({
            "path": self.path,
            "body": json.loads(raw),
            "fake_auth": self.headers.get("Authorization") == "Bearer " + PROVIDER_KEY,
        })
        body = {
            "id": "chatcmpl_local",
            "object": "chat.completion",
            "created": 1,
            "model": "custom-model",
            "choices": [{"index": 0, "message": {"role": "assistant", "content": RESPONSE_SENTINEL}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 1, "completion_tokens": 1},
        }
        self._write(200, body)

    def _write(self, status, value):
        payload = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def start_server(handler, **attributes):
    server = Server(("127.0.0.1", 0), handler)
    for name, value in attributes.items():
        setattr(server, name, value)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def post(url, body, anthropic=False):
    headers = {"Content-Type": "application/json"}
    if anthropic:
        headers.update({"x-api-key": GATEWAY_KEY, "anthropic-version": "2023-06-01"})
        path = "/v1/messages"
    else:
        headers["Authorization"] = "Bearer " + GATEWAY_KEY
        path = "/v1/chat/completions"
    request = Request(url + path, data=json.dumps(body).encode(), headers=headers, method="POST")
    try:
        with urlopen(request, timeout=20) as response:
            return response.status, response.read().decode()
    except HTTPError as error:
        return error.code, error.read().decode()


def budget_rows(path):
    with sqlite3.connect(path, timeout=5) as connection:
        connection.execute("PRAGMA busy_timeout=5000")
        return connection.execute(
            "SELECT state, charged_amount FROM budget_charges ORDER BY charged_at_ms, request_id"
        ).fetchall()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path, help="extracted TideMux candidate binary")
    args = parser.parse_args()
    if sys.platform != "darwin":
        raise SystemExit("This acceptance check requires the packaged macOS binary.")
    binary = args.binary.resolve()
    if not binary.is_file() or not os.access(binary, os.X_OK):
        raise SystemExit("--binary must point to an executable packaged candidate")

    upstream = start_server(UpstreamHandler, posts=[])
    try:
        with tempfile.TemporaryDirectory(prefix="tidemux-budget-package.") as temporary:
            root = Path(temporary)
            fake_bin = root / "bin"
            fake_bin.mkdir()
            security = fake_bin / "security"
            security.write_text(
                "#!/bin/sh\ncase \"$*\" in "
                "*test.gateway*) printf 'local-gateway-key\\n';; "
                "*test.provider*) printf 'local-provider-key\\n';; "
                "*) exit 1;; esac\n",
                encoding="utf-8",
            )
            security.chmod(0o755)
            home = root / "home"
            home.mkdir()
            port = free_port()
            ledger = root / "ledger.db"
            config = {
                "listen_addr": f"127.0.0.1:{port}",
                "max_in_flight": 2,
                "ledger_path": str(ledger),
                "access_token_keychain": {"service": "test.gateway", "account": "local"},
                "providers": {"mock": {
                    "protocol": "openai",
                    "base_url": f"http://127.0.0.1:{upstream.server_port}/v1",
                    "upstream_id": "mock",
                    "upstream_keychain": {"service": "test.provider", "account": "local"},
                    "supported_models": ["custom-model"],
                    "prices": {"custom-model": {
                        "currency": "USD", "source": "local-mock", "version": "1",
                        "input_cache_hit_per_million": 0,
                        "input_cache_miss_per_million": 1000,
                        "output_per_million": 1000,
                    }},
                    "budget": {
                        "currency": "USD", "five_hour_limit": 10,
                        "weekly_limit": 20, "alert_threshold": 0.8, "mode": "hard",
                    },
                }},
            }
            config_path = root / "config.json"
            config_path.write_text(json.dumps(config), encoding="utf-8")
            env = {
                "PATH": str(fake_bin) + ":/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin",
                "HOME": str(home),
                "TMPDIR": str(root),
                "LANG": "C.UTF-8",
            }
            gateway = subprocess.Popen(
                [str(binary), "serve", "--config", str(config_path)],
                cwd=root, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            )
            try:
                ready = False
                for _ in range(100):
                    if gateway.poll() is not None:
                        break
                    try:
                        with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                            ready = True
                            break
                    except OSError:
                        time.sleep(0.05)
                if not ready:
                    raise RuntimeError("packaged TideMux did not start with isolated fake credentials")

                rejected = {
                    "model": "mock/custom-model",
                    "max_tokens": 64,
                    "messages": [
                        {"role": "user", "content": "Earlier context."},
                        {"role": "assistant", "content": "Earlier answer."},
                        {"role": "user", "content": [{"type": "document", "source": {
                            "type": "text", "media_type": "text/plain", "data": REQUEST_SENTINELS[0],
                        }}]},
                    ],
                }
                status, body = post(f"http://127.0.0.1:{port}", rejected, anthropic=True)
                if status != 400 or "unsupported_request_feature" not in body or upstream.posts:
                    raise RuntimeError(f"pre-dispatch conversion check failed: HTTP {status}, upstream POSTs={len(upstream.posts)}")
                if budget_rows(ledger):
                    raise RuntimeError("pre-dispatch rejection left a pending/unknown budget charge")

                valid = {
                    "model": "mock/custom-model", "max_tokens": 16,
                    "messages": [{"role": "user", "content": REQUEST_SENTINELS[1]}],
                }
                status, body = post(f"http://127.0.0.1:{port}", valid, anthropic=True)
                rows = budget_rows(ledger)
                if status != 200 or RESPONSE_SENTINEL not in body or len(upstream.posts) != 1:
                    raise RuntimeError(f"valid follow-up failed: HTTP {status}, upstream POSTs={len(upstream.posts)}")
                if len(rows) != 1 or rows[0][0] != "settled" or rows[0][1] <= 0:
                    raise RuntimeError(f"valid request budget charge not settled: {rows}")

                with sqlite3.connect(ledger, timeout=5) as connection:
                    connection.execute("PRAGMA busy_timeout=5000")
                    connection.execute("CREATE TRIGGER fail_budget_settlement BEFORE UPDATE ON budget_charges BEGIN SELECT RAISE(FAIL, 'injected settlement failure'); END")
                    connection.commit()
                status, body = post(f"http://127.0.0.1:{port}", valid, anthropic=True)
                if status != 200 or len(upstream.posts) != 2:
                    raise RuntimeError(f"injected settlement request did not reach the mock: HTTP {status}")
                status, body = post(f"http://127.0.0.1:{port}", valid, anthropic=True)
                if status != 429 or "budget_usage_unknown" not in body or len(upstream.posts) != 2:
                    raise RuntimeError(f"failed settlement did not remain fail-closed: HTTP {status}, upstream POSTs={len(upstream.posts)}")
                if not all(item["fake_auth"] and item["path"] == "/v1/chat/completions" for item in upstream.posts):
                    raise RuntimeError("upstream did not see only authenticated local mock requests")
                for ledger_file in root.glob("ledger.db*"):
                    if not ledger_file.is_file():
                        continue
                    stored = ledger_file.read_bytes()
                    for sentinel in (*REQUEST_SENTINELS, RESPONSE_SENTINEL):
                        if sentinel.encode() in stored:
                            raise RuntimeError(f"request/response body sentinel was persisted in {ledger_file.name}")
                print(json.dumps({
                    "binary": str(binary),
                    "pre_dispatch_rejection_status": 400,
                    "pre_dispatch_upstream_posts": 0,
                    "pre_dispatch_budget_rows": 0,
                    "valid_followup_status": 200,
                    "settled_charge_state": rows[0][0],
                    "settlement_failure_followup_status": 429,
                    "settlement_failure_followup_code": "budget_usage_unknown",
                    "upstream_posts_after_block": len(upstream.posts),
                    "fake_credentials_only": True,
                }, ensure_ascii=False))
            finally:
                gateway.terminate()
                try:
                    gateway.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    gateway.kill()
                    gateway.wait(timeout=5)
    finally:
        upstream.shutdown()
        upstream.server_close()


if __name__ == "__main__":
    main()
