#!/usr/bin/env python3
"""Exercise real Claude Code through a local TideMux and disposable mocks.

Requires macOS, Go, and the Claude Code CLI. It overrides Claude's endpoint and
API key, isolates its config directory, and provides a fake `security` command
for TideMux; no system/provider credentials or public endpoint are used.
"""
import hashlib
import argparse
import http.client
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = Path(__file__).resolve().parents[1]
MODEL = "claude-haiku-4-5"
BETA = "context-management-2025-06-27"
FAKE_GATEWAY_KEY = "local-gateway-key"
FAKE_PROVIDER_KEY = "local-provider-key"


def isolated_environment(root):
    # Preserve only process settings required to launch the CLI. In particular,
    # do not pass ambient provider credentials, proxies, or cloud tokens into
    # Claude Code during this local-mock acceptance run.
    env = {key: os.environ[key] for key in ("PATH", "LANG", "LC_ALL", "TERM")
           if key in os.environ}
    env.update({
        "HOME": str(root),
        "TMPDIR": str(root),
        "XDG_CONFIG_HOME": str(root / "xdg-config"),
        "XDG_CACHE_HOME": str(root / "xdg-cache"),
        "XDG_DATA_HOME": str(root / "xdg-data"),
    })
    return env


def sse(events):
    return "".join(
        f"event: {name}\ndata: {json.dumps(value, separators=(',', ':'))}\n\n"
        for name, value in events
    ).encode()


def anthropic_text(model):
    return sse([
        ("message_start", {"type": "message_start", "message": {
            "id": "msg_local", "type": "message", "role": "assistant", "model": model,
            "content": [], "stop_reason": None, "stop_sequence": None,
            "usage": {"input_tokens": 1, "output_tokens": 0},
        }}),
        ("content_block_start", {"type": "content_block_start", "index": 0,
                                  "content_block": {"type": "text", "text": ""}}),
        ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                  "delta": {"type": "text_delta", "text": "OK"}}),
        ("content_block_stop", {"type": "content_block_stop", "index": 0}),
        ("message_delta", {"type": "message_delta",
                            "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                            "usage": {"output_tokens": 1}}),
        ("message_stop", {"type": "message_stop"}),
    ])


def openai_text(model):
    chunks = [
        {"id": "chatcmpl_local", "object": "chat.completion.chunk", "created": 1,
         "model": model, "choices": [{"index": 0,
             "delta": {"role": "assistant", "content": "OK"}, "finish_reason": None}]},
        {"id": "chatcmpl_local", "object": "chat.completion.chunk", "created": 1,
         "model": model, "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]},
        {"id": "chatcmpl_local", "object": "chat.completion.chunk", "created": 1,
         "model": model, "choices": [], "usage": {"prompt_tokens": 1, "completion_tokens": 1}},
    ]
    return b"".join(b"data: " + json.dumps(chunk, separators=(",", ":")).encode() + b"\n\n"
                     for chunk in chunks) + b"data: [DONE]\n\n"


class Server(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True


class UpstreamHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def do_GET(self):
        if self.server.mode == "openai":
            body = {"object": "list", "data": [{"id": MODEL, "object": "model"}]}
        else:
            body = {"data": [{"id": MODEL, "type": "model"}], "has_more": False}
        self._write(200, "application/json", json.dumps(body).encode())

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length)
        try:
            body = json.loads(raw)
        except Exception:
            self._write(400, "application/json", b"{}")
            return
        fake_auth = (self.headers.get("Authorization") == "Bearer " + FAKE_PROVIDER_KEY
                     if self.server.mode == "openai"
                     else self.headers.get("x-api-key") == FAKE_PROVIDER_KEY)
        self.server.requests.append({
            "path": self.path,
            "body": body,
            "beta": self.headers.get("anthropic-beta", ""),
            "fake_auth": fake_auth,
        })
        model = body.get("model", MODEL)
        payload = openai_text(model) if self.server.mode == "openai" else anthropic_text(model)
        self._write(200, "text/event-stream", payload)

    def _write(self, status, content_type, payload):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


class IngressHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def do_GET(self):
        self._forward()

    def do_POST(self):
        self._forward()

    def _forward(self):
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length) if length else b""
        record = {
            "path": self.path,
            "body": None,
            "beta": self.headers.get("anthropic-beta", ""),
            "fake_auth": self.headers.get("x-api-key") == FAKE_GATEWAY_KEY,
        }
        if self.command == "POST":
            try:
                record["body"] = json.loads(raw)
            except Exception:
                record["body"] = {}
            self.server.requests.append(record)
        headers = {
            key: value for key, value in self.headers.items()
            if key.lower() not in {"host", "connection", "content-length", "transfer-encoding"}
        }
        conn = http.client.HTTPConnection("127.0.0.1", self.server.gateway_port, timeout=60)
        try:
            conn.request(self.command, self.path, body=raw or None, headers=headers)
            response = conn.getresponse()
            payload = response.read()
            record["response_status"] = response.status
            record["response_type"] = response.getheader("Content-Type", "")
            self.send_response(response.status)
            for key, value in response.getheaders():
                if key.lower() in {"content-type", "retry-after", "x-tidemux-request-id"}:
                    self.send_header(key, value)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            if payload:
                self.wfile.write(payload)
        except Exception:
            self.send_error(502)
        finally:
            conn.close()


