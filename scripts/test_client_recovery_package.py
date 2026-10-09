#!/usr/bin/env python3
"""Run installed Claude Code, Kilo and Hermes against an extracted binary.

The clients and their tools are real; only the provider is a scripted loopback
fixture. All credentials, profiles, workspaces and tool inputs are disposable.
Nothing reads a user's TideMux config, client history or Keychain credentials.
"""
import argparse
import hashlib
import http.client
from http.server import BaseHTTPRequestHandler
import json
import os
from pathlib import Path
import shutil
import signal
import sqlite3
import subprocess
import threading
import time

from test_client_reliability_package import GATEWAY_KEY, PROVIDER_KEY, running_gateway
from test_context_management_claude import Server, sse

MODEL = "claude-haiku-4-5"
PREFIX = "SAFE_PREFIX_BEFORE_TOOL_ERROR"
PRIVATE = "client-recovery-private-tool-payload"
PRIVATE_ID = "client-recovery-private-tool-id"
READ_MARKER = "REAL_CLIENT_READ_OK"
DONE = "REAL_CLIENT_WORKFLOW_OK"
CONTINUED = "REAL_CLIENT_CONTINUATION_OK"
CAPACITY_READY = "REAL_CLIENT_CAPACITY_RECOVERED"
GLM_DONE = "GLM_DUAL_OUTPUT_OK"
GLM_FIXTURE = Path(__file__).resolve().parents[1] / "internal/adapter/testdata/glm_dual_output.json"


def frames(content, stop="end_turn"):
    events = [("message_start", {"type": "message_start", "message": {
        "id": "msg_fixture", "type": "message", "role": "assistant", "model": MODEL,
        "content": [], "stop_reason": None, "stop_sequence": None,
        "usage": {"input_tokens": 10, "output_tokens": 0}}})]
    for index, block in enumerate(content):
        if block["type"] == "text":
            events.extend([
                ("content_block_start", {"type": "content_block_start", "index": index,
                                         "content_block": {"type": "text", "text": ""}}),
                ("content_block_delta", {"type": "content_block_delta", "index": index,
                                         "delta": {"type": "text_delta", "text": block["text"]}}),
            ])
        elif block["type"] in ("tool_use", "server_tool_use"):
            events.extend([
                ("content_block_start", {"type": "content_block_start", "index": index,
                    "content_block": {"type": block["type"], "id": block["id"], "name": block["name"], "input": {}}}),
                ("content_block_delta", {"type": "content_block_delta", "index": index,
                    "delta": {"type": "input_json_delta", "partial_json": json.dumps(block["input"])}}),
            ])
        else:
            events.append(("content_block_start", {"type": "content_block_start", "index": index,
                                                    "content_block": block}))
        events.append(("content_block_stop", {"type": "content_block_stop", "index": index}))
    events.extend([
        ("message_delta", {"type": "message_delta", "delta": {"stop_reason": stop, "stop_sequence": None},
                           "usage": {"output_tokens": 10}}),
        ("message_stop", {"type": "message_stop"}),
    ])
    return events


