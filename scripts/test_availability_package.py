#!/usr/bin/env python3
"""Check unified routing state/query/CLI in an extracted Darwin package.

Uses fake Keychain credentials and loopback upstreams. Real cooldown recovery
is exercised by test_route_recovery_package.py; clocks/backoff/TTL by Go tests.
"""
import argparse
from datetime import datetime
from http.server import BaseHTTPRequestHandler
import json
import math
import re
from pathlib import Path
import sqlite3
import subprocess
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from test_client_reliability_package import running_gateway, GATEWAY_KEY, PROVIDER_KEY, BODY_MARKER, SESSION_MARKER


class Upstream(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def reply(self, status, value):
        data = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.reply(200, {"object": "list", "data": [{"id": "one"}, {"id": "two"}]})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        provider = self.path.split("/")[1]
        with self.server.lock:
            self.server.posts += 1
        if provider == "a" and body["model"] == "one":
            self.reply(404, {"error": {"code": "missing_model", "message": "Bearer private-upstream-error"}})
            return
        if self.path.endswith("/messages"):
            self.reply(200, {"id": "fixture", "type": "message", "role": "assistant", "model": body["model"],
                             "content": [{"type": "text", "text": "ok"}], "stop_reason": "end_turn",
                             "usage": {"input_tokens": 1, "output_tokens": 1}})
        else:
            self.reply(200, {"id": "fixture", "object": "chat.completion", "model": body["model"],
                             "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
                             "usage": {"prompt_tokens": 1, "completion_tokens": 1}})


def exchange(req):
    try:
        response = urlopen(req, timeout=10)
    except HTTPError as error:
        response = error
    with response:
        return response.status, json.load(response), response.headers


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    args = parser.parse_args()
    binary = args.binary.resolve()
    version = subprocess.check_output([str(binary), "version"], text=True).strip()
    results = []
    for protocol in ("openai", "anthropic"):
        for strategy in ("random", "price_priority"):
            for session in (None, SESSION_MARKER):
                def configure(config):
                    base = config["providers"][protocol]
                    config["providers"] = {ref: dict(base, base_url=base["base_url"].replace("/v1", "/"+ref+"/v1"),
                        supported_models=["one", "two"], upstream_id=ref,
                        error_code_mappings=[{"upstream_code": "missing_model", "http_status": 404, "category": "model_not_found"}],
                        prices={"one": {"currency": "USD", "source": "fixture", "version": "1",
                                         "input_cache_hit_per_million": cost, "input_cache_miss_per_million": cost, "output_per_million": cost}})
                        for ref, cost in (("a", 1), ("b", 2))}
                    config["routing"] = {"shared_model_strategy": strategy, "provider_switch_threshold": 0}
                    config["auto_chain"] = [{"provider": ref, "model": "one"} for ref in ("a", "b")]

                with running_gateway(binary, upstream_handler=Upstream, configure=configure) as gateway:
                    def call(model):
                        body = {"model": model, "max_tokens": 16, "messages": [{"role": "user", "content": BODY_MARKER}]}
                        headers = {"Authorization": "Bearer "+GATEWAY_KEY, "Content-Type": "application/json"}
                        if session:
                            headers["X-TideMux-Session-ID"] = session
                        endpoint = "/v1/messages" if protocol == "anthropic" else "/v1/chat/completions"
                        return exchange(Request(gateway["url"]+endpoint, json.dumps(body).encode(), headers, method="POST"))

                    if call("a/one")[0] != 404:
                        raise RuntimeError("First mapped failure was changed or replayed")
                    status, error, headers = call("a/one")
                    if status != 429 or "model_not_found_cooling_down" not in json.dumps(error) or not headers.get("Retry-After"):
                        raise RuntimeError("Explicit target did not explain cooldown")
                    detail = error["error"]
                    advice = detail.get("retry", {})
                    if headers.get("request-id") != error.get("request_id") or headers.get("x-request-id") != error.get("request_id"):
                        raise RuntimeError("SDK-standard correlation headers missing")
                    if (detail["type"] != "rate_limit_error" or advice.get("after_seconds") != int(headers["Retry-After"])
                            or advice.get("alternatives") != ["b/one"] or advice.get("reselect_provider")
                            or not advice.get("alternatives_require_model_change") or advice.get("max_attempts") != 3
                            or error.get("request_id") not in detail["message"]):
                        raise RuntimeError("Cooldown lost native retry, explicit routing or visible request ID")
                    for model in ("one", "auto", "a/two", "b/one"):
                        if call(model)[0] != 200:
                            raise RuntimeError("Failed route affected healthy target: "+model)
                    query = Request(gateway["url"]+"/tidemux/availability-status")
                    status, value, _ = exchange(query)
                    if status != 401 or "providers" in value:
                        raise RuntimeError("Unauthenticated availability disclosure")
                    query.add_header("Authorization", "Bearer "+GATEWAY_KEY)
                    status, value, headers = exchange(query)
                    provider = next(p for p in value["providers"] if p["ref"] == "a")
                    failed = next(m for m in provider["models"] if m["model"] == "one")
                    if status != 200 or provider["state"] != "available" or failed["state"] != "cooling" or failed["reason"] != "model_not_found" or failed["probe_due"]:
                        raise RuntimeError("Model and provider state were conflated")
                    if headers.get("X-TideMux-Version") != version or failed.get("scope") != "model" or failed.get("alternatives") != ["b/one"]:
                        raise RuntimeError("Version, health scope or model alternative missing")
                    cli = subprocess.run([str(binary), "gateway", "availability", "--json", "--config", str(gateway["config_file"])],
                                         env=gateway["env"], cwd=gateway["root"], capture_output=True, text=True, timeout=10)
                    def stable_states(snapshot):
                        def parse_time(value):
                            normalized = re.sub(r"\.(\d+)", lambda match: "." + match[1][:6].ljust(6, "0"), value)
                            return datetime.fromisoformat(normalized.replace("Z", "+00:00"))
                        observed = parse_time(snapshot["timestamp"])
                        for item in snapshot["providers"]:
                            for target in [item, *item["models"]]:
                                remaining = target.pop("retry_after_seconds", None)
                                if remaining is not None:
                                    deadline = parse_time(target["retry_at"])
                                    expected = math.ceil(max(0, (deadline-observed).total_seconds()))
                                    if abs(remaining-expected) > 1:
                                        raise RuntimeError("Availability countdown does not match its snapshot")
                        return snapshot["providers"]
                    if cli.returncode or stable_states(json.loads(cli.stdout)) != stable_states(value):
                        raise RuntimeError("CLI and endpoint disagree")
                    if gateway["upstream"].posts != 5:
                        raise RuntimeError("Cooling requests were dispatched")
                    with sqlite3.connect(gateway["ledger"]) as db:
                        if db.execute("SELECT count(*) FROM request_audit").fetchone()[0] != 5:
                            raise RuntimeError("Availability changed authoritative audits")
                    for private in (GATEWAY_KEY, PROVIDER_KEY, BODY_MARKER, SESSION_MARKER, "private-upstream-error", str(gateway["root"])):
                        if private in cli.stdout:
                            raise RuntimeError("Private data in shareable diagnostics")
                    results.append({"protocol": protocol, "strategy": strategy, "session": bool(session), "http_attempts": 5})
    print(json.dumps({"passed": True, "scenarios": results, "query_and_cli": True, "privacy": True,
                      "cooldown_retry_guidance": True, "version_header": version}))


if __name__ == "__main__":
    main()
