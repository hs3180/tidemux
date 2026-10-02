#!/usr/bin/env python3
"""Verify #92/#94/#90 through an extracted binary using isolated loopback mocks.

No real Keychain, provider credentials or paid upstream calls are used.
Use --long-stream-seconds 65 to exercise the former 60-second cutoff on a
real clock; the normal CI gate keeps that slow check in the Go virtual-clock test.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler
import json
import os
from pathlib import Path
import signal
import socket
import sqlite3
import subprocess
import tempfile
import threading
import time
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from test_runtime_logs_package import GATEWAY_KEY, PROVIDER_KEY, Server, free_port

BODY_MARKER = "private-client-reliability-body"
SESSION_MARKER = "private-client-reliability-session"


def start_frame(protocol):
    if protocol == "anthropic":
        return (b'event: message_start\ndata: {"type":"message_start","message":'
                b'{"id":"msg_mock","type":"message","role":"assistant","model":"custom-model",'
                b'"content":[],"usage":{"input_tokens":3,"output_tokens":1}}}\n\n')
    return b'data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}\n\n'


def end_frame(protocol):
    if protocol == "anthropic":
        return (b'event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn"},'
                b'"usage":{"output_tokens":2}}\n\nevent: message_stop\ndata: {"type":"message_stop"}\n\n')
    return (b'data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],'
            b'"usage":{"prompt_tokens":3,"completion_tokens":2}}\n\ndata: [DONE]\n\n')


class UpstreamHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def do_GET(self):
        self.reply({"object": "list", "data": [{"id": "custom-model"}]})

    def reply(self, body):
        data = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        protocol = "anthropic" if self.path.endswith("/messages") else "openai"
        serialized = json.dumps(body)
        with self.server.lock:
            self.server.posts += 1
            if '"type": "image"' in serialized:
                self.server.image_preserved = len(body["messages"][0]["content"][1]["source"]["data"]) == 1200 << 10
        if "hold" in serialized or "long" in serialized:
            if body.get("stream"):
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Connection", "close")
                self.end_headers()
                self.close_connection = True
                self.wfile.write(start_frame(protocol))
                self.wfile.flush()
            self.server.started.set()
            if "hold" in serialized:
                while not self.server.stop.wait(0.05):
                    if body.get("stream"):
                        try:
                            self.wfile.write(b": heartbeat\n\n")
                            self.wfile.flush()
                        except (BrokenPipeError, ConnectionResetError):
                            return
                return
            deadline = time.monotonic() + self.server.long_seconds
            while time.monotonic() < deadline:
                if self.server.stop.wait(min(1, max(0, deadline - time.monotonic()))):
                    return
                try:
                    self.wfile.write(b": heartbeat\n\n")
                    self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError):
                    return
            self.wfile.write(end_frame(protocol))
            self.wfile.flush()
            return
        if protocol == "anthropic":
            self.reply({"id": "msg_mock", "type": "message", "role": "assistant", "model": "custom-model",
                        "content": [{"type": "text", "text": "hi"}], "stop_reason": "end_turn",
                        "usage": {"input_tokens": 3, "output_tokens": 2}})
        else:
            self.reply({"id": "chat_mock", "object": "chat.completion", "model": "custom-model",
                        "choices": [{"index": 0, "message": {"role": "assistant", "content": "hi"},
                                     "finish_reason": "stop"}], "usage": {"prompt_tokens": 3, "completion_tokens": 2}})


def request(gateway, protocol, text=BODY_MARKER, stream=False, session=None, image=False):
    content = text
    if image:
        content = [{"type": "text", "text": text}, {"type": "image", "source": {
            "type": "base64", "media_type": "image/png", "data": "a" * (1200 << 10)}}]
    body = {"model": protocol + "/custom-model", "messages": [{"role": "user", "content": content}]}
    if protocol == "anthropic":
        body["max_tokens"] = 16
    if stream:
        body["stream"] = True
    headers = {"Authorization": "Bearer " + GATEWAY_KEY, "Content-Type": "application/json"}
    if protocol == "anthropic":
        headers = {"x-api-key": GATEWAY_KEY, "anthropic-version": "2023-06-01", "Content-Type": "application/json"}
    if session:
        headers["X-TideMux-Session-ID"] = session
    endpoint = "/v1/messages" if protocol == "anthropic" else "/v1/chat/completions"
    req = Request(gateway["url"] + endpoint, json.dumps(body).encode(), headers, method="POST")
    try:
        return urlopen(req, timeout=max(10, gateway["upstream"].long_seconds + 10))
    except HTTPError as err:
        return err


def read_response(response):
    with response:
        return response.status, response.read().decode(), response.headers


@contextmanager
def running_gateway(binary, *, request_limit=None, capacity=0, long_seconds=0):
    with tempfile.TemporaryDirectory(prefix="tidemux-client-reliability.") as temporary:
        root = Path(temporary)
        fake_bin = root / "bin"
        fake_bin.mkdir()
        security = fake_bin / "security"
        security.write_text("#!/bin/sh\ncase \"$*\" in *test.gateway*) printf '" + GATEWAY_KEY +
                            "\\n';; *test.provider*) printf '" + PROVIDER_KEY + "\\n';; *) exit 1;; esac\n")
        security.chmod(0o755)
        home = root / "home"
        home.mkdir()
        upstream = Server(("127.0.0.1", 0), UpstreamHandler)
        upstream.lock, upstream.stop, upstream.started = threading.Lock(), threading.Event(), threading.Event()
        upstream.posts, upstream.image_preserved, upstream.long_seconds = 0, False, long_seconds
        threading.Thread(target=upstream.serve_forever, daemon=True).start()
        port = free_port()
        ledger = root / "ledger.db"
        config = {"listen_addr": f"127.0.0.1:{port}", "max_in_flight": 2, "max_active_sessions": capacity,
                  "ledger_path": str(ledger), "access_token_keychain": {"service": "test.gateway", "account": "local"},
                  "providers": {protocol: {"protocol": protocol, "base_url": f"http://127.0.0.1:{upstream.server_port}/v1",
                                            "upstream_id": protocol, "upstream_keychain": {"service": "test.provider", "account": "local"},
                                            "supported_models": ["custom-model"]} for protocol in ("openai", "anthropic")}}
        if request_limit is not None:
            config["limits"] = {"request_bytes": request_limit}
        config_file = root / "config.json"
        config_file.write_text(json.dumps(config))
        env = {"PATH": str(fake_bin) + ":/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin", "HOME": str(home), "TMPDIR": str(root), "LANG": "C.UTF-8"}
        process = None
        try:
            with (root / "stdout").open("w+") as stdout, (root / "stderr").open("w+") as stderr:
                process = subprocess.Popen([str(binary), "serve", "--config", str(config_file)], cwd=root, env=env, stdout=stdout, stderr=stderr)
                gateway = {"process": process, "upstream": upstream, "url": f"http://127.0.0.1:{port}", "ledger": ledger}
                for _ in range(100):
                    if process.poll() is not None:
                        stderr.seek(0)
                        raise RuntimeError("candidate failed to start: " + stderr.read())
                    try:
                        with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                            break
                    except OSError:
                        time.sleep(0.05)
                else:
                    raise RuntimeError("candidate did not start")
                yield gateway
                if process.poll() is None:
                    process.send_signal(signal.SIGTERM)
                process.wait(timeout=10)
                if process.returncode != 0:
                    stderr.seek(0)
                    raise RuntimeError("candidate shutdown failed: " + stderr.read())
                stderr.seek(0)
                logs = stderr.read()
                events = [json.loads(line) for line in logs.splitlines() if line.strip()]
                if not any(e.get("event") == "gateway_shutdown" and e.get("outcome") == "success" for e in events):
                    raise RuntimeError("missing successful shutdown event")
                with sqlite3.connect(ledger) as db:
                    persisted = "\n".join(db.iterdump())
                for marker in (GATEWAY_KEY, PROVIDER_KEY, BODY_MARKER, SESSION_MARKER):
                    if marker in logs or marker in persisted:
                        raise RuntimeError("sensitive test marker persisted")
        finally:
            if process and process.poll() is None:
                process.kill()
                process.wait(timeout=5)
            upstream.stop.set()
            upstream.shutdown()
            upstream.server_close()


def check_shutdown(binary, protocol, stream):
    with running_gateway(binary) as gateway:
        with ThreadPoolExecutor(max_workers=1) as executor:
            pending = executor.submit(request, gateway, protocol, "hold " + BODY_MARKER, stream)
            if not gateway["upstream"].started.wait(5):
                raise RuntimeError("mock request did not start")
            response = pending.result(timeout=5) if stream else None
            if stream:
                # Consume a complete first frame before delivering SIGTERM.
                while response.readline().strip():
                    pass
            gateway["process"].send_signal(signal.SIGTERM)
            status, data, _ = read_response(response if stream else pending.result(timeout=5))
            expected = 200 if stream else 503
            if status != expected or '"code":"server_shutting_down"' not in data:
                raise RuntimeError(f"{protocol} shutdown: HTTP {status}: {data}")
            if stream and ("event: error\n" not in data or "[DONE]" in data or "message_stop" in data):
                raise RuntimeError("shutdown stream did not end with one safe error")
            gateway["process"].wait(timeout=5)
            if gateway["upstream"].posts != 1:
                raise RuntimeError("shutdown replayed a request")
            with sqlite3.connect(gateway["ledger"]) as db:
                rows = db.execute("SELECT status,json_extract(record_json,'$.error_code') FROM request_audit").fetchall()
            if rows != [("canceled", "server_shutting_down")]:
                raise RuntimeError(f"shutdown audit: {rows}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--long-stream-seconds", type=int, default=0, choices=[0, *range(61, 81)])
    args = parser.parse_args()
    binary = args.binary.resolve()
    if not binary.is_file() or not os.access(binary, os.X_OK):
        raise SystemExit("--binary must be an extracted executable candidate")
    with running_gateway(binary) as gateway:
        status, _, _ = read_response(request(gateway, "anthropic", image=True))
        if status != 200 or not gateway["upstream"].image_preserved or gateway["upstream"].posts != 1:
            raise RuntimeError("image request above 1 MiB did not survive the gateway")
    with running_gateway(binary, request_limit=2048) as gateway:
        for protocol in ("openai", "anthropic"):
            status, data, _ = read_response(request(gateway, protocol, BODY_MARKER + "a" * 3000))
            detail = json.loads(data)["error"]
            if status != 413 or detail.get("code") != "request_too_large" or detail.get("limit_bytes") != 2048:
                raise RuntimeError(f"{protocol} request limit: {status}: {data}")
        if gateway["upstream"].posts:
            raise RuntimeError("oversize request was dispatched")
    with running_gateway(binary, capacity=1) as gateway:
        status, _, _ = read_response(request(gateway, "openai", session=SESSION_MARKER + "-a"))
        if status != 200:
            raise RuntimeError("first session was rejected")
        for protocol in ("openai", "anthropic"):
            status, data, headers = read_response(request(gateway, protocol, session=SESSION_MARKER + "-" + protocol))
            detail = json.loads(data)["error"]
            if (status != 429 or headers.get("Retry-After") or detail.get("code") != "active_session_limit"
                    or detail.get("scope") != "gateway" or detail.get("limit") != 1
                    or detail.get("retry", {}).get("strategy") != "exponential_backoff_with_jitter"):
                raise RuntimeError(f"{protocol} capacity guidance: {status}: {data}")
        if gateway["upstream"].posts != 1:
            raise RuntimeError("capacity rejection was dispatched")
    for protocol in ("openai", "anthropic"):
        for stream in (False, True):
            check_shutdown(binary, protocol, stream)
    elapsed = None
    if args.long_stream_seconds:
        with running_gateway(binary, long_seconds=args.long_stream_seconds) as gateway:
            started = time.monotonic()
            status, data, _ = read_response(request(gateway, "anthropic", "long " + BODY_MARKER, stream=True))
            elapsed = round(time.monotonic() - started, 2)
            if status != 200 or "message_stop" not in data or "event: error\n" in data or elapsed < args.long_stream_seconds:
                raise RuntimeError(f"long stream ended early: {elapsed}s HTTP {status}: {data[-500:]}")
    print(json.dumps({"binary": str(binary), "image_above_one_mib_preserved": True, "explicit_body_limit_and_guidance": True,
                      "both_protocol_capacity_guidance": True, "sigterm_json_and_sse": True,
                      "shutdown_not_replayed": True, "cancellation_audits_persisted": True, "logs_and_ledger_private": True,
                      "real_clock_stream_seconds": elapsed, "fake_credentials_and_loopback_mocks_only": True}, sort_keys=True))


if __name__ == "__main__":
    main()