class Upstream(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        self.reply("application/json", json.dumps({"data": [{"id": MODEL, "type": "model"}], "has_more": False}).encode())

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
        with self.server.lock:
            self.server.posts += 1
            self.server.requests.append(body)
            scene = self.server.scene
        if scene == "capacity":
            if "CAPACITY_HOLD" in json.dumps(body["messages"]):
                self.server.capacity_started.set()
                self.server.capacity_release.wait(30)
            self.respond(body, [{"type": "text", "text": CAPACITY_READY}])
            return
        if scene == "glm_dual":
            content = json.loads(GLM_FIXTURE.read_text())["content"]
        elif scene == "glm_continue":
            if GLM_DONE not in json.dumps(body["messages"]):
                self.server.failures.append("GLM continuation omitted preserved text")
            for message in body["messages"]:
                if message.get("role") == "assistant" and isinstance(message.get("content"), list):
                    server_tools = set()
                    for block in message["content"]:
                        if block is None:
                            self.server.failures.append("GLM continuation has a missing content block")
                            continue
                        if block.get("type") == "server_tool_use":
                            server_tools.add(block["id"])
                        if block.get("type") == "tool_result" and block.get("tool_use_id") not in server_tools:
                            self.server.failures.append("GLM continuation has an unpaired result")
            content = [{"type": "text", "text": "GLM_CONTINUATION_OK"}]
        elif scene == "malformed":
            content = [{"type": "text", "text": PREFIX}, {"type": "tool_result", "tool_use_id": PRIVATE_ID,
                        "content": [{"type": "text", "text": PRIVATE}]}]
        elif scene == "continue":
            if DONE not in json.dumps(body["messages"]):
                self.server.failures.append("continuation omitted prior assistant history")
            content = [{"type": "text", "text": CONTINUED}]
        elif scene == "workflow":
            if not body.get("tools"):
                # Hermes can issue a JSON-schema planning request before its
                # tool turn. This is a separate auxiliary client request.
                self.respond(body, [{"type": "text", "text": "{}"}])
                return
            content, stop = workflow_response(self.server, body)
            self.respond(body, content, stop)
            return
        else:
            content = [{"type": "text", "text": "PROBE_OK"}]
        self.respond(body, content)

    def respond(self, body, content, stop="end_turn"):
        if body.get("stream"):
            # Flush the complete legal text block before producing the bad block.
            # The ingress observer forwards each chunk immediately to the CLI.
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            for name, value in frames(content, stop):
                try:
                    self.wfile.write(sse([(name, value)]))
                    self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError):
                    return
        else:
            self.reply("application/json", json.dumps({"id": "msg_fixture", "type": "message", "role": "assistant",
                "model": MODEL, "content": content, "stop_reason": stop,
                "usage": {"input_tokens": 10, "output_tokens": 10}}).encode())

    def reply(self, content_type, payload):
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def workflow_response(server, body):
    tools = {tool["name"]: tool for tool in body.get("tools", [])}
    server.tools = tools
    step = server.workflow_step
    server.workflow_step += 1
    if step == 0 and PREFIX in json.dumps(body["messages"]):
        server.failures.append("new conversation retained the failed conversation's partial answer")
    if step in (0, 1):
        name = next((n for n in ("Read", "read", "read_file") if n in tools), None)
        file = "fixture.txt" if step == 0 else "calc.py"
        inputs = {"Read": {"file_path": str(server.work / file)},
                  "read": {"filePath": str(server.work / file)},
                  "read_file": {"path": str(server.work / file)}}
    elif step == 2:
        if READ_MARKER not in json.dumps(body["messages"]):
            server.failures.append("read tool did not return fixture contents")
        name = next((n for n in ("Edit", "edit", "patch", "write_file") if n in tools), None)
        inputs = {"Edit": {"file_path": str(server.work / "calc.py"), "old_string": "return a - b", "new_string": "return a + b"},
                  "edit": {"filePath": str(server.work / "calc.py"), "oldString": "return a - b", "newString": "return a + b"},
                  "patch": {"path": str(server.work / "calc.py"), "old_string": "return a - b", "new_string": "return a + b"},
                  "write_file": {"path": str(server.work / "calc.py"), "content": "def add(a, b):\n    return a + b\n"}}
    elif step == 3:
        name = next((n for n in ("Bash", "bash", "terminal") if n in tools), None)
        inputs = {"Bash": {"command": "python3 -m unittest -v", "description": "Run the isolated fixture tests"},
                  "bash": {"command": "python3 -m unittest -v", "description": "Run the isolated fixture tests"},
                  "terminal": {"command": "python3 -m unittest -v"}}
    else:
        last_tool_result = json.dumps(body["messages"][-1])
        if "\\nOK" not in last_tool_result or "Ran 1 test" not in last_tool_result:
            server.failures.append("test tool did not report passing unittest")
        return [{"type": "text", "text": DONE}], "end_turn"
    if name is None:
        server.failures.append("required tool absent: " + str(sorted(tools)))
        return [{"type": "text", "text": "FIXTURE_TOOL_MISSING"}], "end_turn"
    return [{"type": "tool_use", "id": f"tool_fixture_{step}", "name": name, "input": inputs[name]}], "tool_use"