def start_server(handler, **attributes):
    server = Server(("127.0.0.1", 0), handler)
    for name, value in attributes.items():
        setattr(server, name, value)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def redact(text):
    return text.replace(FAKE_GATEWAY_KEY, "[fake-gateway-key]").replace(
        FAKE_PROVIDER_KEY, "[fake-provider-key]")[-1200:]


def run_mode(root, binary, mode):
    upstream = start_server(UpstreamHandler, mode=mode, requests=[])
    gateway_port = _free_port()
    config = {
        "listen_addr": f"127.0.0.1:{gateway_port}",
        "max_in_flight": 4,
        "ledger_path": str(root / f"ledger-{mode}.db"),
        "access_token_keychain": {"service": "test.gateway", "account": "local"},
        "providers": {"test": {
            "protocol": mode,
            "base_url": f"http://127.0.0.1:{upstream.server_port}/v1",
            "upstream_id": "test",
            "upstream_keychain": {"service": "test.provider", "account": "local"},
            "supported_models": [MODEL],
        }},
    }
    config_path = root / f"config-{mode}.json"
    config_path.write_text(json.dumps(config))
    fake_bin = root / f"bin-{mode}"
    fake_bin.mkdir()
    security = fake_bin / "security"
    security.write_text(
        "#!/bin/sh\ncase \"$*\" in *test.gateway*) "
        "printf 'local-gateway-key\\n';; *) printf 'local-provider-key\\n';; esac\n"
    )
    security.chmod(0o755)
    home = root / f"home-{mode}"
    claude_config = root / f"claude-config-{mode}"
    work = root / f"work-{mode}"
    home.mkdir(); claude_config.mkdir(); work.mkdir()
    env = isolated_environment(home)
    env.update({
        "PATH": str(fake_bin) + ":" + env.get("PATH", ""),
        "HOME": str(home),
        "CLAUDE_CONFIG_DIR": str(claude_config),
        "ANTHROPIC_API_KEY": FAKE_GATEWAY_KEY,
        "ANTHROPIC_AUTH_TOKEN": "",
        "ANTHROPIC_BETAS": BETA,
        "CLAUDE_CODE_DISABLE_NON_ESSENTIAL_TRAFFIC": "1",
        "DISABLE_AUTOUPDATER": "1",
        "DISABLE_ERROR_REPORTING": "1",
        "DISABLE_TELEMETRY": "1",
    })
    gateway = subprocess.Popen(
        [str(binary), "serve", "--config", str(config_path)],
        cwd=ROOT, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    ingress = None
    try:
        ready = False
        for _ in range(100):
            if gateway.poll() is not None:
                break
            try:
                with socket.create_connection(("127.0.0.1", gateway_port), timeout=0.1):
                    ready = True
                    break
            except OSError:
                time.sleep(0.05)
        if not ready:
            raise RuntimeError("candidate TideMux did not start")
        ingress = start_server(IngressHandler, gateway_port=gateway_port, requests=[])
        env["ANTHROPIC_BASE_URL"] = f"http://127.0.0.1:{ingress.server_port}"
        result = subprocess.run(
            ["claude", "-p", "Reply with exactly OK.", "--model", MODEL, "--output-format", "json"],
            cwd=work, env=env, text=True, capture_output=True, timeout=120,
        )
        try:
            cli_result = json.loads(result.stdout).get("result")
        except Exception:
            cli_result = None
        if result.returncode != 0 or cli_result != "OK":
            summary = {
                "returncode": result.returncode,
                "stdout": redact(result.stdout),
                "stderr": redact(result.stderr),
                "ingress": [{"path": item["path"], "beta_present": BETA in item["beta"],
                             "response_status": item.get("response_status")} for item in ingress.requests],
                "upstream": [{"path": item["path"], "fake_auth": item["fake_auth"],
                              "has_context_management": isinstance(item["body"].get("context_management"), dict)}
                             for item in upstream.requests],
            }
            raise RuntimeError("Claude Code local-mock run failed: " + json.dumps(summary, ensure_ascii=False))
        if not ingress.requests or not upstream.requests:
            raise RuntimeError("Claude Code request did not traverse both TideMux boundaries")
        inbound = ingress.requests[0]
        outbound = upstream.requests[0]
        source = inbound["body"].get("context_management")
        forwarded = outbound["body"].get("context_management")
        if not isinstance(source, dict):
            raise RuntimeError("Claude Code did not emit a non-null context_management object")
        if source != forwarded:
            raise RuntimeError("complete context_management JSON changed on the TideMux route")
        if BETA not in inbound["beta"]:
            raise RuntimeError("Claude Code context-management beta header was absent")
        if mode == "anthropic" and BETA not in outbound["beta"]:
            raise RuntimeError("TideMux did not forward the native Anthropic beta header")
        if not inbound["fake_auth"] or not outbound["fake_auth"]:
            raise RuntimeError("a local fake credential was not used at both boundaries")
        digest = hashlib.sha256(json.dumps(source, sort_keys=True, separators=(",", ":")).encode()).hexdigest()[:16]
        return {
            "mode": mode,
            "claude_exit": result.returncode,
            "client_requests": len(ingress.requests),
            "upstream_requests": len(upstream.requests),
            "context_management_present": True,
            "context_management_keys": sorted(source.keys()),
            "complete_json_equal": True,
            "context_digest": digest,
            "beta_at_client": True,
            "beta_at_native_upstream": BETA in outbound["beta"] if mode == "anthropic" else None,
            "fake_credentials_only": True,
            "cli_result": cli_result,
        }
    finally:
        gateway.terminate()
        try:
            gateway.wait(timeout=5)
        except subprocess.TimeoutExpired:
            gateway.kill(); gateway.wait(timeout=5)
        if ingress is not None:
            ingress.shutdown(); ingress.server_close()
        upstream.shutdown(); upstream.server_close()


def _free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path,
                        help="candidate TideMux binary; defaults to a source build")
    args = parser.parse_args()
    if sys.platform != "darwin":
        raise SystemExit("This acceptance check requires macOS Keychain-compatible runtime behavior.")
    if shutil.which("claude") is None:
        raise SystemExit("Claude Code CLI is required for this acceptance check.")
    with tempfile.TemporaryDirectory(prefix="tidemux-claude-context-e2e.") as temporary:
        root = Path(temporary)
        if args.binary:
            binary = args.binary.resolve()
            if not binary.is_file() or not os.access(binary, os.X_OK):
                raise SystemExit("--binary must point to an executable candidate binary")
        else:
            binary = root / "tidemux"
            build_env = {key: os.environ[key] for key in (
                "PATH", "HOME", "GOPATH", "GOMODCACHE", "GOCACHE", "GOTOOLCHAIN"
            ) if key in os.environ}
            build_env["CGO_ENABLED"] = "0"
            subprocess.run(["go", "build", "-o", str(binary), "./cmd/tidemux"], cwd=ROOT,
                           env=build_env, check=True, stdout=subprocess.DEVNULL)
        results = [run_mode(root, binary, "openai"), run_mode(root, binary, "anthropic")]
        version_home = root / "version-home"
        version_home.mkdir()
        print(json.dumps({"claude_version": subprocess.check_output(
                              ["claude", "--version"], text=True,
                              env=isolated_environment(version_home)).strip(),
                          "runs": results}, ensure_ascii=False))


if __name__ == "__main__":
    main()
