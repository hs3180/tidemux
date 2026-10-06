#!/usr/bin/env python3
"""Verify provider logical-session capacity on an isolated actual binary."""
import argparse
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler
import hashlib
import json
import os
from pathlib import Path
import signal
import sqlite3
import subprocess
import tempfile
import threading
import time
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from test_runtime_logs_package import Server, free_port, GATEWAY_KEY

PRIVATE_SESSION = "private-capacity-session"
PRIVATE_KEY = "private-capacity-provider-key"
PRIVATE_PROMPT = "private-capacity-prompt"


class Upstream(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def reply(self, body):
        data = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.reply({"object": "list", "data": [{"id": m} for m in ("custom-model", "other-model")]})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        with self.server.lock:
            self.server.calls += 1
        if body.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            self.wfile.write(b'data: {"choices":[{"index":0,"delta":{"content":"held"}}]}\n\n')
            self.wfile.flush()
            self.server.started.set()
            self.server.release.wait(15)
            self.wfile.write(b'data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}\n\ndata: [DONE]\n\n')
            self.wfile.flush()
            return
        self.reply({"id": "test", "object": "chat.completion", "model": body["model"], "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 3, "completion_tokens": 2}})


def wait_for(check, label):
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(.05)
    raise RuntimeError(label + " timed out")


def request(url, protocol, session, model="glm/custom-model", stream=False):
    body = {"model": model, "max_tokens": 16, "stream": stream, "messages": [{"role": "user", "content": PRIVATE_PROMPT}]}
    headers = {"Authorization": "Bearer " + GATEWAY_KEY, "Content-Type": "application/json"}
    if session:
        headers["X-TideMux-Session-ID"] = session
    path = "/v1/messages" if protocol == "anthropic" else "/v1/chat/completions"
    try:
        return urlopen(Request(url + path, json.dumps(body).encode(), headers), timeout=20)
    except HTTPError as error:
        return error


def query(url, protocol, session, model="glm/custom-model"):
    with request(url, protocol, session, model) as response:
        return response.status, json.loads(response.read())


def status(url, path="/tidemux/session-status"):
    with urlopen(Request(url + path, headers={"Authorization": "Bearer " + GATEWAY_KEY}), timeout=5) as response:
        return json.loads(response.read())


def reject(result, scope, cap):
    code, body = result
    error = body.get("error", {})
    if code != 429 or error.get("code") != "active_session_limit" or error.get("scope") != scope or error.get("limit") != cap:
        raise RuntimeError("capacity refusal mismatch: " + str(result))


def run(binary, root, protocol, cap, global_cap, action):
    upstream = Server(("127.0.0.1", 0), Upstream)
    upstream.lock, upstream.calls = threading.Lock(), 0
    upstream.started, upstream.release = threading.Event(), threading.Event()
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    env = dict(os.environ, PATH=str(root / "bin") + ":" + os.environ.get("PATH", ""))
    port = free_port()
    url = f"http://127.0.0.1:{port}"
    endpoint = f"http://127.0.0.1:{upstream.server_port}/v1"
    config, ledger = root / "config.json", root / "ledger.db"
    provider = {"protocol": "openai", "base_url": endpoint, "upstream_keychain": {"service": "test.provider", "account": "local"}, "supported_models": ["custom-model", "other-model"]}
    value = {"listen_addr": f"127.0.0.1:{port}", "max_in_flight": 32, "max_active_sessions": global_cap, "ledger_path": str(ledger), "access_token_keychain": {"service": "test.gateway", "account": "local"}, "providers": {"glm": dict(provider, max_active_sessions=cap), "other": provider}}
    config.write_text(json.dumps(value))
    config.chmod(0o600)
    process = None
    try:
        with (root / "stdout").open("w") as stdout, (root / "stderr").open("w") as stderr:
            process = subprocess.Popen([str(binary), "serve", "--config", str(config)], env=env, stdout=stdout, stderr=stderr)
            wait_for(lambda: "listening" in (root / "stdout").read_text(), "startup")
            action(url, upstream, config, env)
            process.send_signal(signal.SIGTERM)
            process.wait(timeout=10)
            if process.returncode:
                raise RuntimeError("shutdown failed")
        logs = (root / "stderr").read_text()
        if any(marker in logs for marker in (PRIVATE_SESSION, PRIVATE_KEY, PRIVATE_PROMPT)):
            raise RuntimeError("runtime metadata privacy regression")
        for line in logs.splitlines():
            json.loads(line)
        with sqlite3.connect(ledger) as connection:
            audit_count = connection.execute("SELECT COUNT(*) FROM request_audit").fetchone()[0]
        if audit_count != upstream.calls:
            raise RuntimeError(f"dispatch/audit mismatch: {upstream.calls}/{audit_count}")
        return {"protocol": protocol, "upstream_calls": upstream.calls, "audits": audit_count}
    finally:
        upstream.release.set()
        if process and process.poll() is None:
            process.kill()
            process.wait()
        upstream.shutdown()
        upstream.server_close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    args = parser.parse_args()
    binary = args.binary.resolve()
    results = []
    with tempfile.TemporaryDirectory(prefix="tidemux-provider-capacity.") as temporary:
        base = Path(temporary)
        for protocol in ("openai", "anthropic"):
            for phase in ("caps", "reload"):
                root = base / (protocol + "-" + phase)
                (root / "bin").mkdir(parents=True)
                security = root / "bin" / "security"
                security.write_text("#!/usr/bin/env python3\nimport sys\na=sys.argv\nprint(" + repr(GATEWAY_KEY) + " if a[a.index('-s')+1]=='test.gateway' else " + repr(PRIVATE_KEY) + ")\n")
                security.chmod(0o700)
                if phase == "caps":
                    def caps(url, upstream, config, env):
                        for i in range(5):
                            if query(url, protocol, PRIVATE_SESSION + str(i))[0] != 200:
                                raise RuntimeError("GLM admitted fewer than five")
                        reject(query(url, protocol, PRIVATE_SESSION + "sixth"), "provider", 5)
                        if upstream.calls != 5:
                            raise RuntimeError("sixth GLM request dispatched")
                        for session, model in ((PRIVATE_SESSION + "0", "glm/other-model"), (PRIVATE_SESSION + "sixth", "other/custom-model"), (PRIVATE_SESSION + "0", "other/custom-model")):
                            if query(url, protocol, session, model)[0] != 200:
                                raise RuntimeError("same-session or provider independence failed")
                        reject(query(url, protocol, PRIVATE_SESSION + "seventh", "other/custom-model"), "gateway", 6)
                        state = status(url)
                        if state["gateway"]["current"] != 6 or state["providers"]["glm"]["current"] != 5 or state["providers"]["other"]["current"] != 2 or upstream.calls != 8:
                            raise RuntimeError("logical session counts mismatch")
                        with ThreadPoolExecutor(max_workers=8) as pool:
                            if any(code != 200 for code, _ in pool.map(lambda _: query(url, protocol, PRIVATE_SESSION + "0"), range(8))):
                                raise RuntimeError("same session concurrency refused")
                    results.append(dict(run(binary, root, protocol, 5, 6, caps), phase=phase))
                else:
                    def reload(url, upstream, config, env):
                        for i in range(2):
                            if query(url, protocol, PRIVATE_SESSION + str(i))[0] != 200:
                                raise RuntimeError("unlimited initial requests failed")
                        cli = subprocess.run([str(binary), "provider", "update", "glm", "--max-active-sessions", "1", "--config", str(config)], env=env, capture_output=True, text=True, timeout=15)
                        if cli.returncode:
                            raise RuntimeError("provider capacity CLI failed")
                        wait_for(lambda: status(url)["providers"]["glm"]["limit"] == 1, "limit application")
                        reject(query(url, protocol, PRIVATE_SESSION + "new"), "provider", 1)
                        if upstream.calls != 2 or query(url, protocol, PRIVATE_SESSION + "0")[0] != 200:
                            raise RuntimeError("unlimited-to-limited lost occupancy")
                        # Remove and re-add the profile while an old-view stream
                        # is active; the quota is shared across those views.
                        with request(url, protocol, PRIVATE_SESSION + "0", stream=True) as held:
                            if not upstream.started.wait(5):
                                raise RuntimeError("held SSE did not begin")
                            current = json.loads(config.read_text())
                            saved = current["providers"].pop("glm")
                            config.write_text(json.dumps(current))
                            wait_for(lambda: "glm" not in status(url)["providers"], "provider removal")
                            current["providers"]["glm"] = saved
                            config.write_text(json.dumps(current))
                            wait_for(lambda: "glm" in status(url)["providers"], "provider re-addition")
                            reject(query(url, protocol, PRIVATE_SESSION + "new"), "provider", 1)
                            upstream.release.set()
                            tail = held.read().decode()
                            if ("[DONE]" if protocol == "openai" else "message_stop") not in tail or "event: error" in tail:
                                raise RuntimeError("reload interrupted held stream")
                        if status(url)["providers"]["glm"]["current"] != 2:
                            raise RuntimeError("stream release lost retained session")
                        if query(url, protocol, PRIVATE_SESSION + "new", "other/custom-model")[0] != 200:
                            raise RuntimeError("GLM cap constrained another provider")
                    results.append(dict(run(binary, root, protocol, 0, 0, reload), phase=phase))
    print(json.dumps({"binary": str(binary), "sha256": hashlib.sha256(binary.read_bytes()).hexdigest(), "results": results, "glm_five": True, "provider_and_global_isolation": True, "concurrent_reuse": True, "unlimited_to_limited": True, "remove_readd_held_sse": True, "privacy": True}))


if __name__ == "__main__":
    main()