class Observer(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        self.forward()

    def do_POST(self):
        self.forward()

    def forward(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        record = {"path": self.path, "body": json.loads(raw) if raw else None}
        if self.command == "POST":
            self.server.requests.append(record)
        conn = http.client.HTTPConnection("127.0.0.1", self.server.gateway_port, timeout=45)
        headers = {key: value for key, value in self.headers.items()
                   if key.lower() not in {"host", "connection", "content-length", "transfer-encoding"}}
        try:
            conn.request(self.command, self.path, body=raw or None, headers=headers)
            response = conn.getresponse()
            record["status"] = response.status
            record["retry_after"] = response.getheader("Retry-After")
            record["request_id"] = response.getheader("X-TideMux-Request-ID")
            if response.status == 429 and hasattr(self.server, "capacity_release"):
                self.server.capacity_release.set()
            self.send_response(response.status)
            self.send_header("Content-Type", response.getheader("Content-Type", "application/json"))
            self.end_headers()
            chunks = []
            while True:
                chunk = response.read1(4096)
                if not chunk:
                    break
                chunks.append(chunk)
                self.wfile.write(chunk)
                self.wfile.flush()
            record["response"] = b"".join(chunks).decode()
        except (BrokenPipeError, ConnectionResetError):
            record["client_closed"] = True
        finally:
            conn.close()


def environment(gateway):
    root = gateway["root"]
    env = dict(gateway["env"])
    env.update({"PATH": str(root / "bin") + os.pathsep + os.environ.get("PATH", env["PATH"]),
        "XDG_CONFIG_HOME": str(root / "xdg-config"), "XDG_DATA_HOME": str(root / "xdg-data"),
        "XDG_CACHE_HOME": str(root / "xdg-cache"), "XDG_STATE_HOME": str(root / "xdg-state"),
        "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_AUTOUPDATER": "1",
        "DISABLE_TELEMETRY": "1", "DISABLE_ERROR_REPORTING": "1",
        "KILO_DISABLE_AUTOUPDATE": "1", "OPENCODE_DISABLE_AUTOUPDATE": "1"})
    return env


def client_arguments(client, prompt, continuing=False):
    if client == "claude":
        return ["-p", prompt, "--output-format", "stream-json", "--verbose", "--tools", "Read,Edit,Bash",
                "--allowedTools", "Read,Edit,Bash(python3 -m unittest*)", "--permission-mode", "dontAsk"] + (["--continue"] if continuing else [])
    if client == "kilo":
        return ["run", "--pure", "--format", "json", "--title", "TideMux local recovery fixture"] + (["--continue"] if continuing else []) + [prompt]
    return ["-q", prompt, "-Q", "--oneshot", "--ignore-rules", "-t", "file,terminal",
            "--max-turns", "8", "--run-budget", "40"] + (["--continue"] if continuing else [])


def execute(binary, client, executable, config, gateway, prompt, continuing=False):
    env = environment(gateway)
    if client == "kilo":
        env["KILO_CONFIG_CONTENT"] = json.dumps({"permission": {"*": "deny", "read": "allow", "edit": "allow",
            "external_directory": {str(gateway["work"]) + "/*": "allow"},
            "bash": {"*": "deny", "python3 -m unittest*": "allow"}}})
    command = [str(binary), client, "--config", str(config), "--model", "anthropic/" + MODEL,
               "--executable", str(executable), "--"] + client_arguments(client, prompt, continuing)
    process = subprocess.Popen(command, cwd=gateway["work"], env=env, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, start_new_session=True)
    timed_out = False
    try:
        stdout, stderr = process.communicate(timeout=60)
    except subprocess.TimeoutExpired:
        timed_out = True
        os.killpg(process.pid, signal.SIGTERM)
        try:
            stdout, stderr = process.communicate(timeout=5)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            stdout, stderr = process.communicate()
    return {"command": command, "exit_code": process.returncode, "timed_out": timed_out,
            "stdout": stdout.decode(errors="replace"), "stderr": stderr.decode(errors="replace")}


def configure(config):
    config["providers"] = {"anthropic": config["providers"]["anthropic"]}
    config["providers"]["anthropic"]["supported_models"] = [MODEL]
    config["providers"]["anthropic"]["model_capabilities"] = {"context_tokens": 128000, "max_output_tokens": 4096}


def check_client(binary, client, executable, evidence):
    with running_gateway(binary, upstream_handler=Upstream, configure=configure) as gateway:
        root, upstream = gateway["root"], gateway["upstream"]
        upstream.requests, upstream.failures = [], []
        upstream.scene, upstream.workflow_step = "malformed", 0
        work = root / "workspace"
        work.mkdir()
        work = work.resolve()
        gateway["work"] = upstream.work = work
        (work / "fixture.txt").write_text(READ_MARKER + "\n")
        (work / "calc.py").write_text("def add(a, b):\n    return a - b\n")
        test = "import unittest\nfrom calc import add\nclass CalcTest(unittest.TestCase):\n    def test_add(self):\n        self.assertEqual(add(2, 3), 5)\n"
        (work / "test_calc.py").write_text(test)
        observer = Server(("127.0.0.1", 0), Observer)
        observer.gateway_port = int(gateway["url"].rsplit(":", 1)[1])
        observer.requests = []
        threading.Thread(target=observer.serve_forever, daemon=True).start()
        launch_config = dict(gateway["config"])
        launch_config["listen_addr"] = f"127.0.0.1:{observer.server_port}"
        path = root / "launch.json"
        path.write_text(json.dumps(launch_config))
        version = subprocess.run([str(executable), "--version"], env=environment(gateway),
                                 capture_output=True, text=True, timeout=30)
        if version.returncode != 0 or not version.stdout.strip():
            raise RuntimeError(client + ": cannot determine installed version")
        result = {"client": client, "version": version.stdout.strip().splitlines()[0],
                  "client_api": "anthropic" if client == "claude" else "openai",
                  "provider_api": "anthropic", "cli_mode": "noninteractive"}
        try:
            upstream.scene = "glm_dual"
            result["glm_dual"] = execute(binary, client, executable, path, gateway, "Reply to the local GLM dual-output fixture.")
            result["glm_upstream_calls"] = upstream.posts
            result["glm_wire"] = [{key: value for key, value in item.items() if key != "body"} for item in observer.requests]
            if result["glm_dual"]["exit_code"] != 0 or result["glm_dual"]["timed_out"] or GLM_DONE not in result["glm_dual"]["stdout"]:
                save(evidence, client, result, root)
                raise RuntimeError(client + ": duplicate GLM tool result did not complete")
            if client == "claude" and (upstream.posts != 1 or len(observer.requests) != 1):
                raise RuntimeError("Claude retried the native GLM turn")
            if any(item.get("status") != 200 or "event: error" in item.get("response", "") or
                   (client != "claude" and '"tool_result"' in item.get("response", "")) for item in observer.requests):
                raise RuntimeError(client + ": GLM stream failed or a server tool was exposed as a client tool")
            if client == "claude" and not any('"tool_result"' in item.get("response", "") and '"server_tool_use"' in item.get("response", "") for item in observer.requests):
                raise RuntimeError("native Claude route removed server-tool blocks")
            before = upstream.posts
            upstream.scene = "glm_continue"
            result["glm_continuation"] = execute(binary, client, executable, path, gateway, "Continue the prior GLM fixture conversation.", continuing=True)
            if result["glm_continuation"]["exit_code"] != 0 or result["glm_continuation"]["timed_out"] or "GLM_CONTINUATION_OK" not in result["glm_continuation"]["stdout"] or upstream.failures:
                save(evidence, client, result, root)
                raise RuntimeError(client + ": GLM continuation was poisoned")
            if client == "claude" and upstream.posts != before+1:
                raise RuntimeError("Claude retried the GLM continuation")
            result["glm_single_claude_request"] = client == "claude" and before == 1 and upstream.posts == 2
            result["glm_compatible_completion_and_continuation"] = True
            observer.requests.clear()
            before_malformed = upstream.posts
            upstream.scene = "malformed"
            result["malformed"] = execute(binary, client, executable, path, gateway, "Reply to this local recovery check.")
            bad_requests = list(observer.requests)
            result["malformed_upstream_calls"] = upstream.posts - before_malformed
            result["malformed_wire"] = [{key: value for key, value in request.items() if key != "body"} for request in bad_requests]
            result["tool_schemas"] = []
            for request in upstream.requests:
                result["tool_schemas"] = request.get("tools", []) or result["tool_schemas"]
            # Save partial results even when a supported-client behavior changes.
            save(evidence, client, result, root)
            if result["malformed"]["timed_out"] or not bad_requests:
                raise RuntimeError(client + ": malformed stream did not finish")
            streaming_requests = 0
            for request in bad_requests:
                wire = request.get("response", "")
                if request["body"].get("stream"):
                    streaming_requests += 1
                    errors = [json.loads(line[6:])["error"] for line in wire.splitlines()
                              if line.startswith("data: ") and '"error"' in line]
                    if request["status"] != 200 or PREFIX not in wire or len(errors) != 1:
                        raise RuntimeError(client + ": legal prefix/terminal error missing from wire")
                    payloads = [json.loads(line[6:]) for line in wire.splitlines() if line.startswith("data: ") and '"error"' in line]
                    if not request.get("request_id") or payloads[0].get("request_id") != request["request_id"]:
                        raise RuntimeError(client + ": terminal SSE error has no correlated request ID")
                else:
                    errors = [json.loads(wire).get("error", {})]
                    if request["status"] != 502:
                        raise RuntimeError(client + ": malformed non-streaming auxiliary response admitted")
                error = errors[0]
                if error.get("code") != "invalid_upstream_tool_history" or error.get("param") != "content[1].type" or error.get("recovery", {}).get("retryable") is not False:
                    raise RuntimeError(client + ": safe recovery diagnostic missing")
                if PRIVATE in wire or PRIVATE_ID in wire or "message_stop" in wire or "[DONE]" in wire:
                    raise RuntimeError(client + ": malformed block escaped or false success emitted")
            if not streaming_requests:
                raise RuntimeError(client + ": no real client streaming request observed")
            result["malformed_client_streaming_requests"] = streaming_requests
            result["malformed_client_auxiliary_requests"] = len(bad_requests) - streaming_requests
            visible = result["malformed"]["stdout"] + result["malformed"]["stderr"]
            result["client_visible_safe_prefix"] = PREFIX in visible
            result["client_visible_error_code"] = "invalid_upstream_tool_history" in visible
            result["client_visible_recovery_text"] = "Start a clean conversation" in visible
            if upstream.posts - before_malformed != len(bad_requests):
                raise RuntimeError(client + ": a dispatched client request was replayed by the gateway")
            if PRIVATE in json.dumps(result) or PRIVATE_ID in json.dumps(result):
                raise RuntimeError(client + ": malformed payload reached client output")
            for marker in (GATEWAY_KEY, PROVIDER_KEY):
                if marker in visible:
                    raise RuntimeError(client + ": synthetic credential reached client output")
            upstream.scene = "workflow"
            result["clean_conversation"] = execute(binary, client, executable, path, gateway,
                "Read fixture.txt. Fix add in calc.py without changing test_calc.py. Run python3 -m unittest -v. Work only in this fixture folder.")
            result["tool_schemas"] = upstream.tools
            save(evidence, client, result, root)
            if result["clean_conversation"]["timed_out"] or result["clean_conversation"]["exit_code"] != 0 or DONE not in result["clean_conversation"]["stdout"]:
                raise RuntimeError(client + ": clean conversation workflow did not finish: " + str(upstream.failures))
            if (work / "calc.py").read_text() != "def add(a, b):\n    return a + b\n" or (work / "test_calc.py").read_text() != test or upstream.failures:
                raise RuntimeError(client + ": real read/edit/test workflow failed: " + str(upstream.failures))
            upstream.scene = "continue"
            result["continuation"] = execute(binary, client, executable, path, gateway, "Continue the prior fixture conversation.", continuing=True)
            if result["continuation"]["timed_out"] or result["continuation"]["exit_code"] != 0 or CONTINUED not in result["continuation"]["stdout"] or upstream.failures:
                raise RuntimeError(client + ": persistent continuation failed: " + str(upstream.failures))
            with sqlite3.connect(gateway["ledger"]) as db:
                persisted = "\n".join(db.iterdump())
                result["audit"] = [json.loads(row[0]) for row in db.execute("SELECT record_json FROM request_audit")]
            logs = (root / "stderr").read_text()
            terminal = [json.loads(line) for line in logs.splitlines() if line.strip()]
            errors = [event for event in terminal if event.get("event") == "request_terminal"
                      and event.get("error_code") == "invalid_upstream_tool_history"]
            if len(errors) != len(bad_requests) or any(event.get("outcome") != "error" for event in errors):
                raise RuntimeError(client + ": each rejected client request needs one terminal error")
            result["malformed_terminal_events"] = errors
            if PRIVATE in persisted + logs or PRIVATE_ID in persisted + logs:
                raise RuntimeError(client + ": withheld tool content persisted in gateway ledger/logs")
            result.update(real_client_read_edit_test=True, clean_conversation_recovery=True,
                          persistent_continuation=True, gateway_no_replay_after_output=True, privacy=True)
            save(evidence, client, result, root)
            return result
        finally:
            observer.shutdown()
            observer.server_close()


def save(evidence, client, result, root):
    if evidence:
        safe = json.dumps(result, indent=2).replace(str(root.resolve()), "[isolated-profile]").replace(str(root), "[isolated-profile]")
        (evidence / (client + ".json")).write_text(safe + "\n")


def check_capacity(binary, client, executable, scope, evidence):
    def capacity_config(config):
        configure(config)
        config["active_session_idle_timeout_seconds"] = 1
        if scope == "provider":
            config["providers"]["anthropic"]["max_active_sessions"] = 1

    with running_gateway(binary, capacity=1 if scope == "gateway" else 0,
                         upstream_handler=Upstream, configure=capacity_config) as gateway:
        root, upstream = gateway["root"], gateway["upstream"]
        upstream.scene, upstream.requests, upstream.failures = "capacity", [], []
        upstream.capacity_started, upstream.capacity_release = threading.Event(), threading.Event()
        work = root / "workspace"
        work.mkdir()
        gateway["work"] = work.resolve()
        observer = Server(("127.0.0.1", 0), Observer)
        observer.gateway_port = int(gateway["url"].rsplit(":", 1)[1])
        observer.requests, observer.capacity_release = [], upstream.capacity_release
        threading.Thread(target=observer.serve_forever, daemon=True).start()
        launch = dict(gateway["config"])
        launch["listen_addr"] = f"127.0.0.1:{observer.server_port}"
        path = root / "launch.json"
        path.write_text(json.dumps(launch))
        holder_result = []

        def hold():
            conn = http.client.HTTPConnection("127.0.0.1", observer.gateway_port, timeout=40)
            try:
                conn.request("POST", "/v1/messages", json.dumps({"model": "anthropic/" + MODEL,
                    "max_tokens": 64, "messages": [{"role": "user", "content": "CAPACITY_HOLD"}]}),
                    {"Content-Type": "application/json", "x-api-key": GATEWAY_KEY,
                     "X-TideMux-Session-ID": "capacity-fixture-holder"})
                response = conn.getresponse()
                holder_result.append((response.status, response.read().decode()))
            finally:
                conn.close()

        holder = threading.Thread(target=hold, daemon=True)
        holder.start()
        result = {"client": client, "capacity_scope": scope}
        try:
            if not upstream.capacity_started.wait(5):
                raise RuntimeError("capacity holder did not acquire a session")
            result["first_attempt"] = execute(binary, client, executable, path, gateway, "Reply after capacity becomes available.")
            holder.join(timeout=5)
            rejected = [request for request in observer.requests if request.get("status") == 429]
            result["rejection_wire"] = [{key: value for key, value in request.items() if key != "body"} for request in rejected]
            save(evidence, client + "-capacity-" + scope, result, root)
            if not rejected:
                raise RuntimeError(client + ": no real CLI " + scope + " capacity rejection observed")
            for request in rejected:
                error = json.loads(request["response"])["error"]
                if error.get("code") != "active_session_limit" or error.get("scope") != scope or error.get("limit") != 1:
                    raise RuntimeError(client + ": capacity diagnostic missing or wrong scope")
                if error.get("retry", {}).get("strategy") != "exponential_backoff_with_jitter":
                    raise RuntimeError("capacity response lost bounded backoff guidance")
                if request.get("retry_after") is not None:
                    raise RuntimeError("capacity response invented an exact Retry-After")
            if not holder_result or holder_result[0][0] != 200:
                raise RuntimeError("capacity holder failed to finish")
            # If the client terminates instead of retrying, run the same fresh
            # command after the occupied session's configured idle retention.
            # The test does not change capacity or edit the client's history.
            if CAPACITY_READY not in result["first_attempt"]["stdout"] or result["first_attempt"]["exit_code"] != 0:
                time.sleep(1.2)
                result["explicit_retry"] = execute(binary, client, executable, path, gateway, "Reply after capacity becomes available.")
            recovered = result.get("explicit_retry", result["first_attempt"])
            if recovered["timed_out"] or recovered["exit_code"] != 0 or CAPACITY_READY not in recovered["stdout"]:
                raise RuntimeError(client + ": real CLI did not recover after capacity was released")
            successful_posts = sum(request.get("status") == 200 for request in observer.requests)
            if upstream.posts != 1 + successful_posts:
                raise RuntimeError(client + ": rejected capacity request was dispatched or a request replayed")
            logs = (root / "stderr").read_text()
            events = [json.loads(line) for line in logs.splitlines() if line.strip()]
            terminal = [event for event in events if event.get("event") in ("request_terminal", "local_rejection")
                        and event.get("error_code") == "active_session_limit"]
            if len(terminal) != len(rejected) or any(event.get("upstream_attempted") for event in terminal):
                raise RuntimeError(client + ": capacity rejection needs one terminal event and zero dispatch")
            result.update(real_cli_capacity_rejection=True, real_cli_capacity_recovery=True,
                rejected_client_requests=len(rejected), automatic_recovery="explicit_retry" not in result,
                client_visible_capacity_error_code="active_session_limit" in result["first_attempt"]["stdout"] + result["first_attempt"]["stderr"],
                first_attempt_timed_out=result["first_attempt"]["timed_out"],
                capacity_rejections_zero_upstream_dispatch=True, terminal_events=terminal,
                upstream_posts=upstream.posts, successful_client_requests=successful_posts)
            save(evidence, client + "-capacity-" + scope, result, root)
            return {key: result[key] for key in ("client", "capacity_scope", "real_cli_capacity_rejection",
                "real_cli_capacity_recovery", "rejected_client_requests", "automatic_recovery",
                "client_visible_capacity_error_code", "first_attempt_timed_out",
                "capacity_rejections_zero_upstream_dispatch")}
        finally:
            upstream.capacity_release.set()
            holder.join(timeout=5)
            observer.shutdown()
            observer.server_close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--client", action="append", choices=["claude", "kilo", "hermes"])
    parser.add_argument("--evidence", type=Path)
    parser.add_argument("--capacity-scope", choices=["gateway", "provider", "both"], default="both",
                        help="both is the 0.3.2 release gate; gateway checks the older baseline")
    args = parser.parse_args()
    binary = args.binary.resolve()
    if not binary.is_file() or not os.access(binary, os.X_OK):
        raise SystemExit("--binary must be an extracted executable candidate")
    if args.evidence:
        args.evidence.mkdir(parents=True, exist_ok=True, mode=0o700)
    clients = args.client or ["claude", "kilo", "hermes"]
    results, capacity_results = [], []
    for client in clients:
        executable = shutil.which(client)
        if not executable:
            raise SystemExit("Required installed client not found: " + client)
        result = check_client(binary, client, Path(executable).resolve(), args.evidence)
        results.append({key: result[key] for key in ("client", "version", "client_api", "provider_api", "cli_mode",
            "malformed_client_streaming_requests", "malformed_client_auxiliary_requests", "client_visible_safe_prefix",
            "client_visible_error_code", "client_visible_recovery_text", "real_client_read_edit_test", "clean_conversation_recovery",
            "persistent_continuation", "gateway_no_replay_after_output", "privacy",
            "glm_compatible_completion_and_continuation", "glm_single_claude_request")})
        print(json.dumps(results[-1]), flush=True)
        scopes = ("gateway", "provider") if args.capacity_scope == "both" else (args.capacity_scope,)
        for scope in scopes:
            capacity_result = check_capacity(binary, client, Path(executable).resolve(), scope, args.evidence)
            capacity_results.append(capacity_result)
            print(json.dumps(capacity_result), flush=True)
    summary = {"binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(), "clients": results,
               "capacity": capacity_results,
               "real_cli_with_scripted_loopback_provider": True, "production_or_paid_api_used": False}
    if args.evidence:
        (args.evidence / "result.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary))


if __name__ == "__main__":
    main()
