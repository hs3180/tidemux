#!/usr/bin/env python3
"""Verify the instance auto chain, session pinning and accounting in a package.

Uses only fake Keychain credentials and loopback mocks. Shares HTTP fixture
helpers with the shared-model package check, but exercises independent state.
"""
import argparse
import hashlib
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
from http.server import BaseHTTPRequestHandler

from test_shared_model_affinity_package import Server, free_port, post, GATEWAY_KEY, PROVIDER_KEY, SESSION, BODY


class AutoUpstream(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def reply(self, status, value, content_type="application/json"):
        data = value if isinstance(value, bytes) else json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.reply(200, {"object": "list", "data": [{"id": "model-one"}, {"id": "model-two"}]})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        provider = self.path.split("/")[1]
        with self.server.lock:
            self.server.posts.append((provider, body["model"], self.headers.get("X-TideMux-Session-ID", "")))
        if body.get("stream"):
            self.reply(200, b'data: {"model":"provider-alias","choices":[{"index":0,"delta":{"content":"partial"}}]}\n\nevent: error\ndata: {"error":{"code":"missing_model","status":404}}\n\n', "text/event-stream")
        elif provider == "a":
            self.reply(404, {"error": {"code": "missing_model", "message": "private_auto_provider_error"}})
        else:
            self.reply(200, {
                "id": "chatcmpl-auto", "object": "chat.completion", "model": "provider-alias",
                "choices": [{"index": 0, "message": {"role": "assistant", "content": "completed"}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 2, "completion_tokens": 2, "total_tokens": 4, "prompt_tokens_details": {"cached_tokens": 0}},
            })


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--baseline-binary", type=Path, help="also verify v0.2.2 can read the candidate's audit ledger")
    args = parser.parse_args()
    binary = args.binary.resolve()
    version = subprocess.check_output([str(binary), "version"], text=True).strip()
    if version != "0.3.1":
        raise RuntimeError("expected the 0.3.1 packaged binary")
    upstream = Server(("127.0.0.1", 0), AutoUpstream)
    upstream.posts, upstream.lock = [], threading.Lock()
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    process = None
    try:
        with tempfile.TemporaryDirectory(prefix="tidemux-auto-chain.") as temporary:
            root = Path(temporary)
            fake_bin = root / "bin"
            fake_bin.mkdir()
            security = fake_bin / "security"
            security.write_text('#!/bin/sh\ncase "$*" in *test.gateway*) printf "affinity-local-gateway-key\\n";; *test.provider*) printf "affinity-local-provider-key\\n";; *) exit 1;; esac\n')
            security.chmod(0o755)
            home = root / "home"
            home.mkdir()
            env = {"PATH": str(fake_bin) + ":/usr/bin:/bin:/opt/homebrew/bin", "HOME": str(home), "TMPDIR": str(root), "LANG": "C.UTF-8"}
            port = free_port()
            url = f"http://127.0.0.1:{port}"
            ledger = root / "ledger.db"
            config = {
                "listen_addr": f"127.0.0.1:{port}", "max_in_flight": 2, "ledger_path": str(ledger),
                "access_token_keychain": {"service": "test.gateway", "account": "local"},
                "routing": {"billing_exhaustion_failover": True},
                "providers": {
                    ref: {
                        "protocol": "openai", "base_url": f"http://127.0.0.1:{upstream.server_port}/{ref}/v1", "upstream_id": "same-vendor",
                        "upstream_keychain": {"service": "test.provider", "account": ref}, "supported_models": ["model-one", "model-two"],
                        "error_code_mappings": [{"upstream_code": "missing_model", "http_status": 404, "category": "model_not_found"}],
                        "prices": {"model-two": {"currency": "USD", "source": "package-fixture", "version": "2026-10-02", "input_cache_hit_per_million": 1, "input_cache_miss_per_million": 2, "output_per_million": 3}},
                    } for ref in ("a", "b")
                },
            }
            config_path = root / "config.json"
            config_path.write_text(json.dumps(config))
            def cli(*args):
                return subprocess.check_output([str(binary), "auto-chain", *args, "--config", str(config_path)], env=env, text=True)
            cli("set", "--entries", "a/model-one,b/model-two")
            if "1\ta/model-one\n2\tb/model-two" not in cli("show"):
                raise RuntimeError("top-level CLI did not preserve the configured order")
            stdout_path, stderr_path = root / "stdout.log", root / "stderr.log"
            with stdout_path.open("w") as stdout, stderr_path.open("w") as stderr:
                def start():
                    started = subprocess.Popen([str(binary), "serve", "--config", str(config_path)], env=env, stdout=stdout, stderr=stderr)
                    for _ in range(200):
                        if started.poll() is not None:
                            raise RuntimeError("packaged gateway failed to start")
                        try:
                            with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                                return started
                        except OSError:
                            time.sleep(0.05)
                    started.terminate()
                    started.wait(timeout=5)
                    raise RuntimeError("gateway startup timed out")
                def stop(started):
                    started.send_signal(signal.SIGTERM)
                    started.wait(timeout=10)
                    if started.returncode != 0:
                        raise RuntimeError("gateway shutdown failed")
                process = start()
                for header in (None, None, SESSION):
                    status, _ = post(url, header, protocol="anthropic", model="auto", metadata=SESSION)
                    if status != 404:
                        raise RuntimeError("existing auto session was replayed or moved after failure")
                status, result = post(url, protocol="anthropic", model="auto", metadata=SESSION + "-reset")
                if status != 200 or json.loads(result)["model"] != "model-two":
                    raise RuntimeError("new auto session did not use the next provider/model")
                for _ in range(2):
                    status, result = post(url, model="auto")
                    if status != 200 or json.loads(result)["model"] != "model-two":
                        raise RuntimeError("request-scoped auto selection failed")
                status, _ = post(url, SESSION, model="a/auto")
                if status != 400:
                    raise RuntimeError("provider-qualified auto was accepted")
                status, _ = post(url, SESSION, model="a/model-one")
                if status != 404:
                    raise RuntimeError("explicit model request used the auto chain")
                with upstream.lock:
                    first_run = list(upstream.posts)
                if len(first_run) != 7 or any(p != "a" or m != "model-one" for p, m, _ in first_run[:3]):
                    raise RuntimeError("auto failure produced duplicate or incorrectly attributed upstream requests")
                if first_run[4][2] == "" or first_run[5][2] == "" or first_run[4][2] == first_run[5][2]:
                    raise RuntimeError("no-ID auto requests did not receive fresh session IDs")
                stop(process)
                process = start()
                status, result = post(url, SESSION + "-stream", model="auto", stream=True)
                if status != 200 or "partial" not in result or "event: error" not in result or '"model":"model-one"' not in result or "provider-alias" in result:
                    raise RuntimeError("auto stream terminal failure was hidden")
                status, _ = post(url, SESSION + "-after-stream", model="auto")
                if status != 404:
                    raise RuntimeError("stream output advanced the preference for a new session")
                stop(process)
            with upstream.lock:
                posts = list(upstream.posts)
            if len(posts) != 9 or any(p != "a" or m != "model-one" for p, m, _ in posts[-2:]):
                raise RuntimeError("restart/stream behavior changed the auto chain unexpectedly")
            with sqlite3.connect(ledger) as connection:
                rows = [json.loads(row[0]) for row in connection.execute("SELECT record_json FROM request_audit")]
            if len(rows) != len(posts):
                raise RuntimeError("auto attempt audit count differs from upstream work")
            for row in rows:
                if row["upstream"] != "same-vendor" or row.get("provider_ref") not in ("a", "b"):
                    raise RuntimeError("audit could not distinguish profiles of the same upstream vendor")
                if row["provider_ref"] == "b" and (row["model"] != "model-two" or row["input_tokens"] != 2 or row["output_tokens"] != 2 or row["currency"] != "USD" or abs(row["estimated_cost"] - 0.000010) > 1e-12):
                    raise RuntimeError("usage or fees did not belong to the actual provider/model: " + json.dumps({key: row.get(key) for key in ("upstream", "model", "input_tokens", "output_tokens", "cache_read_tokens", "currency", "estimated_cost", "cost_source")}))
            logs = stdout_path.read_text() + stderr_path.read_text() + json.dumps(rows)
            if any(value in logs for value in (GATEWAY_KEY, PROVIDER_KEY, SESSION, BODY, "private_auto_provider_error")):
                raise RuntimeError("auto session identifiers, credentials or bodies were persisted")
            cli("clear")
            if "auto_chain" in json.loads(config_path.read_text()):
                raise RuntimeError("clear retained the optional config field")
            if args.baseline_binary:
                baseline = args.baseline_binary.resolve()
                if subprocess.check_output([str(baseline), "version"], text=True).strip() != "0.2.2":
                    raise RuntimeError("expected v0.2.2 for ledger rollback verification")
                subprocess.run([str(binary), "routing", "set", "--billing-exhaustion-failover=false", "--config", str(config_path)], env=env, check=True, capture_output=True)
                default_config = home / "Library" / "Application Support" / "TideMux" / "config.json"
                default_config.parent.mkdir(parents=True)
                default_config.write_bytes(config_path.read_bytes())
                before = hashlib.sha256(ledger.read_bytes()).hexdigest()
                report = json.loads(subprocess.check_output([str(baseline), "billing", "--details", "--json", "--from", "1970-01-01T00:00:00.001Z"], env=env, text=True))
                if len(report["requests"]) != len(rows) or before != hashlib.sha256(ledger.read_bytes()).hexdigest():
                    raise RuntimeError("v0.2.2 could not read all candidate audits without changing the ledger")
                if sorted(row["id"] for row in report["requests"]) != sorted(row["id"] for row in rows):
                    raise RuntimeError("ledger rollback omitted candidate audit records")
            print(json.dumps({"binary": str(binary), "version": version, "instance_chain_cli": True, "existing_sessions_pinned_across_failure": True, "new_sessions_advance_provider_and_model": True, "no_id_fresh_ids": True, "qualified_auto_rejected": True, "stream_does_not_advance": True, "actual_usage_and_cost_attribution": True, "private_logs_and_ledger": True, "baseline_ledger_read_only": bool(args.baseline_binary), "upstream_attempts": len(posts)}))
    finally:
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        upstream.shutdown()
        upstream.server_close()


if __name__ == "__main__":
    main()
