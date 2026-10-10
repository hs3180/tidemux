#!/usr/bin/env python3
"""Check shared-model session affinity with an extracted macOS candidate.

Uses fake Keychain credentials and loopback upstreams. TTL and caller-namespace
isolation are covered by deterministic Go tests; this check exercises the
packaged request/dispatch boundary, concurrency, failures and privacy.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
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

GATEWAY_KEY = "affinity-local-gateway-key"
PROVIDER_KEY = "affinity-local-provider-key"
SESSION = "affinity_session_must_not_be_logged"
BODY = "affinity_body_must_not_be_logged"


class Server(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True


class UpstreamHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def do_GET(self):
        self.write_payload(200, {"object": "list", "data": [{"id": "shared-model"}, {"id": "other-model"}]})

    def do_POST(self):
        payload = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        provider = self.path.split("/")[1]
        with self.server.lock:
            self.server.posts.append((provider, payload["model"], self.headers.get("X-TideMux-Session-ID", "")))
            exhausted = provider in self.server.exhausted
        if exhausted:
            self.write_payload(403, {"error": {"code": "credits_empty", "message": "private_provider_billing_detail"}})
            return
        if payload.get("stream"):
            data = (
                'data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}\n\n'
                'event: error\ndata: {"error":{"code":"credits_empty","status":403}}\n\n'
            ).encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            self.wfile.flush()
            return
        self.write_payload(200, {
            "id": "chatcmpl-affinity", "object": "chat.completion", "model": payload["model"],
            "choices": [{"index": 0, "message": {"role": "assistant", "content": "provider:" + provider}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 2, "completion_tokens": 2, "total_tokens": 4},
        })

    def write_payload(self, status, value):
        data = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def post(url, session=None, protocol="openai", model="shared-model", metadata=None, stream=False):
    headers = {"Content-Type": "application/json"}
    body = {"model": model, "messages": [{"role": "user", "content": BODY}]}
    if protocol == "anthropic":
        headers.update({"x-api-key": GATEWAY_KEY, "anthropic-version": "2023-06-01"})
        path = "/v1/messages"
        body["max_tokens"] = 32
        if metadata is not None:
            body["metadata"] = {"user_id": metadata}
    else:
        headers["Authorization"] = "Bearer " + GATEWAY_KEY
        path = "/v1/chat/completions"
    if session is not None:
        headers["X-TideMux-Session-ID"] = session
    if stream:
        body["stream"] = True
    request = Request(url + path, data=json.dumps(body).encode(), headers=headers, method="POST")
    try:
        with urlopen(request, timeout=30) as response:
            return response.status, response.read().decode()
    except HTTPError as error:
        return error.code, error.read().decode()


def provider_of(result):
    status, text = result
    if status != 200:
        raise RuntimeError(f"affinity request failed with HTTP {status}")
    value = json.loads(text)
    if "choices" in value:
        return value["choices"][0]["message"]["content"].removeprefix("provider:")
    return value["content"][0]["text"].removeprefix("provider:")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    args = parser.parse_args()
    binary = args.binary.resolve()
    if sys.platform != "darwin" or not binary.is_file() or not os.access(binary, os.X_OK):
        raise SystemExit("--binary must be an executable packaged macOS candidate")
    version = subprocess.check_output([str(binary), "version"], text=True).strip()
    if version != "0.3.3":
        raise RuntimeError(f"candidate version is {version!r}, expected 0.3.3")
    upstream = Server(("127.0.0.1", 0), UpstreamHandler)
    upstream.lock, upstream.posts, upstream.exhausted = threading.Lock(), [], set()
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    process = None
    try:
        with tempfile.TemporaryDirectory(prefix="tidemux-affinity.") as temporary:
            root = Path(temporary)
            fake_bin = root / "bin"
            fake_bin.mkdir()
            security = fake_bin / "security"
            security.write_text(
                '#!/bin/sh\ncase "$*" in '
                '*test.gateway*) printf "affinity-local-gateway-key\\n";; '
                '*test.provider*) printf "affinity-local-provider-key\\n";; '
                '*) exit 1;; esac\n', encoding="utf-8",
            )
            security.chmod(0o755)
            ledger = root / "ledger.db"
            port = free_port()
            url = f"http://127.0.0.1:{port}"
            config = {
                "listen_addr": f"127.0.0.1:{port}", "max_in_flight": 4,
                "ledger_path": str(ledger),
                "access_token_keychain": {"service": "test.gateway", "account": "local"},
                "routing": {"shared_model_strategy": "random", "billing_exhaustion_failover": True},
                "auto_chain": [{"provider": "a", "model": "shared-model"}],
                "providers": {
                    ref: {
                        "protocol": "openai", "base_url": f"http://127.0.0.1:{upstream.server_port}/{ref}/v1",
                        "upstream_id": ref, "upstream_keychain": {"service": "test.provider", "account": ref},
                        "supported_models": ["shared-model", "other-model"],
                        "error_code_mappings": [{"upstream_code": "credits_empty", "http_status": 403, "category": "insufficient_balance"}],
                    } for ref in ("a", "b")
                },
            }
            config_path = root / "config.json"
            config_path.write_text(json.dumps(config), encoding="utf-8")
            home = root / "home"
            home.mkdir()
            env = {"PATH": str(fake_bin) + ":/usr/bin:/bin:/opt/homebrew/bin", "HOME": str(home), "TMPDIR": str(root), "LANG": "C.UTF-8"}
            stdout_path, stderr_path = root / "stdout.log", root / "stderr.log"
            with stdout_path.open("w") as stdout, stderr_path.open("w") as stderr:
                def start():
                    started = subprocess.Popen([str(binary), "serve", "--config", str(config_path)], cwd=root, env=env, stdout=stdout, stderr=stderr)
                    for _ in range(200):
                        if started.poll() is not None:
                            raise RuntimeError(f"packaged gateway exited during startup: {started.returncode}")
                        try:
                            with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                                return started
                        except OSError:
                            time.sleep(0.05)
                    started.terminate()
                    started.wait(timeout=5)
                    raise RuntimeError("packaged gateway did not become ready")

                def stop(started):
                    started.send_signal(signal.SIGTERM)
                    started.wait(timeout=10)
                    if started.returncode != 0:
                        raise RuntimeError(f"packaged gateway exited {started.returncode}")

                process = start()
                with ThreadPoolExecutor(max_workers=16) as pool:
                    first = list(pool.map(lambda _: provider_of(post(url, SESSION)), range(32)))
                selected = first[0]
                if set(first) != {selected}:
                    raise RuntimeError("concurrent first requests did not share one provider")
                repeated = [provider_of(post(url, SESSION)) for _ in range(8)]
                if set(repeated) != {selected}:
                    raise RuntimeError("repeated session requests changed provider")
                anthropic = [provider_of(post(url, protocol="anthropic", metadata=SESSION)) for _ in range(8)]
                same_identity = provider_of(post(url, SESSION, protocol="anthropic", metadata="ignored-metadata"))
                if len(set(anthropic + [same_identity])) != 1:
                    raise RuntimeError("Anthropic metadata fallback/header precedence did not preserve affinity")
                if provider_of(post(url, SESSION, model="a/shared-model")) != "a":
                    raise RuntimeError("explicit provider request used session affinity")
                if provider_of(post(url, SESSION, model="auto")) != "a":
                    raise RuntimeError("auto chain used shared-model session state")
                if provider_of(post(url, SESSION)) != selected:
                    raise RuntimeError("explicit/auto routing changed shared-model binding")
                without_id = {provider_of(post(url)) for _ in range(64)}
                if without_id != {"a", "b"}:
                    raise RuntimeError("no-ID requests did not retain per-request random routing")

                with upstream.lock:
                    upstream.exhausted.add(selected)
                    before = len(upstream.posts)
                status, error_body = post(url, SESSION)
                with upstream.lock:
                    attempted = upstream.posts[before:]
                if status != 403 or len(attempted) != 1 or attempted[0][0] != selected:
                    raise RuntimeError("dispatched session request crossed providers on billing exhaustion")
                if "insufficient_balance" not in error_body or "private_provider_billing_detail" in error_body:
                    raise RuntimeError("billing error was not safely classified")
                rebound = provider_of(post(url, SESSION))
                if rebound == selected:
                    raise RuntimeError("next request did not rebind from a cooled provider")
                stop(process)
                with upstream.lock:
                    upstream.exhausted.clear()
                process = start()
                with upstream.lock:
                    before = len(upstream.posts)
                status, stream_body = post(url, SESSION + "-stream", stream=True)
                with upstream.lock:
                    attempts = upstream.posts[before:]
                if status != 200 or "partial" not in stream_body or "event: error" not in stream_body or len(attempts) != 1:
                    raise RuntimeError("streaming session request switched providers or hid its terminal error")
                stop(process)

            logs = stdout_path.read_text() + stderr_path.read_text()
            with sqlite3.connect(ledger, timeout=5) as connection:
                rows = connection.execute("SELECT record_json FROM request_audit").fetchall()
            stored = json.dumps(rows)
            for secret in (GATEWAY_KEY, PROVIDER_KEY, SESSION, BODY, "private_provider_billing_detail", "ignored-metadata"):
                if secret in logs or secret in stored:
                    raise RuntimeError("credentials, session identifiers or bodies reached logs/ledger")
            with upstream.lock:
                posts = list(upstream.posts)
            if len(rows) != len(posts) or any(session != SESSION for _, _, session in posts[:40]):
                raise RuntimeError("upstream session forwarding or per-attempt audit count was incorrect")
            if any(json.loads(row[0]).get("provider_ref") != json.loads(row[0])["upstream"] for row in rows):
                raise RuntimeError("audit did not identify the actual selected provider profile")
            print(json.dumps({
                "binary": str(binary), "version": version,
                "concurrent_first_requests": 32, "session_affinity": True,
                "anthropic_metadata_fallback_and_header_precedence": True,
                "explicit_and_auto_routes_separate": True, "no_id_random_requests": 64,
                "billing_error_not_replayed": True, "next_request_rebinds_after_cooldown": True,
                "stream_not_replayed": True, "logs_and_ledger_private": True,
                "audit_rows": len(rows), "fake_credentials_and_loopback_mocks_only": True,
            }))
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
