#!/usr/bin/env python3
"""Exercise JSON runtime events through an extracted local candidate binary.

The test uses fake Keychain credentials and loopback HTTP mocks only. It does
not read the user's Keychain or contact a real provider.
"""
import argparse
from datetime import datetime
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

from runtime_event_checks import validate_runtime_events

GATEWAY_KEY = "local-gateway-key"
PROVIDER_KEY = "local-provider-key"
REQUEST_SENTINEL = "package_request_body_sensitive_sentinel"
UPSTREAM_SENTINEL = "package_upstream_body_sensitive_sentinel"
QUERY_SENTINEL = "package_query_sensitive_sentinel"
SESSION_SENTINEL = "package_session_sensitive_sentinel"


class Server(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True


class UpstreamHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def do_GET(self):
        body = json.dumps({"object": "list", "data": [{"id": "custom-model"}]}, separators=(",", ":")).encode()
        self._write(200, body)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length))
        self.server.posts.append({
            "body": body,
            "auth_ok": self.headers.get("Authorization") == "Bearer " + PROVIDER_KEY,
        })
        if self.path.startswith("/success/"):
            self._write(200, json.dumps({
                "id": "test", "object": "chat.completion", "model": "custom-model",
                "choices": [{"index": 0, "message": {"role": "assistant", "content": UPSTREAM_SENTINEL}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 7, "completion_tokens": 2,
                          "prompt_tokens_details": {"cached_tokens": 3, "cache_write_tokens": 1}},
            }).encode())
            return
        if body.get("stream"):
            # Deliver content, then EOF without a terminal marker.
            frame = {"choices": [{"index": 0, "delta": {"content": UPSTREAM_SENTINEL}}]}
            self._write(200, b"data: " + json.dumps(frame).encode() + b"\n\n", "text/event-stream")
            return
        self._write(403, json.dumps({"error": {
            "code": "private-provider-code",
            "message": UPSTREAM_SENTINEL,
        }}).encode())

    def _write(self, status, payload, content_type="application/json"):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def start_upstream():
    server = Server(("127.0.0.1", 0), UpstreamHandler)
    server.posts = []
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def request(url, method, body=None, token=GATEWAY_KEY, session_id=None):
    headers = {"Authorization": "Bearer " + token}
    if session_id:
        headers["X-TideMux-Session-ID"] = session_id
    data = None if body is None else json.dumps(body).encode()
    if data is not None:
        headers["Content-Type"] = "application/json"
    req = Request(url, data=data, headers=headers, method=method)
    try:
        with urlopen(req, timeout=10) as response:
            return response.status, response.read().decode(), response.headers
    except HTTPError as error:
        return error.code, error.read().decode(), error.headers


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path, help="extracted TideMux candidate binary")
    args = parser.parse_args()
    if sys.platform != "darwin":
        raise SystemExit("This package-level acceptance check requires the packaged macOS binary.")
    binary = args.binary.resolve()
    if not binary.is_file() or not os.access(binary, os.X_OK):
        raise SystemExit("--binary must point to an executable packaged candidate")

    upstream = start_upstream()
    process = None
    try:
        with tempfile.TemporaryDirectory(prefix="tidemux-runtime-logs.") as temporary:
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
                "providers": {name: {
                    "protocol": "openai",
                    "base_url": f"http://127.0.0.1:{upstream.server_port}/{name}/v1",
                    "upstream_id": name,
                    "upstream_keychain": {"service": "test.provider", "account": "local"},
                    "forward_error_messages": False,
                    "supported_models": ["custom-model"],
                } for name in ("mock", "success", "stream")},
            }
            config_path = root / "config.json"
            config_path.write_text(json.dumps(config), encoding="utf-8")
            env = {
                "PATH": str(fake_bin) + ":/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin",
                "HOME": str(home), "TMPDIR": str(root), "LANG": "C.UTF-8",
            }
            process = subprocess.Popen(
                [str(binary), "serve", "--config", str(config_path)],
                cwd=root, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
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
                raise RuntimeError(f"packaged TideMux failed to start (exit {process.returncode}): {stderr or stdout}")
            startup_line = process.stdout.readline()
            if not startup_line.startswith(f"TideMux listening on http://127.0.0.1:{port}"):
                raise RuntimeError(f"serve stdout was not human-readable CLI output: {startup_line!r}")

            status, body, headers = request(
                f"http://127.0.0.1:{port}/v1/chat/completions?{QUERY_SENTINEL}",
                "POST", {"model": "mock/custom-model", "messages": [{"role": "user", "content": REQUEST_SENTINEL}]},
                session_id=SESSION_SENTINEL,
            )
            if status != 502 or not headers.get("X-TideMux-Request-ID"):
                raise RuntimeError(f"mock upstream error was not relayed safely: HTTP {status}: {body}")
            terminal_id = headers["X-TideMux-Request-ID"]

            outcomes = [(terminal_id, "mock", "error", 502)]
            for provider, stream in (("success", False), ("stream", True)):
                status, body, headers = request(
                    f"http://127.0.0.1:{port}/v1/chat/completions",
                    "POST", {"model": provider + "/custom-model", "stream": stream,
                             "messages": [{"role": "user", "content": REQUEST_SENTINEL}]},
                    session_id=SESSION_SENTINEL,
                )
                if status != 200 or not headers.get("X-TideMux-Request-ID"):
                    raise RuntimeError(f"{provider} fixture failed: HTTP {status}")
                if stream and ("event: error" not in body or "[DONE]" in body):
                    raise RuntimeError("incomplete HTTP-200 stream did not end with an error")
                outcomes.append((headers["X-TideMux-Request-ID"], provider, "error" if stream else "success", 200))

            status, _, headers = request(
                f"http://127.0.0.1:{port}/v1/unsupported?{QUERY_SENTINEL}", "GET", token="invalid-local-token",
            )
            if status != 401 or not headers.get("X-TideMux-Request-ID"):
                raise RuntimeError(f"local rejection did not return a request ID: HTTP {status}")
            rejection_id = headers["X-TideMux-Request-ID"]

            process.send_signal(signal.SIGTERM)
            stdout, stderr = process.communicate(timeout=10)
            if process.returncode != 0:
                raise RuntimeError(f"packaged serve exited {process.returncode}: {stderr or stdout}")

            events = [json.loads(line) for line in stderr.splitlines() if line.strip()]
            version = subprocess.check_output([str(binary), "version"], text=True).strip()
            validate_runtime_events(events, version)
            for event in events:
                if event.get("schema_version") != 2:
                    raise RuntimeError("unexpected runtime schema version")
                datetime.fromisoformat(event["timestamp"].replace("Z", "+00:00"))
            for identity, provider, outcome, status in outcomes:
                matches = [event for event in events if event.get("event") == "request_terminal"
                           and event.get("requestId") == identity]
                if len(matches) != 1:
                    raise RuntimeError("missing or duplicate terminal event for " + provider)
                event, = matches
                if (event.get("provider_ref"), event.get("outcome"), event.get("http_status")) != (provider, outcome, status):
                    raise RuntimeError("terminal outcome/status mismatch for " + provider)
                for field in ("http_status", "latency_ms", "queue_time_ms"):
                    if type(event.get(field)) is not int or event[field] < 0:
                        raise RuntimeError("invalid numeric terminal field: " + field)
                if event.get("record_persisted") is not True:
                    raise RuntimeError("terminal event is not joined to persisted accounting")
                if event.get("message", {}).get("model") != "custom-model":
                    raise RuntimeError("terminal event lost its canonical model")
                if any(field in event for field in ("session_id", "estimated_cost", "price_snapshot", "input_tokens", "output_tokens")):
                    raise RuntimeError("terminal event included session/accounting fields")
                usage = event.get("message", {}).get("usage")
                if provider == "success":
                    expected = {"input_tokens": 3, "output_tokens": 2,
                                "cache_read_input_tokens": 3, "cache_creation_input_tokens": 1}
                    if usage != expected:
                        raise RuntimeError("canonical known usage changed")
                elif usage is not None:
                    raise RuntimeError("failed request fabricated unknown usage")
            rejections = [event for event in events if event.get("event") == "local_rejection"
                          and event.get("requestId") == rejection_id]
            if len(rejections) != 1 or rejections[0].get("http_status") != 401:
                raise RuntimeError(f"local rejection JSON event did not correlate to {rejection_id}: {stderr}")
            if not any(event.get("event") == "gateway_start" for event in events) or not any(
                event.get("event") == "gateway_shutdown" for event in events
            ):
                raise RuntimeError(f"missing gateway lifecycle events: {events}")
            with sqlite3.connect(ledger, timeout=5) as connection:
                audit_ids = {row[0] for row in connection.execute("SELECT id FROM request_audit")}
                diagnostic_ids = {row[0] for row in connection.execute("SELECT id FROM local_diagnostics")}
            if not {item[0] for item in outcomes}.issubset(audit_ids) or rejection_id not in diagnostic_ids:
                raise RuntimeError("runtime request IDs did not join to their persisted audit/diagnostic rows")
            for secret in (GATEWAY_KEY, PROVIDER_KEY, REQUEST_SENTINEL, UPSTREAM_SENTINEL, QUERY_SENTINEL, SESSION_SENTINEL, "invalid-local-token"):
                if secret in stderr:
                    raise RuntimeError(f"runtime logs exposed sensitive marker {secret!r}")
            if len(upstream.posts) != 3 or any(not post["auth_ok"] or REQUEST_SENTINEL not in json.dumps(post["body"]) for post in upstream.posts):
                raise RuntimeError(f"unexpected provider mock traffic: {upstream.posts}")
            for ledger_file in root.glob("ledger.db*"):
                if ledger_file.is_file() and REQUEST_SENTINEL.encode() in ledger_file.read_bytes():
                    raise RuntimeError(f"request body was persisted in {ledger_file.name}")
            print(json.dumps({
                "binary": str(binary), "jsonl_events": len(events),
                "terminal_request_correlated": True, "local_rejection_correlated": True,
                "stdout_human_readable": True, "stderr_jsonl": True,
                "ledger_join_verified": True, "request_body_not_persisted": True,
                "http_200_stream_failure": True, "known_and_unknown_usage": True,
                "typed_fields_and_timestamps": True,
                "sensitive_markers_absent": True, "fake_credentials_only": True,
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
