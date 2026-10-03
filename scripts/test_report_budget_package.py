#!/usr/bin/env python3
"""Exercise live report opens, queued cancellation and crash recovery in a binary.

Uses temporary state, fake Keychain credentials and loopback upstreams only.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
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


class Upstream(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        self.reply({"object": "list", "data": [{"id": "custom-model"}]})

    def reply(self, data):
        payload = json.dumps(data).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        self.server.posts += 1
        if "hold" in json.dumps(body):
            self.server.started.set()
            self.server.release.wait(20)
        self.reply({"id": "chat_test", "object": "chat.completion", "model": "custom-model",
                    "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"},
                                 "finish_reason": "stop"}], "usage": {"prompt_tokens": 3, "completion_tokens": 1}})


def rows(ledger):
    with sqlite3.connect(ledger, timeout=5) as db:
        return db.execute("SELECT state FROM budget_charges ORDER BY request_id").fetchall()


def wait_for(check, reason):
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(.02)
    raise RuntimeError(reason)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    args = parser.parse_args()
    binary = args.binary.resolve()
    upstream = Server(("127.0.0.1", 0), Upstream)
    upstream.started, upstream.release, upstream.posts = threading.Event(), threading.Event(), 0
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    process = None
    try:
        with tempfile.TemporaryDirectory(prefix="tidemux-report-budget.") as temporary:
            root = Path(temporary)
            fake_bin = root / "bin"
            fake_bin.mkdir()
            security = fake_bin / "security"
            security.write_text("#!/bin/sh\ncase \"$*\" in *test.gateway*) printf '" + GATEWAY_KEY +
                                "\\n';; *test.provider*) printf '" + PROVIDER_KEY + "\\n';; *) exit 1;; esac\n")
            security.chmod(0o755)
            ledger = root / "ledger.db"
            port = free_port()
            config = {"listen_addr": f"127.0.0.1:{port}", "max_in_flight": 1,
                      "ledger_path": str(ledger), "access_token_keychain": {"service": "test.gateway", "account": "local"},
                      "providers": {"mock": {"protocol": "openai", "base_url": f"http://127.0.0.1:{upstream.server_port}/v1",
                                             "upstream_id": "mock", "upstream_keychain": {"service": "test.provider", "account": "local"},
                                             "supported_models": ["custom-model"], "prices": {"custom-model": {
                                                 "currency": "USD", "source": "mock", "version": "1",
                                                 "input_cache_hit_per_million": 0, "input_cache_miss_per_million": 1, "output_per_million": 1}},
                                             "budget": {"currency": "USD", "five_hour_limit": 10,
                                                        "weekly_limit": 20, "alert_threshold": .8, "mode": "hard"}}}}
            config_file = root / "config.json"
            config_file.write_text(json.dumps(config))
            env = {"PATH": str(fake_bin) + ":/usr/bin:/bin", "HOME": str(root), "TMPDIR": str(root)}
            private_marker = "report-budget-private-body"

            def start():
                nonlocal process
                process = subprocess.Popen([str(binary), "serve", "--config", str(config_file)],
                                           env=env, cwd=root, stdout=subprocess.DEVNULL, stderr=log)
                def ready():
                    if process.poll() is not None:
                        raise RuntimeError("gateway failed to start: " + (root / "stderr.log").read_text())
                    try:
                        with socket.create_connection(("127.0.0.1", port), timeout=.1):
                            return True
                    except OSError:
                        return False
                wait_for(ready, "gateway did not listen")

            def post(text, anthropic=False):
                body = {"model": "mock/custom-model", "messages": [{"role": "user", "content": text}]}
                if anthropic:
                    body["max_tokens"] = 16
                req = Request(f"http://127.0.0.1:{port}/v1/" + ("messages" if anthropic else "chat/completions"),
                              json.dumps(body).encode(), {"Authorization": "Bearer " + GATEWAY_KEY,
                              "x-api-key": GATEWAY_KEY, "Content-Type": "application/json"})
                try:
                    response = urlopen(req, timeout=20)
                except HTTPError as error:
                    response = error
                with response:
                    return response.status, response.read()

            with (root / "stderr.log").open("wb") as log, ThreadPoolExecutor(max_workers=1) as executor:
                start()
                held = executor.submit(post, "hold " + private_marker)
                if not upstream.started.wait(5):
                    raise RuntimeError("held request was not dispatched")
                queued = socket.create_connection(("127.0.0.1", port))
                body = json.dumps({"model": "mock/custom-model", "messages": [{"role": "user", "content": private_marker}]}).encode()
                queued.sendall((f"POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer {GATEWAY_KEY}\r\n"
                                f"Content-Type: application/json\r\nContent-Length: {len(body)}\r\n\r\n").encode() + body)
                wait_for(lambda: rows(ledger) == [("pending",), ("pending",)], "two pending reservations missing")
                for command in [["list"], ["generate"], ["export", "--days", "1", "--output", str(root / "report.html")]]:
                    result = subprocess.run([str(binary), "report", *command, "--config", str(config_file)], env=env, capture_output=True)
                    if result.returncode or rows(ledger) != [("pending",), ("pending",)]:
                        raise RuntimeError("report disrupted live reservations: " + command[0])
                # A second gateway with a different port must fail before recovery.
                second = dict(config, listen_addr=f"127.0.0.1:{free_port()}")
                second_file = root / "second.json"
                second_file.write_text(json.dumps(second))
                duplicate = subprocess.run([str(binary), "serve", "--config", str(second_file)], env=env, capture_output=True, timeout=10)
                if duplicate.returncode == 0 or rows(ledger) != [("pending",), ("pending",)]:
                    raise RuntimeError("second gateway recovered a live ledger")
                queued.shutdown(socket.SHUT_RDWR)
                queued.close()
                wait_for(lambda: rows(ledger) == [("pending",)], "queued cancellation did not release its reservation")
                if upstream.posts != 1:
                    raise RuntimeError("queued request reached upstream")
                upstream.release.set()
                if held.result(timeout=5)[0] != 200:
                    raise RuntimeError("held request failed")
                for anthropic in (False, True):
                    if post(private_marker, anthropic)[0] != 200:
                        raise RuntimeError("cancellation latched a provider budget block")
                if rows(ledger) != [("settled",)] * 3 or upstream.posts != 3:
                    raise RuntimeError("successful requests were not settled once each")

                upstream.started.clear()
                upstream.release.clear()
                crashed = executor.submit(post, "hold " + private_marker)
                if not upstream.started.wait(5):
                    raise RuntimeError("crash fixture was not dispatched")
                process.kill()
                process.wait(timeout=5)
                try:
                    crashed.result(timeout=5)
                except OSError:
                    pass
                if ("pending",) not in rows(ledger):
                    raise RuntimeError("crash did not leave an abandoned reservation")
                start()
                if ("unknown",) not in rows(ledger):
                    raise RuntimeError("restart did not recover the abandoned attempt")
                status, data = post(private_marker)
                if status != 429 or b"budget_usage_unknown" not in data or upstream.posts != 4:
                    raise RuntimeError("genuine crash recovery did not stay fail-closed")
                process.send_signal(signal.SIGTERM)
                process.wait(timeout=10)
                if process.returncode != 0:
                    raise RuntimeError("gateway shutdown failed")
            persisted = (root / "stderr.log").read_text()
            with sqlite3.connect(ledger) as db:
                persisted += "\n".join(db.iterdump())
            if any(marker in persisted for marker in (private_marker, GATEWAY_KEY, PROVIDER_KEY)):
                raise RuntimeError("private fixture data persisted")
            print(json.dumps({"binary": str(binary), "reports_preserve_live_pending": True,
                              "queued_cancel_released_without_dispatch": True, "both_protocol_followups": True,
                              "second_gateway_excluded": True, "crash_recovery_unknown": True,
                              "unique_settlements": 3, "privacy": True}))
    finally:
        if process and process.poll() is None:
            process.kill()
            process.wait(timeout=5)
        upstream.release.set()
        upstream.shutdown()
        upstream.server_close()


if __name__ == "__main__":
    main()
