#!/usr/bin/env python3
"""Verify default usage JSONL with a packaged binary, fake keys and loopback mocks."""
import argparse
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler
import json
import hashlib
import hmac
import math
import os
from pathlib import Path
import shutil
import signal
import sqlite3
import subprocess
import tempfile
import threading
import time
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

from test_runtime_logs_package import Server, free_port, GATEWAY_KEY, PROVIDER_KEY

PROMPT = "usage-private-prompt-sentinel"
SESSION = "usage-private-session-sentinel"
RESPONSE = "usage-private-response-sentinel"
MODEL = "claude-sonnet-4-20250514"


class Mock(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def write(self, status, value):
        data = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.write(200, {"object": "list", "data": [{"id": MODEL}]})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if body.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            self.wfile.write(b'data: {"choices":[{"index":0,"delta":{"content":"usage-private-response-sentinel"}}]}\n\n')
            self.wfile.flush()
            self.server.started.set()
            self.server.release.wait()
            try:
                self.wfile.write(b'data: [DONE]\n\n')
                self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError):
                pass
            return
        if "/error/" in self.path:
            self.write(403, {"error": {"message": RESPONSE, "code": "private-error-sentinel"}})
            return
        if "/native/" in self.path:
            self.write(200, {"id": "fixture", "type": "message", "role": "assistant", "model": MODEL,
                             "content": [{"type": "text", "text": RESPONSE}], "stop_reason": "end_turn",
                             "usage": {"input_tokens": 3, "output_tokens": 2,
                                       "cache_read_input_tokens": 3, "cache_creation_input_tokens": 1}})
            return
        value = {"id": "fixture", "object": "chat.completion", "model": MODEL,
                 "choices": [{"index": 0, "message": {"role": "assistant", "content": RESPONSE}, "finish_reason": "stop"}]}
        if "/unknown/" not in self.path:
            value["usage"] = {"prompt_tokens": 7, "completion_tokens": 2,
                              "prompt_tokens_details": {"cached_tokens": 3, "cache_write_tokens": 1}}
        if "/partial/" in self.path:
            value["usage"] = {"prompt_tokens": 5}
        self.write(200, value)


def wait_for(predicate, label):
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(.1)
    raise RuntimeError(label + " timed out")


def audits(path):
    with sqlite3.connect(path) as db:
        return [json.loads(row[0]) for row in db.execute("SELECT record_json FROM request_audit ORDER BY rowid")]


def unique_object(pairs):
    value = {}
    for key, item in pairs:
        assert key not in value, "duplicate JSON field: " + key
        value[key] = item
    return value


