#!/usr/bin/env python3
"""Check bounded route recovery in a packaged binary with a real 30-second clock.

Uses isolated profiles, fake Keychain credentials and loopback providers only.
"""
import argparse
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from contextlib import ExitStack
from http.server import BaseHTTPRequestHandler
import json
from pathlib import Path
import sqlite3
import subprocess
import threading
import time
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from test_client_reliability_package import running_gateway, BODY_MARKER, SESSION_MARKER
from test_runtime_logs_package import GATEWAY_KEY, PROVIDER_KEY


class RecoveryUpstream(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def reply(self, status, body):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.reply(200, {"object": "list", "data": [{"id": "custom-model"}]})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        provider = self.path.split("/")[1]
        with self.server.lock:
            self.server.posts += 1
            self.server.routes.append(provider)
            failed = provider in self.server.failed
        if failed:
            self.reply(404, {"error": {"code": "missing_model", "message": "private-recovery-error"}})
            return
        if provider == "a" and self.server.hold:
            self.server.started.set()
            if not self.server.stop.wait(10):
                raise RuntimeError("recovery fixture was not released")
        if self.path.endswith("/messages"):
            self.reply(200, {"id": "msg_fixture", "type": "message", "role": "assistant", "model": body["model"],
                             "content": [{"type": "text", "text": provider}], "stop_reason": "end_turn",
                             "usage": {"input_tokens": 3, "output_tokens": 2}})
        else:
            self.reply(200, {"id": "chat_fixture", "object": "chat.completion", "model": body["model"],
                             "choices": [{"index": 0, "message": {"role": "assistant", "content": provider}, "finish_reason": "stop"}],
                             "usage": {"prompt_tokens": 3, "completion_tokens": 2}})


def request(gateway, protocol, mode, session):
    body = {"model": "auto" if mode == "auto" else "custom-model", "max_tokens": 32,
            "messages": [{"role": "user", "content": BODY_MARKER}]}
    headers = {"Authorization": "Bearer " + GATEWAY_KEY, "Content-Type": "application/json"}
    if session:
        headers["X-TideMux-Session-ID"] = SESSION_MARKER + session
    endpoint = "/v1/messages" if protocol == "anthropic" else "/v1/chat/completions"
    req = Request(gateway["url"] + endpoint, json.dumps(body).encode(), headers, method="POST")
    try:
        response = urlopen(req, timeout=15)
    except HTTPError as err:
        response = err
    with response:
        return response.status, json.loads(response.read()), response.headers.get("X-TideMux-Request-ID")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    args = parser.parse_args()
    binary = args.binary.resolve()
    if subprocess.check_output([str(binary), "version"], text=True).strip() != "0.3.2":
        raise RuntimeError("expected a packaged 0.3.2 candidate")
    scenarios = []
    with ExitStack() as stack:
        for protocol in ("openai", "anthropic"):
            for mode in ("auto", "shared"):
                for exhausted in (False, True):
                    def configure(config, protocol=protocol, mode=mode):
                        base = config["providers"][protocol]
                        config["providers"] = {
                            ref: dict(base, base_url=base["base_url"].replace("/v1", "/" + ref + "/v1"),
                                      upstream_id=ref, error_code_mappings=[{
                                          "upstream_code": "missing_model", "http_status": 404, "category": "model_not_found"}])
                            for ref in ("a", "b")}
                        config["max_in_flight"] = 16
                        config["routing"] = {"shared_model_strategy": "random"}
                        if mode == "auto":
                            config["auto_chain"] = [{"provider": ref, "model": "custom-model"} for ref in ("a", "b")]
                    gateway = stack.enter_context(running_gateway(binary, upstream_handler=RecoveryUpstream, configure=configure))
                    upstream = gateway["upstream"]
                    upstream.routes, upstream.failed, upstream.hold = [], {"a"}, False
                    if mode == "shared":
                        # Both keys are normally ready. Fail whichever provider
                        # the first request chooses, then keep that semantic route.
                        upstream.failed = {"a", "b"}
                    status, _, _ = request(gateway, protocol, mode, "-initial")
                    if status != 404 or upstream.posts != 1:
                        raise RuntimeError("initial failure was hidden or replayed")
                    failed_provider = upstream.routes[0]
                    healthy_provider = "b" if failed_provider == "a" else "a"
                    upstream.failed = {failed_provider, healthy_provider} if exhausted else {failed_provider}
                    status, _, _ = request(gateway, protocol, mode, "-healthy")
                    if status != (404 if exhausted else 200) or upstream.posts != 2 or upstream.routes[-1] != healthy_provider:
                        raise RuntimeError("failed route was immediately retried or fallback failed")
                    before = upstream.posts
                    if exhausted:
                        status, result, _ = request(gateway, protocol, mode, "-exhausted")
                        code = "auto_chain_exhausted" if mode == "auto" else "provider_keys_cooling_down"
                        if status != 503 or code not in json.dumps(result) or upstream.posts != before:
                            raise RuntimeError("exhausted routing sent unnecessary upstream requests")
                    scenarios.append((gateway, protocol, mode, exhausted, failed_provider, healthy_provider))
        # Every scenario must pass its actual cooldown, without a config reload,
        # process restart or test-only clock supplied to the packaged gateway.
        deadline = time.monotonic() + 30.2
        while time.monotonic() < deadline:
            time.sleep(min(1, deadline - time.monotonic()))
        for gateway, protocol, mode, exhausted, failed_provider, healthy_provider in scenarios:
            upstream = gateway["upstream"]
            upstream.failed = set()
            if exhausted:
                status, _, audit_id = request(gateway, protocol, mode, "-recovered")
                if status != 200 or not audit_id or upstream.posts != 3:
                    raise RuntimeError("exhausted routes did not recover after cooldown")
            elif mode == "auto":
                upstream.hold = True
                with ThreadPoolExecutor(max_workers=9) as pool:
                    probe = pool.submit(request, gateway, protocol, mode, "-probe")
                    if not upstream.started.wait(5):
                        upstream.stop.set()
                        raise RuntimeError("expired first auto entry did not receive a recovery probe")
                    try:
                        results = list(pool.map(lambda i: request(gateway, protocol, mode, "-concurrent-" + str(i)), range(8)))
                        if any(status != 200 for status, _, _ in results) or upstream.routes.count("a") != 2:
                            raise RuntimeError("more than one recovery probe was dispatched")
                    finally:
                        upstream.hold = False
                        upstream.stop.set()
                    if probe.result()[0] != 200:
                        raise RuntimeError("recovery probe did not succeed")
                request(gateway, protocol, mode, "-fresh")
                request(gateway, protocol, mode, "-healthy")
                request(gateway, protocol, mode, None)
                if upstream.routes[-3:] != ["a", "b", "a"]:
                    raise RuntimeError("configured preference or healthy/no-ID routing changed")
            else:
                # Random new bindings must eventually select the now eligible
                # route, while the established healthy binding remains pinned.
                for i in range(100):
                    if request(gateway, protocol, mode, "-fresh-" + str(i))[0] != 200:
                        raise RuntimeError("shared-model recovery request failed")
                    if upstream.routes[-1] == failed_provider:
                        break
                else:
                    raise RuntimeError("missing-model route remained permanently excluded")
                request(gateway, protocol, mode, "-healthy")
                if upstream.routes[-1] != healthy_provider:
                    raise RuntimeError("recovery moved an established healthy shared binding")
            with sqlite3.connect(gateway["ledger"]) as db:
                rows = [json.loads(row[0]) for row in db.execute("SELECT record_json FROM request_audit ORDER BY rowid")]
            if len(rows) != upstream.posts or Counter(row["provider_ref"] for row in rows) != Counter(upstream.routes):
                raise RuntimeError("audits do not match actual dispatches")
            logs = (gateway["root"] / "stderr").read_text() + json.dumps(rows)
            if any(marker in logs for marker in (GATEWAY_KEY, PROVIDER_KEY, BODY_MARKER, SESSION_MARKER, "private-recovery-error")):
                raise RuntimeError("routing recovery persisted private data")
    print(json.dumps({"binary": str(binary), "scenarios": len(scenarios), "real_cooldown_seconds": 30,
                      "exhausted_chain_and_shared_model_recover_without_reload": True,
                      "one_concurrent_recovery_probe": True, "healthy_bindings_retained": True,
                      "configured_auto_preference_and_anonymous_requests": True,
                      "no_gateway_replay": True, "audits_match_dispatches": True, "private_logs": True}))


if __name__ == "__main__":
    main()
