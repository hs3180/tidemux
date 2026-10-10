#!/usr/bin/env python3
"""Verify actual per-key attempts, failover, privacy and optional telemetry."""
import argparse
from http.server import BaseHTTPRequestHandler
import json
from pathlib import Path
import sqlite3
import subprocess
import time
from urllib.request import Request

from test_availability_package import exchange
from test_client_reliability_package import running_gateway, GATEWAY_KEY, BODY_MARKER, SESSION_MARKER

FIRST = "private-key-diagnostic-first"
SECOND = "private-key-diagnostic-second"


class Upstream(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def reply(self, status, value):
        data = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        if status == 429:
            self.send_header("Retry-After", "0")
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.reply(200, {"object": "list", "data": [{"id": "one"}]})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        key = self.headers.get("x-api-key", self.headers.get("Authorization", "").removeprefix("Bearer "))
        with self.server.lock:
            self.server.posts += 1
            n = self.server.posts
        if self.server.mode == "429" and n < 3:
            if key != FIRST:
                raise RuntimeError("Same-key retry switched credentials")
            self.reply(429, {"error": {"message": "private diagnostic upstream detail"}})
        elif self.server.mode == "auth" and key == FIRST:
            self.reply(401, {"error": {"message": "private diagnostic upstream detail"}})
        elif self.path.endswith("/messages"):
            self.reply(200, {"id": "fixture", "type": "message", "role": "assistant", "model": body["model"],
                             "content": [{"type": "text", "text": "ok"}], "stop_reason": "end_turn",
                             "usage": {"input_tokens": 1, "output_tokens": 1}})
        else:
            self.reply(200, {"id": "fixture", "object": "chat.completion", "model": body["model"],
                             "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
                             "usage": {"prompt_tokens": 1, "completion_tokens": 1}})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    binary = parser.parse_args().binary.resolve()
    scenarios = []
    for protocol in ("openai", "anthropic"):
        for mode in ("auth", "429"):
            for enabled in (False, True):
                def configure(config):
                    provider = config["providers"][protocol]
                    provider.pop("upstream_keychain")
                    provider["upstream_keychains"] = [{"service": "test.provider", "account": account} for account in ("key-one", "key-two")]
                    provider["supported_models"] = ["one"]
                    config["providers"] = {"a": provider}
                    config["health_diagnostics"] = enabled
                    security = Path(config["ledger_path"]).parent / "bin" / "security"
                    security.write_text("#!/bin/sh\ncase \"$*\" in *test.gateway*) printf '%s\\n' '" + GATEWAY_KEY +
                                        "';; *key-one*) printf '%s\\n' '" + FIRST + "';; *key-two*) printf '%s\\n' '" + SECOND + "';; *) exit 1;; esac\n")

                with running_gateway(binary, upstream_handler=Upstream, configure=configure) as gateway:
                    gateway["upstream"].mode = mode
                    headers = {"Authorization": "Bearer " + GATEWAY_KEY, "Content-Type": "application/json", "X-TideMux-Session-ID": SESSION_MARKER}
                    path = "/v1/messages" if protocol == "anthropic" else "/v1/chat/completions"
                    body = {"model": "a/one", "max_tokens": 16, "messages": [{"role": "user", "content": BODY_MARKER}]}
                    status, _, _ = exchange(Request(gateway["url"] + path, json.dumps(body).encode(), headers))
                    if status != 200 or gateway["upstream"].posts != (3 if mode == "429" else 2):
                        raise RuntimeError("Unexpected upstream attempts or result")
                    query = Request(gateway["url"] + "/tidemux/availability-status", headers=headers)
                    status, report, _ = exchange(query)
                    pool = report["providers"][0]["key_pool"]
                    if status != 200 or (pool["capacity"], pool["eligible"], pool["cooling"]) != (2, 1, 1):
                        raise RuntimeError("Pool state did not retain existing cooldown rules")
                    labels = [k["label"] for k in pool["keys"]]
                    if len(set(labels)) != 2 or any(not label.startswith("key-") for label in labels):
                        raise RuntimeError("Missing assigned key labels")
                    if enabled:
                        expected = dict(requests=1, http_attempts=3 if mode == "429" else 1, successes=1 if mode == "429" else 0,
                                        failures=2 if mode == "429" else 1, cancellations=0, timeouts=0, in_flight=0)
                        if pool["keys"][0]["counters"] != expected:
                            raise RuntimeError("Actual attempt counters disagree")
                        decisions = pool["recent_failovers"]
                        if mode == "429" and decisions:
                            raise RuntimeError("Same-key retries counted as failover")
                        if mode == "auth" and (len(decisions) != 1 or decisions[0]["from"] != labels[0] or decisions[0]["to"] != labels[1] or decisions[0]["reason"] != "authentication"):
                            raise RuntimeError("Actual failover diagnosis missing")
                    elif any("counters" in key for key in pool["keys"]) or pool["recent_failovers"]:
                        raise RuntimeError("Disabled telemetry retained optional observations")
                    cli = subprocess.run([str(binary), "gateway", "availability", "--json", "--config", str(gateway["config_file"])],
                                         env=gateway["env"], capture_output=True, text=True, timeout=10)
                    if cli.returncode or json.loads(cli.stdout)["providers"][0]["key_pool"] != pool:
                        raise RuntimeError("CLI did not expose the authenticated key snapshot")
                    for private in (FIRST, SECOND, GATEWAY_KEY, BODY_MARKER, SESSION_MARKER, "private diagnostic upstream detail", "key-one", "key-two", str(gateway["root"])):
                        if private in cli.stdout:
                            raise RuntimeError("Private data in key diagnostics")
                    with sqlite3.connect(gateway["ledger"]) as db:
                        if db.execute("SELECT count(*) FROM request_audit WHERE status='ok'").fetchone()[0] != 1:
                            raise RuntimeError("Diagnostics changed authoritative audits")
                    # CLI-only flag must apply without an interactive credential prompt.
                    toggle = subprocess.run([str(binary), "gateway", "configure", "--health-diagnostics=" + str(not enabled).lower(),
                                             "--config", str(gateway["config_file"])], env=gateway["env"], capture_output=True, text=True, timeout=10)
                    if toggle.returncode:
                        raise RuntimeError("Telemetry configuration required a prompt")
                    for _ in range(50):
                        _, new_report, _ = exchange(query)
                        changed = new_report["providers"][0]["key_pool"]
                        if changed["telemetry_enabled"] != enabled:
                            break
                        time.sleep(.1)
                    else:
                        raise RuntimeError("Telemetry reload did not apply")
                    if [k["label"] for k in changed["keys"]] != labels or changed["counter_epoch"] <= pool["counter_epoch"]:
                        raise RuntimeError("Telemetry toggle changed identities or retained the old epoch")
                    scenarios.append({"protocol": protocol, "mode": mode, "telemetry": enabled, "http_attempts": gateway["upstream"].posts})
    print(json.dumps({"passed": True, "scenarios": scenarios, "privacy": True, "audit_preserved": True, "telemetry_reload": True}))


if __name__ == "__main__":
    main()