def verify_records(output, rows, groups, omitted=()):
    expected = {row["id"]: row for row in rows if row["id"] not in omitted}
    records = {}
    for path in output.glob("projects/tidemux/*.jsonl"):
        for line in path.read_text().splitlines():
            record = json.loads(line, object_pairs_hook=unique_object)
            identity = record["requestId"]
            assert identity not in records, "terminal request logged twice"
            assert record["message"]["id"] == identity and record["type"] == "assistant"
            assert "costUSD" not in record and "tidemux" not in record
            assert record["schema_version"] == 2 and record["event"] == "request_terminal"
            assert all(key in record for key in ("level", "msg", "protocol", "provider_protocol", "provider_ref", "endpoint", "outcome", "error_code", "http_status", "latency_ms", "queue_time_ms", "upstream_attempted", "record_persisted"))
            assert not any(key in record for key in ("time", "request_id", "model"))
            group = groups[identity]
            assert path.stem == (group or "ungrouped") and record.get("sessionId", "") == group, (expected[identity].get("provider_ref"), path.stem, record.get("sessionId"), group)
            row = expected[identity]
            timestamp = record["timestamp"]
            assert len(timestamp) == 24 and timestamp.endswith("Z")
            parsed = datetime.strptime(timestamp, "%Y-%m-%dT%H:%M:%S.%fZ").replace(tzinfo=timezone.utc)
            assert round(parsed.timestamp() * 1000) == row["timestamp_ms"]
            assert record["message"]["model"] == row["model"]
            usage = record["message"].get("usage")
            if row.get("input_tokens") is None or row.get("output_tokens") is None:
                assert usage is None, "unknown usage fabricated as zero"
            else:
                assert usage["output_tokens"] == row["output_tokens"]
                assert usage["input_tokens"] + usage.get("cache_read_input_tokens", 0) + usage.get("cache_creation_input_tokens", 0) == row["input_tokens"]
                assert usage.get("cache_read_input_tokens") == row.get("cache_read_tokens")
                assert usage.get("cache_creation_input_tokens") == row.get("cache_write_tokens")
            records[identity] = record
    assert records.keys() == expected.keys(), "logs differ from terminal requests"
    return records


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--evidence", type=Path)
    args = parser.parse_args()
    binary = args.binary.resolve()
    evidence = args.evidence.resolve() if args.evidence else None
    if evidence:
        evidence.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="tidemux-usage-logs.") as temporary:
        root = Path(temporary)
        fake = root / "bin"
        fake.mkdir()
        security = fake / "security"
        security.write_text("#!/bin/sh\ncase \"$*\" in *test.gateway*) printf 'local-gateway-key\\n';; *test.provider*) printf 'local-provider-key\\n';; *) exit 1;; esac\n")
        security.chmod(0o700)
        home = root / "home"
        home.mkdir()
        env = {"PATH": str(fake) + ":/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin", "HOME": str(home), "TMPDIR": str(root), "LANG": "C.UTF-8"}
        upstream = Server(("127.0.0.1", 0), Mock)
        upstream.started = threading.Event()
        upstream.release = threading.Event()
        threading.Thread(target=upstream.serve_forever, daemon=True).start()
        port = free_port()
        url = f"http://127.0.0.1:{port}"
        ledger, config, output = root / "ledger.db", root / "config.json", root / "logs"
        providers = {}
        for name in ("usd", "eur", "unknown", "partial", "error", "native"):
            providers[name] = {"protocol": "anthropic" if name == "native" else "openai",
                               "base_url": f"http://127.0.0.1:{upstream.server_port}/{name}/v1",
                               "upstream_keychain": {"service": "test.provider", "account": "local"}, "supported_models": [MODEL]}
            if name in ("usd", "eur"):
                providers[name]["prices"] = {MODEL: {"currency": name.upper(), "source": "fixture", "version": "fixture-v1",
                                                    "input_cache_miss_per_million": 1, "input_cache_hit_per_million": .1, "output_per_million": 2}}
            if name == "usd":
                providers[name]["budget"] = {"currency": "USD", "five_hour_limit": 10, "weekly_limit": 10, "alert_threshold": .8, "mode": "hard"}
        value = {"listen_addr": f"127.0.0.1:{port}", "max_in_flight": 8, "ledger_path": str(ledger),
                 "access_token_keychain": {"service": "test.gateway", "account": "local"}, "providers": providers}
        config.write_text(json.dumps(value))
        process = None
        groups = {}

        def start():
            nonlocal process
            with (root / "stdout").open("a") as stdout, (root / "stderr").open("a") as stderr:
                process = subprocess.Popen([str(binary), "serve", "--config", str(config)], env=env, stdout=stdout, stderr=stderr)
            def ready():
                if process.poll() is not None:
                    raise RuntimeError("startup failed: " + (root / "stderr").read_text())
                try:
                    with urlopen(Request(url + "/v1/models", headers={"Authorization": "Bearer " + GATEWAY_KEY}), timeout=1) as response:
                        return response.status == 200
                except URLError:
                    return False
            wait_for(ready, "gateway readiness")

        def stop():
            nonlocal process
            if process is not None:
                process.send_signal(signal.SIGTERM)
                process.wait(timeout=10)
                process = None

        def request(provider, session=None, protocol="openai", stream=False):
            body = {"model": provider + "/" + MODEL, "max_tokens": 32, "messages": [{"role": "user", "content": PROMPT}], "stream": stream}
            headers = {"Authorization": "Bearer " + GATEWAY_KEY, "Content-Type": "application/json"}
            if session:
                if protocol == "anthropic":
                    body["metadata"] = {"user_id": json.dumps({"session_id": session})}
                else:
                    headers["X-TideMux-Session-ID"] = session
            req = Request(url + ("/v1/messages" if protocol == "anthropic" else "/v1/chat/completions"), json.dumps(body).encode(), headers)
            try:
                response = urlopen(req, timeout=20)
            except HTTPError as error:
                response = error
            identity = response.headers["X-TideMux-Request-ID"]
            effective_session = body["metadata"]["user_id"] if session and protocol == "anthropic" else session
            group = hmac.new(GATEWAY_KEY.encode(), ("tidemux-log-session\x00" + protocol + "\x00" + effective_session).encode(), hashlib.sha256).hexdigest() if effective_session else ""
            groups[identity] = group
            if stream:
                return response
            with response:
                return response.status, response.read(), identity

        def snapshot(name, omitted=()):
            rows = audits(ledger)
            records = verify_records(output, rows, groups, omitted)
            # Ledger schema stays unchanged; session identities exist only in logs.
            assert all("session_group" not in row and "usage_source" not in row for row in rows)
            assert not Path(str(ledger) + ".usage-key").exists()
            assert not Path(str(ledger) + ".usage-status.json").exists()
            assert not (output / "checkpoint.json").exists()
            if evidence:
                shutil.copytree(output / "projects", evidence / name / "projects")
                oracle = [dict(row, session_group=groups[row["id"]]) for row in rows if row["id"] not in omitted]
                (evidence / (name + "-ledger-oracle.json")).write_text(json.dumps(oracle, indent=2) + "\n")
                (evidence / (name + "-records.json")).write_text(json.dumps(list(records.values()), indent=2) + "\n")
            return rows, records

        try:
            start()
            assert not output.exists(), "logs were populated from a ledger scan"
            assert request("usd", SESSION)[0] == 200
            stop()
            snapshot("default")
            start()
            for provider, session in (("usd", SESSION), ("usd", SESSION), ("eur", SESSION), ("unknown", SESSION), ("partial", None), ("error", SESSION), ("native", SESSION)):
                code, _, _ = request(provider, session)
                assert code == (502 if provider == "error" else 200), (provider, code)
            assert request("usd", SESSION, "anthropic")[0] == 200
            stream = request("unknown", SESSION, stream=True)
            canceled_id = stream.headers["X-TideMux-Request-ID"]
            try:
                assert upstream.started.wait(5)
                stream.readline()
                stream.close()
                # Keep the upstream open until the gateway observes the client disconnect.
                wait_for(lambda: any(row["id"] == canceled_id and row["status"] == "canceled" for row in audits(ledger)), "canceled audit")
            finally:
                stream.close()
                upstream.release.set()
            stop()
            snapshot("initial")
            start()
            assert request("usd", SESSION)[0] == 200
            value["providers"]["reloaded"] = dict(providers["usd"])
            config.write_text(json.dumps(value))
            def reloaded():
                with urlopen(Request(url + "/v1/models", headers={"Authorization": "Bearer " + GATEWAY_KEY}), timeout=2) as response:
                    return any(model["id"] == "reloaded/" + MODEL for model in json.loads(response.read())["data"])
            wait_for(reloaded, "reloaded provider")
            assert request("reloaded", SESSION)[0] == 200
            stop()
            snapshot("restart")
            saved = root / "saved-logs"
            output.rename(saved)
            output.write_text("blocked output")
            start()
            assert request("usd", SESSION)[0] == 200
            omitted = {audits(ledger)[-1]["id"]}
            stop()
            assert '"event":"usage_log_write_failure"' in (root / "stderr").read_text()
            output.unlink()
            saved.rename(output)
            before = {str(path): path.read_bytes() for path in output.rglob("*") if path.is_file()}
            start()
            time.sleep(.2)
            assert before == {str(path): path.read_bytes() for path in output.rglob("*") if path.is_file()}, "restart replayed ledger rows"
            assert request("usd", SESSION)[0] == 200
            stop()
            rows, records = snapshot("final", omitted)
            assert len(rows) == 14 and len(records) == 13
            with sqlite3.connect(ledger) as db:
                settled = db.execute("SELECT audit_id,charged_amount FROM budget_charges WHERE provider_scope='usd' AND state='settled'").fetchall()
                by_id = {row["id"]: row for row in rows}
                assert len(settled) == sum(row.get("provider_ref") == "usd" for row in rows)
                assert all(math.isclose(amount, by_id[identity]["estimated_cost"], rel_tol=1e-12) for identity, amount in settled)
                assert db.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0] == sum(len(row["events"]) for row in rows)
            terminals = {event["requestId"]: event for event in (json.loads(line, object_pairs_hook=unique_object) for line in (root / "stderr").read_text().splitlines()) if event.get("event") == "request_terminal"}
            assert terminals.keys() == by_id.keys()
            for identity, record in records.items():
                assert terminals[identity] == record, "stderr and file log records differ"
                assert record["provider_ref"] == by_id[identity]["provider_ref"]
                assert record["latency_ms"] == by_id[identity]["latency_ms"]
            blob = b"".join(path.read_bytes() for path in output.rglob("*") if path.is_file()) + (root / "stdout").read_bytes() + (root / "stderr").read_bytes()
            for sentinel in (PROMPT, SESSION, RESPONSE, GATEWAY_KEY, PROVIDER_KEY, "private-error-sentinel"):
                assert sentinel.encode() not in blob, "private data leaked"
            for path in [output, *output.rglob("*")]:
                assert path.stat().st_mode & 0o077 == 0
            value["usage_log"] = {"directory": str(output)}
            config.write_text(json.dumps(value))
            rejected = subprocess.run([str(binary), "serve", "--config", str(config)], env=env, capture_output=True, text=True, timeout=10)
            assert rejected.returncode != 0, "removed export configuration accepted"
            result = {"result": "passed", "fixture_requests": len(rows), "logged_requests": len(records),
                      "default_direct_logging": True, "single_log_format": True, "stderr_file_identical": True, "no_export_state_or_config": True,
                      "session_restart_and_reload": True, "unknown_and_cache_counts": True,
                      "log_fault_request_and_budget_isolation": True, "no_history_replay": True, "privacy": True}
            if evidence:
                (evidence / "result.json").write_text(json.dumps(result, indent=2) + "\n")
            print(json.dumps(result))
        finally:
            stop()
            upstream.release.set()
            upstream.shutdown()
            upstream.server_close()


if __name__ == "__main__":
    main()
