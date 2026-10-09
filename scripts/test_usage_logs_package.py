#!/usr/bin/env python3
"""Verify default usage JSONL with a packaged binary, fake keys and loopback mocks."""
import argparse
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler
import json
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
            self.server.release.wait(10)
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


def verify_records(output, rows, ledger):
    latest = {}
    identities = {}
    for path in sorted(output.glob("projects/tidemux/*/usage-*.jsonl")):
        for line in path.read_text().splitlines():
            record = json.loads(line)
            metadata = record["tidemux"]
            identity = metadata["request_id"]
            assert metadata["schema_version"] == 2 and metadata["source"] == "tidemux"
            assert record["type"] == "assistant" and "costUSD" not in record
            group = metadata["session_group"]
            assert path.parent.name == (group or "ungrouped")
            assert record.get("sessionId") == group
            assert record["requestId"] == record["message"]["id"] == "tidemux-" + metadata["source_id"] + "-" + identity
            if identity in identities:
                assert identities[identity] == record["requestId"], "replayed snapshot changed identity"
            identities[identity] = record["requestId"]
            previous = latest.get(identity)
            if previous is None or metadata["revision"] > previous["tidemux"]["revision"]:
                latest[identity] = record
            elif metadata["revision"] == previous["tidemux"]["revision"]:
                assert record == previous, "conflicting replay snapshot"
    assert latest.keys() == {row["id"] for row in rows}, "export differs from committed ledger"
    with sqlite3.connect(ledger) as db:
        supplier = {row[0]: row[1:] for row in db.execute(
            "SELECT request_id,MAX(statement_id),COUNT(*),SUM(amount) FROM reconciliation_statements WHERE status='matched' GROUP BY request_id")}
    for row in rows:
        record = latest[row["id"]]
        metadata = record["tidemux"]
        for key in ("input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "estimated_cost"):
            assert metadata[key] == row.get(key), (row["id"], key)
        assert metadata["currency"] == (row.get("currency") or None)
        assert metadata["provider"] == row.get("provider_ref", "")
        assert metadata["protocol"] == row["protocol"] and metadata["outcome"] == row["status"]
        assert metadata["usage_source"] == (row.get("usage_source") or "unknown")
        revision, lines, amount = supplier.get(row["id"], (0, 0, None))
        assert (metadata["revision"], metadata["supplier_statement_lines"], metadata["supplier_amount"]) == (revision, lines, amount)
        timestamp = record["timestamp"]
        assert len(timestamp) == 24 and timestamp.endswith("Z"), "incompatible millisecond precision"
        parsed = datetime.strptime(timestamp, "%Y-%m-%dT%H:%M:%S.%fZ").replace(tzinfo=timezone.utc)
        assert round(parsed.timestamp() * 1000) == row["timestamp_ms"]
        assert record["message"]["model"] == row["model"]
        usage = record["message"].get("usage")
        if row.get("input_tokens") is None or row.get("output_tokens") is None:
            assert usage is None, "unknown usage fabricated as zero"
        else:
            assert usage["output_tokens"] == row["output_tokens"]
            assert usage["input_tokens"] + usage.get("cache_read_input_tokens", 0) + usage.get("cache_creation_input_tokens", 0) == row["input_tokens"]
            for exported, original in (("cache_read_input_tokens", "cache_read_tokens"), ("cache_creation_input_tokens", "cache_write_tokens")):
                assert usage.get(exported) == row.get(original)
    return list(latest.values())


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
        ledger, config, output = root / "ledger.db", root / "config.json", root / "usage"
        price = lambda currency: {"currency": currency, "source": "fixture", "version": "fixture-v1",
                                  "input_cache_miss_per_million": 1, "input_cache_hit_per_million": .1, "output_per_million": 2}
        providers = {}
        for name in ("usd", "eur", "unknown", "partial", "error", "native"):
            providers[name] = {"protocol": "anthropic" if name == "native" else "openai",
                               "base_url": f"http://127.0.0.1:{upstream.server_port}/{name}/v1",
                               "upstream_keychain": {"service": "test.provider", "account": "local"}, "supported_models": [MODEL]}
            if name in ("usd", "eur"):
                providers[name]["prices"] = {MODEL: price(name.upper())}
            if name == "usd":
                providers[name]["budget"] = {"currency": "USD", "five_hour_limit": 10, "weekly_limit": 10, "alert_threshold": .8, "mode": "hard"}
        value = {"listen_addr": f"127.0.0.1:{port}", "max_in_flight": 8, "ledger_path": str(ledger),
                 "access_token_keychain": {"service": "test.gateway", "account": "local"},
                 "providers": providers, "reconciliation": {"poll_interval_seconds": 1}}
        config.write_text(json.dumps(value))
        process = None

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
            if stream:
                return response
            with response:
                return response.status, response.read()

        def exported():
            status = Path(str(ledger) + ".usage-status.json")
            if not status.exists():
                return False
            state = json.loads(status.read_text())
            assert "enabled" not in state, "removed enablement state remains"
            with sqlite3.connect(ledger) as db:
                audit_id = db.execute("SELECT COALESCE(MAX(rowid),0) FROM request_audit").fetchone()[0]
                statement_id = db.execute("SELECT COALESCE(MAX(statement_id),0) FROM reconciliation_statements").fetchone()[0]
            return not state.get("error_code") and state["audit_rowid"] == audit_id and state["statement_id"] == statement_id

        def snapshot(name):
            rows = audits(ledger)
            records = verify_records(output, rows, ledger)
            if evidence:
                shutil.copytree(output / "projects", evidence / name / "projects")
                (evidence / (name + "-ledger-oracle.json")).write_text(json.dumps(rows, indent=2) + "\n")
                (evidence / (name + "-records.json")).write_text(json.dumps(records, indent=2) + "\n")
            return rows

        try:
            help_result = subprocess.run([str(binary)], env=env, capture_output=True, text=True, timeout=10)
            assert help_result.returncode != 0 and "usage       " not in help_result.stderr
            removed = subprocess.run([str(binary), "usage", "status", "--config", str(config)], env=env, capture_output=True, text=True, timeout=10)
            assert removed.returncode != 0, "usage command remains installed"
            start()
            assert request("usd", SESSION)[0] == 200
            wait_for(exported, "default export without usage_log configuration")
            stop()
            assert output.is_dir() and Path(str(ledger) + ".usage-key").is_file()
            assert all(row.get("session_group") for row in audits(ledger))
            snapshot("default")
            # Rebuild test-owned output from committed history, using the default
            # directory and only retention/rotation overrides, without a switch.
            shutil.rmtree(output)
            value["usage_log"] = {"max_bytes": 4096, "max_files": 64}
            config.write_text(json.dumps(value))
            start()
            for provider, session in (("usd", SESSION), ("usd", SESSION), ("eur", SESSION), ("unknown", SESSION), ("partial", None), ("error", SESSION), ("native", SESSION)):
                code, _ = request(provider, session)
                assert code == (502 if provider == "error" else 200), (provider, code)
            assert request("usd", SESSION, "anthropic")[0] == 200
            stream = request("unknown", SESSION, stream=True)
            assert upstream.started.wait(5)
            stream.readline()
            stream.close()
            upstream.release.set()
            wait_for(lambda: any(row["status"] == "canceled" for row in audits(ledger)), "canceled audit")
            wait_for(exported, "committed export")
            stop()
            rows = snapshot("initial")
            groups = {row.get("session_group") for row in rows if row.get("session_group")}
            assert len(groups) == 2
            start()
            assert request("usd", SESSION)[0] == 200
            wait_for(exported, "restart export")
            stop()
            rows = audits(ledger)
            assert {row.get("session_group") for row in rows if row.get("session_group")} == groups
            target = next(row for row in rows if row.get("currency") == "EUR")
            statements = root / "statements"
            statements.mkdir(exist_ok=True)
            (statements / "fixture.csv").write_text(f"period_start,period_end,currency,amount,request_id,model\n{target['timestamp_ms']-1},{target['timestamp_ms']+1},EUR,2.25,{target['id']},{MODEL}\n")
            start()
            def matched():
                with sqlite3.connect(ledger) as db:
                    return db.execute("SELECT COUNT(*) FROM reconciliation_statements WHERE status='matched'").fetchone()[0] == 1
            wait_for(matched, "supplier snapshot")
            wait_for(exported, "supplier export")
            stop()
            snapshot("supplier")
            checkpoint = output / "checkpoint.json"
            state = json.loads(checkpoint.read_text())
            latest = max(output.glob("projects/tidemux/*/usage-*.jsonl"), key=lambda path: path.name)
            with latest.open("ab") as file:
                file.write(b'{"crash_half":')
            checkpoint.unlink()
            start()
            wait_for(lambda: checkpoint.exists() and exported(), "checkpoint replay and tail recovery")
            stop()
            snapshot("replay")
            checkpoint.write_text(json.dumps(state))
            checkpoint.chmod(0o600)
            blocked = root / "blocked"
            blocked.write_text("output fault")
            value["usage_log"]["directory"] = str(blocked)
            config.write_text(json.dumps(value))
            start()
            assert request("usd", SESSION)[0] == 200
            status = Path(str(ledger) + ".usage-status.json")
            wait_for(lambda: json.loads(status.read_text()).get("error_code") == "usage_output_unavailable", "output failure isolation")
            stop()
            value["usage_log"]["directory"] = str(output)
            config.write_text(json.dumps(value))
            key = Path(str(ledger) + ".usage-key")
            saved = key.read_bytes()
            key.write_bytes(b"invalid-key")
            start()
            assert request("usd", SESSION)[0] == 200
            wait_for(lambda: json.loads(status.read_text()).get("error_code") == "usage_identity_unavailable", "identity failure isolation")
            stop()
            assert not audits(ledger)[-1].get("session_group")
            key.write_bytes(saved)
            start()
            wait_for(exported, "fault recovery catchup")
            stop()
            rows = snapshot("recovered")
            assert len(rows) == 13
            with sqlite3.connect(ledger) as db:
                settled = db.execute("SELECT audit_id,charged_amount FROM budget_charges WHERE provider_scope='usd' AND state='settled'").fetchall()
                assert len(settled) == sum(row.get("provider_ref") == "usd" for row in rows)
                by_id = {row["id"]: row for row in rows}
                assert all(math.isclose(amount, by_id[identity]["estimated_cost"], rel_tol=1e-12) for identity, amount in settled)
                assert db.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0] == sum(len(row["events"]) for row in rows)
            blob = b"".join(path.read_bytes() for path in output.rglob("*") if path.is_file()) + (root / "stdout").read_bytes() + (root / "stderr").read_bytes()
            for sentinel in (PROMPT, SESSION, RESPONSE, GATEWAY_KEY, PROVIDER_KEY, "private-error-sentinel"):
                assert sentinel.encode() not in blob, "private data leaked"
            for path in [output, *output.rglob("*")]:
                assert path.stat().st_mode & 0o077 == 0
            before = {str(path): path.read_bytes() for path in output.rglob("*") if path.is_file()}
            for enabled in (False, True):
                value["usage_log"]["enabled"] = enabled
                config.write_text(json.dumps(value))
                rejected = subprocess.run([str(binary), "serve", "--config", str(config)], env=env,
                                          capture_output=True, text=True, timeout=10)
                assert rejected.returncode != 0, "removed usage_log.enabled switch was accepted"
            del value["usage_log"]["enabled"]
            config.write_text(json.dumps(value))
            assert before == {str(path): path.read_bytes() for path in output.rglob("*") if path.is_file()}
            assert len(audits(ledger)) == len(rows)
            result = {"result": "passed", "fixture_requests": len(rows), "default_on": True,
                      "automatic_history": True, "session_restart": True, "unknown_and_multicurrency": True,
                      "supplier_revision_and_replay": True, "half_tail_recovery": True,
                      "output_and_identity_fault_isolation": True, "fault_recovery_catchup": True,
                      "budget_and_audit_events": True, "removed_switch_rejected": True, "privacy": True}
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
