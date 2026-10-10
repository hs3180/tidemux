#!/usr/bin/env python3
"""Verify native content boundaries and tool continuation in a packaged binary.

Loopback fixtures cover opaque web/image results, structural failures, protocol
conversion, compaction and client/server tools. No real credentials used.
"""
import argparse
from http.server import BaseHTTPRequestHandler
import json
from pathlib import Path
import sqlite3

from test_client_reliability_package import running_gateway
from test_anthropic_server_tools_package import post

PRIVATE = "tool-history-private-content"
PRIVATE_ID = "tool-history-private-id"
SAFE_PREFIX = "safe text before malformed tool block"


class ToolUpstream(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def reply(self, body):
        payload = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        self.reply({"object": "list", "data": [{"id": "custom-model", "type": "model"}], "has_more": False})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        with self.server.lock:
            self.server.posts += 1
        messages = body["messages"]
        first = str(messages[0].get("content", ""))
        generic = {"type": "tool_result", "tool_use_id": PRIVATE_ID,
                   "content": [{"type": "text", "text": PRIVATE}]}
        if "image" in first:
            generic["content"].append({"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "aGVsbG8="}})
        if "malformed" in first:
            generic["type"] = 17  # Invalid tagged-object envelope, not a tool-role defect.
            if body.get("stream"):
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                index = 1 if "prefix" in first else 0
                frames = [
                    ("message_start", {"type": "message_start", "message": {"id": "msg_test", "role": "assistant", "type": "message", "model": "custom-model", "content": [], "usage": {"input_tokens": 2}}}),
                ]
                if index:
                    frames.extend([
                        ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
                        ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": SAFE_PREFIX}}),
                        ("content_block_stop", {"type": "content_block_stop", "index": 0}),
                    ])
                frames.extend([
                    ("content_block_start", {"type": "content_block_start", "index": index, "content_block": generic}),
                    ("content_block_stop", {"type": "content_block_stop", "index": index}),
                    ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn"}, "usage": {"output_tokens": 1}}),
                    ("message_stop", {"type": "message_stop"}),
                ])
                for event, data in frames:
                    self.wfile.write(("event: " + event + "\ndata: " + json.dumps(data) + "\n\n").encode())
                self.wfile.flush()
                return
            content = [generic]
        elif len(messages) == 1 and "generic" in first:
            content = [{"type": "text", "text": SAFE_PREFIX}, generic]
            if body.get("stream"):
                from test_client_recovery_package import frames
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                for event, data in frames(content):
                    self.wfile.write(("event: " + event + "\ndata: " + json.dumps(data) + "\n\n").encode())
                self.wfile.flush()
                return
        elif len(messages) == 1 and "server" in first:
            content = [{"type": "server_tool_use", "id": PRIVATE_ID, "name": "webReader", "input": {"url": "https://example.com"}},
                       {"type": "webReader_tool_result", "tool_use_id": PRIVATE_ID, "content": {"opaque": PRIVATE}},
                       {"type": "analyze_image_tool_result", "tool_use_id": PRIVATE_ID, "content": {"opaque": PRIVATE}}]
        elif len(messages) == 1 and "client" in first:
            content = [{"type": "tool_use", "id": PRIVATE_ID, "name": "read_file", "input": {}}]
        else:
            if len(messages) > 1 and PRIVATE not in json.dumps(messages):
                raise RuntimeError("tool content was lost in follow-up")
            content = [{"type": "text", "text": "continued"}]
        if self.path.endswith("/messages"):
            self.reply({"id": "msg_test", "type": "message", "role": "assistant", "model": "custom-model", "content": content,
                        "stop_reason": "tool_use" if content[0]["type"] == "tool_use" else "end_turn", "usage": {"input_tokens": 2, "output_tokens": 1}})
        else:
            message = {"role": "assistant", "content": "continued"}
            if content[0]["type"] == "tool_use":
                message.update(content=None, tool_calls=[{"id": PRIVATE_ID, "type": "function", "function": {"name": "read_file", "arguments": "{}"}}])
            self.reply({"id": "chat_test", "object": "chat.completion", "model": "custom-model", "choices": [{"index": 0, "message": message, "finish_reason": "tool_calls" if "tool_calls" in message else "stop"}], "usage": {"prompt_tokens": 2, "completion_tokens": 1}})


def request_body(provider, text, stream=False):
    return {"model": provider + "/custom-model", "max_tokens": 64, "stream": stream,
            "messages": [{"role": "user", "content": text}]}


def failure(data, code):
    if data.get("code") != code:
        raise RuntimeError("missing safe structural failure: " + json.dumps(data))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    args = parser.parse_args()
    cases = 0
    with running_gateway(args.binary.resolve(), upstream_handler=ToolUpstream) as gateway:
        for native in (True, False):
            for tool in ("web", "image"):
                for stream in (True, False):
                    before = gateway["upstream"].posts
                    status, body = post(gateway["url"], request_body("anthropic", "generic-" + tool, stream), anthropic=native)
                    terminal = "message_stop" if native else "[DONE]"
                    if status != 200 or SAFE_PREFIX not in body or (stream and terminal not in body) or '"error"' in body:
                        raise RuntimeError("generic provider result aborted available output")
                    if native != (PRIVATE in body and PRIVATE_ID in body):
                        raise RuntimeError("native result lost or provider content leaked through conversion")
                    if gateway["upstream"].posts != before + 1:
                        raise RuntimeError("generic result caused gateway replay")
                    cases += 1
        for tool in ("web", "image"):
            for stream in (True, False):
                before = gateway["upstream"].posts
                status, body = post(gateway["url"], request_body("anthropic", "malformed-" + tool, stream))
                if stream:
                    errors = [json.loads(line[6:])["error"] for line in body.splitlines() if line.startswith("data: ") and '"error"' in line]
                    if status != 200 or len(errors) != 1 or "message_stop" in body:
                        raise RuntimeError("malformed stream was not terminated safely")
                    failure(errors[0], "invalid_upstream_stream")
                else:
                    if status != 502:
                        raise RuntimeError("malformed response was admitted")
                    failure(json.loads(body)["error"], "invalid_upstream_response")
                if PRIVATE in body or PRIVATE_ID in body or gateway["upstream"].posts != before + 1:
                    raise RuntimeError("malformed content escaped or upstream was retried")
                cases += 1
        for anthropic_client in (True, False):
            for tool in ("web", "image"):
                before = gateway["upstream"].posts
                status, body = post(gateway["url"], request_body("anthropic", "malformed-prefix-" + tool, True), anthropic=anthropic_client)
                errors = [json.loads(line[6:])["error"] for line in body.splitlines() if line.startswith("data: ") and '"error"' in line]
                if status != 200 or SAFE_PREFIX not in body or len(errors) != 1 or "message_stop" in body or "[DONE]" in body:
                    raise RuntimeError("native or converted stream lost legal prefix or emitted false success")
                failure(errors[0], "invalid_upstream_stream")
                if PRIVATE in body or PRIVATE_ID in body or gateway["upstream"].posts != before + 1:
                    raise RuntimeError("malformed block after legal output escaped or was replayed")
                cases += 1
        for provider in ("anthropic", "openai"):
            for compacted in (False, True):
                before = gateway["upstream"].posts
                history = request_body(provider, "continue")
                prefix = [{"role": "assistant", "content": [{"type": "compaction", "content": "summary"}]}] if compacted else [{"role": "user", "content": "earlier"}]
                history["messages"] = prefix + [{"role": "assistant", "content": [{"type": "text", "text": "earlier"}, {"type": "tool_result", "tool_use_id": PRIVATE_ID, "content": PRIVATE}]}, {"role": "user", "content": "continue"}]
                for _ in range(2):
                    status, body = post(gateway["url"], history)
                    if provider == "anthropic":
                        if status != 200 or "continued" not in body:
                            raise RuntimeError("native generic history was rejected")
                    else:
                        if status != 400:
                            raise RuntimeError("unsupported history was misrepresented during conversion")
                        failure(json.loads(body)["error"], "unsupported_request_feature")
                    if PRIVATE in body or PRIVATE_ID in body:
                        raise RuntimeError("history content leaked in a diagnostic")
                expected = before + (2 if provider == "anthropic" else 0)
                if gateway["upstream"].posts != expected:
                    raise RuntimeError("native history was blocked or unsupported conversion dispatched")
                cases += 1
        for scene, providers in (("server", ("anthropic",)), ("client", ("anthropic", "openai"))):
            for provider in providers:
                request = request_body(provider, scene)
                if scene == "client":
                    request["tools"] = [{"name": "read_file", "input_schema": {"type": "object"}}]
                status, body = post(gateway["url"], request)
                if status != 200:
                    raise RuntimeError("valid tool call rejected")
                answer = json.loads(body)
                if scene == "server" and (answer["content"][1]["content"] != {"opaque": PRIVATE} or answer["content"][2]["type"] != "analyze_image_tool_result"):
                    raise RuntimeError("valid provider server blocks changed")
                request["messages"].append({"role": "assistant", "content": answer["content"]})
                if scene == "client":
                    request["messages"].append({"role": "user", "content": [{"type": "tool_result", "tool_use_id": PRIVATE_ID, "content": [{"type": "text", "text": PRIVATE}]}]})
                else:
                    request["messages"].append({"role": "user", "content": "continue"})
                    request["context_management"] = {"edits": [{"type": "compact_20260112"}]}
                status, body = post(gateway["url"], request)
                if status != 200 or "continued" not in body:
                    raise RuntimeError("valid tool follow-up failed")
                cases += 1
        with sqlite3.connect(gateway["ledger"]) as db:
            persisted = "\n".join(db.iterdump())
        persisted += (gateway["root"] / "stderr").read_text()
        if PRIVATE in persisted or PRIVATE_ID in persisted:
            raise RuntimeError("tool content or IDs leaked into ledger/logs")
    print(json.dumps({"binary": str(args.binary), "cases": cases, "native_and_conversion_history_paths": True,
                      "native_generic_web_and_image_results_preserved": True, "invalid_content_envelopes_withheld": True, "stream_error_after_http_200": True,
                      "native_and_converted_legal_prefix_before_error": True,
                      "native_compacted_history_preserved": True, "unsupported_converted_history_not_dispatched": True, "valid_server_and_client_tools_preserved": True,
                      "tool_roundtrip_and_compaction": True, "privacy": True}))


if __name__ == "__main__":
    main()
