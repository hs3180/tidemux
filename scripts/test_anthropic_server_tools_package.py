#!/usr/bin/env python3
"""Verify Anthropic provider-hosted tools with an extracted candidate binary.

The check uses fake Keychain credentials and loopback protocol mocks only. It
asserts native forwarding, shared-model protocol eligibility, cross-protocol
errors, and the absence of tool data in ordinary logs and the local ledger.
"""
import argparse
import json
import os
from pathlib import Path
import signal
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
EXTENSION_SENTINEL = "server_tool_extension_must_not_be_logged"


class Server(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True


class UpstreamHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def do_GET(self):
        if self.path.startswith("/anthropic/"):
            value = {"data": [{"id": "shared-model", "type": "model"}], "has_more": False}
        else:
            value = {"object": "list", "data": [{"id": "shared-model", "object": "model"}]}
        self._write(200, value)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        payload = json.loads(self.rfile.read(length))
        with self.server.posts_lock:
            self.server.posts.append({
                "path": self.path,
                "headers": {name.lower(): value for name, value in self.headers.items()},
                "body": payload,
            })
        if self.path.endswith("/messages"):
            value = {
                "id": "msg_package_local", "type": "message", "role": "assistant",
                "model": "shared-model", "content": [{"type": "text", "text": "tool forwarded"}],
                "stop_reason": "end_turn", "usage": {"input_tokens": 2, "output_tokens": 2},
            }
            self._write(200, value)
            return
        self._write(500, {"error": {"message": "unexpected OpenAI upstream POST"}})

    def _write(self, status, value):
        payload = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def post(url, body, anthropic=True):
    headers = {"Content-Type": "application/json"}
    if anthropic:
        headers.update({"x-api-key": GATEWAY_KEY, "anthropic-version": "2023-06-01"})
        path = "/v1/messages"
    else:
        headers["Authorization"] = "Bearer " + GATEWAY_KEY
        path = "/v1/chat/completions"
    request = Request(url + path, data=json.dumps(body).encode(), headers=headers, method="POST")
    try:
        with urlopen(request, timeout=15) as response:
            return response.status, response.read().decode()
    except HTTPError as error:
        return error.code, error.read().decode()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path, help="extracted TideMux candidate binary")
    args = parser.parse_args()
    if sys.platform != "darwin":
        raise SystemExit("This package-level acceptance check requires the packaged macOS binary.")
    binary = args.binary.resolve()
    if not binary.is_file() or not os.access(binary, os.X_OK):
        raise SystemExit("--binary must point to an executable packaged candidate")
    version = subprocess.check_output([str(binary), "version"], text=True).strip()
    if version != "0.3.3":
        raise RuntimeError(f"candidate version is {version!r}, expected '0.3.3'")

    upstream = Server(("127.0.0.1", 0), UpstreamHandler)
    upstream.posts = []
    upstream.posts_lock = threading.Lock()
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    process = None
    try:
        with tempfile.TemporaryDirectory(prefix="tidemux-server-tools.") as temporary:
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
            upstream_root = f"http://127.0.0.1:{upstream.server_port}"
            config = {
                "listen_addr": f"127.0.0.1:{port}", "max_in_flight": 2,
                "ledger_path": str(ledger),
                "access_token_keychain": {"service": "test.gateway", "account": "local"},
                "routing": {"shared_model_strategy": "random"},
                "providers": {
                    "anthropic-main": {
                        "protocol": "anthropic", "base_url": upstream_root + "/anthropic/v1",
                        "anthropic_version": "2023-06-01", "upstream_id": "anthropic-main",
                        "upstream_keychain": {"service": "test.provider", "account": "anthropic"},
                        "supported_models": ["shared-model"],
                    },
                    "openai-main": {
                        "protocol": "openai", "base_url": upstream_root + "/openai/v1",
                        "upstream_id": "openai-main",
                        "upstream_keychain": {"service": "test.provider", "account": "openai"},
                        "supported_models": ["shared-model"],
                    },
                },
            }
            config_path = root / "config.json"
            config_path.write_text(json.dumps(config), encoding="utf-8")
            env = {
                "PATH": str(fake_bin) + ":/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin",
                "HOME": str(home), "TMPDIR": str(root), "LANG": "C.UTF-8",
            }
            process = subprocess.Popen(
                [str(binary), "serve", "--config", str(config_path)], cwd=root, env=env,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            )
            ready = False
            for _ in range(100):
                if process.poll() is not None:
                    break
                try:
                    with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                        ready = True
                        break
                except OSError:
                    time.sleep(0.05)
            if not ready:
                stdout, stderr = process.communicate(timeout=5)
                raise RuntimeError(f"packaged TideMux failed to start ({process.returncode}): {stderr or stdout}")

            startup = process.stdout.readline()
            if not startup.startswith(f"TideMux listening on http://127.0.0.1:{port}"):
                raise RuntimeError(f"gateway did not print its startup line: {startup!r}")
            tool = {
                "type": "web_search_20250305", "name": "web_search", "max_uses": 3,
                "allowed_domains": ["example.com"],
                "user_location": {"type": "approximate", "country": "US", "city": "Seattle"},
                "provider_extension": {"sentinel": EXTENSION_SENTINEL, "limits": [1, 2]},
            }
            native_body = {
                "model": "shared-model", "max_tokens": 64,
                "messages": [{"role": "user", "content": "Search for a local fixture."}],
                "tools": [tool],
            }
            status, body = post(f"http://127.0.0.1:{port}", native_body)
            if status != 200 or "tool forwarded" not in body:
                raise RuntimeError(f"native Anthropic server tool failed: HTTP {status}: {body}")

            cross_body = dict(native_body, model="openai-main/shared-model")
            cross_status, cross_response = post(f"http://127.0.0.1:{port}", cross_body)
            if (cross_status != 400 or "unsupported_request_feature" not in cross_response
                    or '"param":"tools"' not in cross_response):
                raise RuntimeError(f"cross-protocol server tool error was not actionable: HTTP {cross_status}: {cross_response}")

            process.send_signal(signal.SIGTERM)
            stdout, stderr = process.communicate(timeout=10)
            if process.returncode != 0:
                raise RuntimeError(f"packaged serve exited {process.returncode}: {stderr or stdout}")
            with upstream.posts_lock:
                posts = list(upstream.posts)
            if len(posts) != 1 or posts[0]["path"] != "/anthropic/v1/messages":
                raise RuntimeError(f"unexpected upstream POSTs: {posts}")
            if posts[0]["body"].get("tools") != [tool] or posts[0]["body"].get("model") != "shared-model":
                raise RuntimeError(f"native tool definition changed in transit: {posts[0]['body']}")
            if posts[0]["headers"].get("x-api-key") != PROVIDER_KEY:
                raise RuntimeError("provider API key was not forwarded through fake Keychain credentials")
            if EXTENSION_SENTINEL in stderr or EXTENSION_SENTINEL in stdout:
                raise RuntimeError("provider-hosted tool fields appeared in ordinary logs")
            with sqlite3.connect(ledger, timeout=5) as connection:
                rows = connection.execute("SELECT record_json FROM request_audit").fetchall()
            if len(rows) != 1 or EXTENSION_SENTINEL in json.dumps(rows):
                raise RuntimeError("tool fields were persisted to the local ledger")
            print(json.dumps({
                "binary": str(binary), "version": version,
                "native_tool_forwarded_unchanged": True,
                "shared_route_respected_anthropic_protocol": True,
                "cross_protocol_error_actionable": True,
                "tool_fields_absent_from_logs_and_ledger": True,
                "fake_credentials_and_loopback_mocks_only": True,
            }, ensure_ascii=False))
    finally:
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        upstream.shutdown()
        upstream.server_close()


if __name__ == "__main__":
    main()
