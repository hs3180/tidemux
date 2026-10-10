#!/usr/bin/env python3
"""Verify #128's GLM dual-output shape, usage and safe continuation in a package.

The sanitized fixture reproduces the issue's reported structure; no production
payload, real credential or paid upstream is used.
"""
import argparse
from http.server import BaseHTTPRequestHandler
import json
from pathlib import Path
import sqlite3
import subprocess
from urllib.request import Request, urlopen

from test_client_reliability_package import running_gateway, GATEWAY_KEY, PROVIDER_KEY

FIXTURES = Path(__file__).resolve().parents[1] / "internal/adapter/testdata"
MODEL = "claude-haiku-4-5"


class Fixture(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(json.dumps({"data": [{"id": MODEL, "type": "model"}]}).encode())

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        with self.server.lock:
            self.server.posts += 1
        variant = self.server.variant
        expected = fixture(variant)
        for message in body["messages"]:
            if message["role"] == "assistant" and isinstance(message.get("content"), list):
                if message["content"] != expected["content"]:
                    raise RuntimeError("native continuation content was changed")
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream" if body.get("stream") else "application/json")
        self.end_headers()
        if variant == "reported":
            suffix = "sse" if body.get("stream") else "json"
            payload = (FIXTURES / ("glm_dual_output." + suffix)).read_bytes()
        elif body.get("stream"):
            from test_client_recovery_package import frames
            events = frames(expected["content"])
            events[0][1]["message"]["usage"] = {**expected["usage"], "output_tokens": 0}
            payload = "".join("event: " + event + "\ndata: " + json.dumps(data) + "\n\n"
                              for event, data in events).encode()
        else:
            payload = json.dumps(expected).encode()
        self.wfile.write(payload)


def fixture(variant):
    response = json.loads((FIXTURES / "glm_dual_output.json").read_text())
    if variant == "unpaired":
        response["content"][2]["tool_use_id"] = "fixture-unpaired-result"
    elif variant == "no_server":
        response["content"] = response["content"][1:]
    return response


def configure(config):
    provider = config["providers"]["anthropic"]
    provider.update(supported_models=[MODEL],
                    prices={MODEL: {"currency": "USD", "source": "package-fixture", "version": "2026-10-09",
                                    "input_cache_hit_per_million": 1, "input_cache_miss_per_million": 2, "output_per_million": 3}},
                    budget={"currency": "USD", "five_hour_limit": 1, "weekly_limit": 10, "alert_threshold": 0.8, "mode": "hard"})
    config["providers"] = {"anthropic": provider}


def request(gateway, protocol, streaming, history=None):
    messages = [{"role": "user", "content": "private-glm-request"}]
    if history:
        messages = [history, {"role": "user", "content": "continue"}]
    body = {"model": "anthropic/" + MODEL, "max_tokens": 32, "stream": streaming, "messages": messages}
    path = "/v1/messages" if protocol == "anthropic" else "/v1/chat/completions"
    req = Request(gateway["url"] + path, json.dumps(body).encode(),
                  {"Authorization": "Bearer " + GATEWAY_KEY, "Content-Type": "application/json"}, method="POST")
    with urlopen(req, timeout=10) as response:
        return response.status, response.read().decode(), response.headers.get("X-TideMux-Request-ID")


def response_history(protocol, streaming, output):
    if not streaming:
        result = json.loads(output)
        return {"role": "assistant", "content": result["content"]} if protocol == "anthropic" else result["choices"][0]["message"]
    frames = [json.loads(line[6:]) for line in output.splitlines() if line.startswith("data: ") and line[6:] != "[DONE]"]
    if protocol == "openai":
        return {"role": "assistant", "content": "".join(frame["choices"][0]["delta"].get("content", "") for frame in frames)}
    blocks = []
    closed = []
    inputs = {}
    for frame in frames:
        if frame["type"] == "content_block_start":
            if frame["index"] != len(blocks):
                raise RuntimeError("native stream block indices changed")
            blocks.append(frame["content_block"])
        elif frame["type"] == "content_block_delta":
            if frame["delta"]["type"] == "input_json_delta":
                inputs[frame["index"]] = inputs.get(frame["index"], "") + frame["delta"]["partial_json"]
            else:
                blocks[frame["index"]]["text"] += frame["delta"]["text"]
        elif frame["type"] == "content_block_stop":
            if frame["index"] in inputs:
                blocks[frame["index"]]["input"] = json.loads(inputs[frame["index"]])
            closed.append(frame["index"])
    if closed != list(range(len(blocks))) or not any(frame["type"] == "message_stop" for frame in frames):
        raise RuntimeError("native stream was incomplete")
    return {"role": "assistant", "content": blocks}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    args = parser.parse_args()
    binary = args.binary.resolve()
    if subprocess.check_output([str(binary), "version"], text=True).strip() != "0.3.2":
        raise RuntimeError("expected a packaged 0.3.2 candidate")
    cases = 0
    with running_gateway(binary, upstream_handler=Fixture, configure=configure) as gateway:
        for variant in ("reported", "unpaired", "no_server"):
            gateway["upstream"].variant = variant
            for protocol in ("anthropic", "openai"):
                for streaming in (False, True):
                    before = gateway["upstream"].posts
                    status, output, request_id = request(gateway, protocol, streaming)
                    if status != 200 or not request_id or "GLM_WEB_READER_OUTPUT" not in output or "GLM_DUAL_OUTPUT_OK" not in output or "event: error" in output:
                        raise RuntimeError("GLM output was lost or aborted")
                    history = response_history(protocol, streaming, output)
                    if protocol == "anthropic":
                        if history["content"] != fixture(variant)["content"]:
                            raise RuntimeError("native server-tool blocks were changed or removed")
                    elif '"tool_result"' in output or '"server_tool_use"' in output:
                        raise RuntimeError("server tool was exposed as a client tool")
                    if request(gateway, protocol, False, history)[0] != 200:
                        raise RuntimeError("GLM history could not continue")
                    if gateway["upstream"].posts - before != 2:
                        raise RuntimeError("gateway replayed a GLM request")
                    cases += 1
        with sqlite3.connect(gateway["ledger"]) as db:
            rows = [json.loads(row[0]) for row in db.execute("SELECT record_json FROM request_audit")]
            pending, = db.execute("SELECT COUNT(*) FROM budget_charges WHERE state!='settled'").fetchone()
        logs = (gateway["root"] / "stderr").read_text()
        events = [json.loads(line) for line in logs.splitlines() if line.strip()]
        warnings = [event for event in events if event.get("event") == "protocol_conversion_omitted_fields"]
        if len(rows) != gateway["upstream"].posts or len(rows) != 24 or pending:
            raise RuntimeError("GLM dispatches or budget settlement disagree")
        if sorted(event.get("field_count") for event in warnings) != [1] * 4 + [2] * 8:
            raise RuntimeError("conversion limits were not reported")
        if any(event.get("event") == "upstream_tool_result_suppressed" for event in events):
            raise RuntimeError("native results were suppressed")
        for row in rows:
            if row.get("status") != "ok" or row.get("input_tokens") != 10 or row.get("output_tokens") != 10 or abs(row.get("estimated_cost", 0) - 0.00005) > 1e-12:
                raise RuntimeError("compatibility handling changed usage or budget charge")
        private = logs + json.dumps(rows)
        if any(marker in private for marker in ("GLM_WEB_READER_OUTPUT", "fixture-web-reader", "fixture-unpaired-result", "private-glm-request", GATEWAY_KEY, PROVIDER_KEY)):
            raise RuntimeError("tool content, IDs or credentials leaked into gateway logs or audits")
    print(json.dumps({"binary": str(binary), "cases": cases, "native_and_converted_buffered_and_streaming": True,
                      "preserved_output_and_valid_continuation": True, "no_gateway_replay": True,
                      "native_blocks_preserved": True, "unpaired_and_no_server_variants": True, "no_provider_policy": True,
                      "usage_and_budget_settlement": True, "private_conversion_warnings": True}))


if __name__ == "__main__":
    main()
