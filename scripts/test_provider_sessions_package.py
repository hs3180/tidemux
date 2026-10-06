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
import socket
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

    def reply(self, body, code=200):
        data = json.dumps(body).encode()
        self.send_response(code)
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
            exhausted = self.server.exhausted
        if exhausted:
            self.reply({"error": {"code": "insufficient_balance", "message": "synthetic"}}, 402)
            return
        if body.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            if self.server.protocol == "anthropic":
                self.wfile.write(b'event: message_start\ndata: {"type":"message_start","message":{"id":"held","type":"message","role":"assistant","model":"custom-model","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}\n\nevent: content_block_start\ndata: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}\n\nevent: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"held"}}\n\n')
            else:
                self.wfile.write(b'data: {"choices":[{"index":0,"delta":{"content":"held"}}]}\n\n')
            self.wfile.flush()
            self.server.started.set()
            self.server.release.wait(15)
            if self.server.protocol == "anthropic":
                self.wfile.write(b'event: content_block_stop\ndata: {"type":"content_block_stop","index":0}\n\nevent: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}\n\nevent: message_stop\ndata: {"type":"message_stop"}\n\n')
            else:
                self.wfile.write(b'data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}\n\ndata: [DONE]\n\n')
            self.wfile.flush()
            return
        if self.server.protocol == "anthropic":
            self.reply({"id": "test", "type": "message", "role": "assistant", "model": body["model"], "content": [{"type": "text", "text": "ok"}], "stop_reason": "end_turn", "usage": {"input_tokens": 3, "output_tokens": 2, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}})
        else:
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


def new_upstream(protocol="openai"):
    upstream = Server(("127.0.0.1", 0), Upstream)
    upstream.lock, upstream.calls, upstream.exhausted = threading.Lock(), 0, False
    upstream.protocol = protocol
    upstream.started, upstream.release = threading.Event(), threading.Event()
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    return upstream


def run(binary, root, protocol, cap, global_cap, action, other_upstream=None, *, max_in_flight=32, idle_seconds=0, provider_budget=False, canceled_audits=0):
    upstream = new_upstream(other_upstream.protocol if other_upstream else "openai")
    env = dict(os.environ, PATH=str(root / "bin") + ":" + os.environ.get("PATH", ""))
    port = free_port()
    url = f"http://127.0.0.1:{port}"
    endpoint = f"http://127.0.0.1:{upstream.server_port}/v1"
    config, ledger = root / "config.json", root / "ledger.db"
    provider = {"protocol": upstream.protocol, "base_url": endpoint, "upstream_keychain": {"service": "test.provider", "account": "local"}, "supported_models": ["custom-model", "other-model"]}
    value = {"listen_addr": f"127.0.0.1:{port}", "max_in_flight": max_in_flight, "max_active_sessions": global_cap, "active_session_idle_timeout_seconds": idle_seconds, "ledger_path": str(ledger), "access_token_keychain": {"service": "test.gateway", "account": "local"}, "providers": {"glm": dict(provider, max_active_sessions=cap), "other": provider}}
    if provider_budget:
        value["providers"]["glm"].update(
            prices={"custom-model": {"currency": "USD", "source": "synthetic", "version": "capacity-test", "input_cache_miss_per_million": 1, "input_cache_hit_per_million": .1, "output_per_million": 2}},
            budget={"currency": "USD", "five_hour_limit": 100, "weekly_limit": 100, "alert_threshold": .8, "mode": "hard"})
    if other_upstream is not None:
        value["routing"] = {"billing_exhaustion_failover": True}
        value["providers"]["other"] = dict(provider, base_url=f"http://127.0.0.1:{other_upstream.server_port}/v1", max_active_sessions=1,
            error_code_mappings=[{"upstream_code": "insufficient_balance", "http_status": 402, "category": "insufficient_balance"}],
            prices={"custom-model": {"currency": "USD", "source": "synthetic", "version": "capacity-test", "input_cache_miss_per_million": 1, "input_cache_hit_per_million": .1, "output_per_million": 2}},
            budget={"currency": "USD", "five_hour_limit": 100, "weekly_limit": 100, "alert_threshold": .8, "mode": "hard"})
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
        dispatched = upstream.calls + (other_upstream.calls if other_upstream else 0)
        if audit_count != dispatched + canceled_audits:
            raise RuntimeError(f"dispatch/audit mismatch: {dispatched}/{audit_count}")
        return {"protocol": protocol, "upstream_calls": dispatched, "audits": audit_count, "queued_canceled_audits": canceled_audits}
    finally:
        upstream.release.set()
        if process and process.poll() is None:
            process.kill()
            process.wait()
        upstream.shutdown()
        upstream.server_close()


def failover_action(source, protocol, retain_source):
    def action(url, target, config, env):
        retained = PRIVATE_SESSION + "-source-retained"
        occupied = PRIVATE_SESSION + "-target-occupied"
        if query(url, protocol, occupied)[0] != 200 or (retain_source and query(url, protocol, retained, "other/custom-model")[0] != 200):
            raise RuntimeError("failover fixture did not retain both provider sessions")
        ledger = config.parent / "ledger.db"
        def snapshot():
            with sqlite3.connect(ledger) as connection:
                audits = dict(connection.execute("SELECT id,record_json FROM request_audit"))
                charges = {row[0]: row[1:] for row in connection.execute("SELECT request_id,state,charged_amount FROM budget_charges")}
            return audits, charges
        def settled_snapshot(want_audits, want_charges):
            def finalized():
                audits, charges = snapshot()
                return len(audits) == want_audits and len(charges) == want_charges and all(row[0] == "settled" for row in charges.values())
            wait_for(finalized, "exact finalized failover audit/budget rows")
            return snapshot()
        def counts():
            with source.lock, target.lock:
                return source.calls, target.calls
        def occupancy():
            state = status(url)
            if state["gateway"]["current"] != 1 + int(retain_source) or state["providers"]["glm"]["current"] != 1 or state["providers"]["other"]["current"] != int(retain_source):
                raise RuntimeError("failover refusal lost retained/in-flight occupancy or leaked a new global lease")
        original_audits, original_charges = settled_snapshot(1 + int(retain_source), int(retain_source))
        if len(original_audits) != 1 + int(retain_source) or len(original_charges) != int(retain_source) or any(row[0] != "settled" for row in original_charges.values()):
            raise RuntimeError("failover fixture accounting did not settle")
        previous_audits, previous_charges = original_audits, original_charges
        with request(url, protocol, occupied, stream=True) as held:
            try:
                if not target.started.wait(5):
                    raise RuntimeError("failover target held SSE did not begin")
                occupancy()
                with source.lock:
                    source.exhausted = True
                before_source, before_target = counts()
                reject(query(url, protocol, retained, "other/custom-model"), "provider", 1)
                if counts() != (before_source + 1, before_target):
                    raise RuntimeError("balance failover refusal dispatched target or lost its prior source attempt")
                occupancy()
                current_audits, current_charges = settled_snapshot(2 + int(retain_source), 1 + int(retain_source))
                if not previous_audits.items() <= current_audits.items() or not previous_charges.items() <= current_charges.items():
                    raise RuntimeError("capacity refusal rewrote an existing audit or budget settlement")
                added = current_audits.keys() - previous_audits.keys()
                if len(current_audits) != 2 + int(retain_source) or len(added) != 1 or len(current_charges) != 1 + int(retain_source) or any(row[0] != "settled" for row in current_charges.values()):
                    raise RuntimeError("failover attempt audit/settled/pending totals are incorrect")
                record = json.loads(current_audits[next(iter(added))])
                if record.get("provider_ref") != "other" or record.get("error_code") != "provider_insufficient_balance" or record.get("status") != "error":
                    raise RuntimeError("prior balance failure audit was not retained with its provider/error")
                previous_audits, previous_charges = current_audits, current_charges
                target.release.set()
                tail = held.read().decode()
                if ("[DONE]" if protocol == "openai" else "message_stop") not in tail or "event: error" in tail:
                    raise RuntimeError("target's old in-flight stream was canceled by another session's failover refusal")
            finally:
                target.release.set()
        occupancy()
        final_audits, final_charges = settled_snapshot(3 + int(retain_source), 1 + int(retain_source))
        if counts() != (1 + int(retain_source), 2) or len(final_audits) != 3 + int(retain_source) or len(final_charges) != 1 + int(retain_source) or not previous_audits.items() <= final_audits.items() or final_charges != previous_charges:
            raise RuntimeError("held-stream completion lost unique dispatch/audit/budget history")
    return action


def anonymous_idle_action(protocol):
    def action(url, upstream, config, env):
        responses = []
        futures = []
        try:
            with ThreadPoolExecutor(max_workers=16) as pool:
                futures = [pool.submit(request, url, protocol, None, stream=True) for _ in range(16)]
                for future in futures:
                    responses.append(future.result(timeout=25))
            admitted = [response for response in responses if response.status == 200]
            rejected = [response for response in responses if response.status == 429]
            if len(admitted) != 1 or len(rejected) != 15 or upstream.calls != 1:
                raise RuntimeError("anonymous concurrent last-slot admission exceeded one dispatch")
            for response in rejected:
                reject((response.status, json.loads(response.read())), "provider", 1)
            if status(url)["providers"]["glm"]["current"] != 1:
                raise RuntimeError("anonymous held request lost provider occupancy")
            upstream.release.set()
            tail = admitted[0].read().decode()
            if ("[DONE]" if protocol == "openai" else "message_stop") not in tail:
                raise RuntimeError("anonymous held stream did not finish")
        finally:
            upstream.release.set()
            for response in responses:
                response.close()
            for future in futures:
                if future.done() and not future.cancelled() and future.exception() is None:
                    future.result().close()
        wait_for(lambda: status(url)["providers"]["glm"]["current"] == 0, "anonymous slot release without idle retention")
        if query(url, protocol, None)[0] != 200 or upstream.calls != 2:
            raise RuntimeError("anonymous released slot could not be reused")
        wait_for(lambda: status(url)["providers"]["glm"]["current"] == 0, "second anonymous release")
        stable = PRIVATE_SESSION + "-stable-idle"
        if query(url, protocol, stable)[0] != 200:
            raise RuntimeError("stable session did not retain its initial slot")
        upstream.started.clear()
        upstream.release.clear()
        with request(url, protocol, stable, stream=True) as held:
            try:
                if not upstream.started.wait(5):
                    raise RuntimeError("stable held SSE did not begin")
                time.sleep(1.1)  # Beyond the configured one-second idle TTL.
                reject(query(url, protocol, PRIVATE_SESSION + "-after-idle"), "provider", 1)
                if status(url)["providers"]["glm"]["current"] != 1 or upstream.calls != 4:
                    raise RuntimeError("idle scan expired an in-flight stable session")
                upstream.release.set()
                tail = held.read().decode()
                if ("[DONE]" if protocol == "openai" else "message_stop") not in tail:
                    raise RuntimeError("stable held stream did not finish")
            finally:
                upstream.release.set()
        if status(url)["providers"]["glm"]["current"] != 1:
            raise RuntimeError("stable slot did not retain activity after held SSE finished")
        wait_for(lambda: status(url)["providers"]["glm"]["current"] == 0, "stable idle expiration after final output")
        if query(url, protocol, PRIVATE_SESSION + "-after-idle")[0] != 200 or upstream.calls != 5:
            raise RuntimeError("idle-released slot could not admit a new session")
    return action


def queued_cancel_action(protocol):
    def action(url, upstream, config, env):
        ledger = config.parent / "ledger.db"
        def snapshot():
            with sqlite3.connect(ledger) as connection:
                return dict(connection.execute("SELECT id,record_json FROM request_audit")), {row[0]: row[1:] for row in connection.execute("SELECT request_id,state,charged_amount FROM budget_charges")}
        def occupancy(count):
            state = status(url)
            return state["gateway"]["current"] == count and state["providers"]["glm"]["current"] == count
        queued = None
        with request(url, protocol, PRIVATE_SESSION + "-queue-held", stream=True) as held:
            try:
                if not upstream.started.wait(5):
                    raise RuntimeError("queue holder did not dispatch")
                wait_for(lambda: len(snapshot()[1]) == 1, "held budget reservation")
                original_audits, original_charges = snapshot()
                if original_audits or any(row[0] != "pending" for row in original_charges.values()):
                    raise RuntimeError("held queue fixture finalized prematurely")
                port = int(url.rsplit(":", 1)[1])
                queued = socket.create_connection(("127.0.0.1", port), timeout=5)
                body = json.dumps({"model": "glm/custom-model", "max_tokens": 16, "messages": [{"role": "user", "content": PRIVATE_PROMPT}]}).encode()
                path = "/v1/messages" if protocol == "anthropic" else "/v1/chat/completions"
                headers = f"POST {path} HTTP/1.1\r\nHost: 127.0.0.1:{port}\r\nAuthorization: Bearer {GATEWAY_KEY}\r\nContent-Type: application/json\r\nX-TideMux-Session-ID: {PRIVATE_SESSION}-queue-canceled\r\nContent-Length: {len(body)}\r\nConnection: close\r\n\r\n"
                queued.sendall(headers.encode() + body)
                wait_for(lambda: occupancy(2) and len(snapshot()[1]) == 2, "second provider/global lease and queued budget reservation")
                if upstream.calls != 1:
                    raise RuntimeError("queued request dispatched before cancellation")
                queued.shutdown(socket.SHUT_RDWR)
                queued.close()
                queued = None
                wait_for(lambda: occupancy(1), "queued cancellation released only its own provider/global leases")
                wait_for(lambda: len(snapshot()[0]) == 1 and snapshot()[1] == original_charges, "canceled audit and deleted undispatched reservation")
                canceled_audits, charges = snapshot()
                canceled_id, raw = next(iter(canceled_audits.items()))
                record = json.loads(raw)
                if record.get("status") != "canceled" or record.get("error_code") != "request_canceled" or record.get("events") != ["queue_wait"] or upstream.calls != 1:
                    raise RuntimeError("queued cancellation was not exactly one undispatched canceled attempt")
                def canceled_terminal():
                    events = [json.loads(line) for line in (config.parent / "stderr").read_text().splitlines() if line]
                    return [event for event in events if event.get("event") == "request_terminal" and event.get("request_id") == canceled_id]
                wait_for(lambda: len(canceled_terminal()) == 1, "queued cancellation terminal event")
                terminal = canceled_terminal()
                if len(terminal) != 1 or terminal[0].get("upstream_attempted") is not False or terminal[0].get("record_persisted") is not True:
                    raise RuntimeError("queued cancellation lacks explicit zero-dispatch terminal metadata")
                upstream.release.set()
                tail = held.read().decode()
                if ("[DONE]" if protocol == "openai" else "message_stop") not in tail:
                    raise RuntimeError("queue holder did not complete after cancellation")
            finally:
                upstream.release.set()
                if queued is not None:
                    queued.close()
        wait_for(lambda: len(snapshot()[0]) == 2 and all(row[0] == "settled" for row in snapshot()[1].values()), "held stream's unique audit and settlement")
        if query(url, protocol, PRIVATE_SESSION + "-queue-canceled")[0] != 200:
            raise RuntimeError("canceled queue session could not be readmitted")
        wait_for(lambda: len(snapshot()[0]) == 3 and len(snapshot()[1]) == 2 and all(row[0] == "settled" for row in snapshot()[1].values()), "readmitted queue session's unique audit and settlement")
        final_audits, final_charges = snapshot()
        if upstream.calls != 2 or not canceled_audits.items() <= final_audits.items() or len(final_charges) != 2:
            raise RuntimeError("queued cancellation produced duplicate dispatch/audit or lost its retained history")
    return action


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    args = parser.parse_args()
    binary = args.binary.resolve()
    results = []
    with tempfile.TemporaryDirectory(prefix="tidemux-provider-capacity.") as temporary:
        base = Path(temporary)
        for protocol in ("openai", "anthropic"):
            for phase in ("caps", "reload", "failover_retained", "failover_new", "anonymous_idle", "queued_cancel"):
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
                        state = status(url)
                        if state["gateway"]["current"] != 5 or state["providers"]["glm"]["current"] != 5 or state["providers"]["other"]["current"] != 0:
                            raise RuntimeError("first provider refusal did not immediately roll back its global lease")
                        for session, model in ((PRIVATE_SESSION + "0", "glm/other-model"), (PRIVATE_SESSION + "different-sixth", "other/custom-model"), (PRIVATE_SESSION + "0", "other/custom-model")):
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
                elif phase == "reload":
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
                elif phase.startswith("failover"):
                    source = new_upstream(protocol)
                    try:
                        results.append(dict(run(binary, root, protocol, 1, 3, failover_action(source, protocol, phase == "failover_retained"), source), phase=phase))
                    finally:
                        source.release.set()
                        source.shutdown()
                        source.server_close()
                elif phase == "anonymous_idle":
                    results.append(dict(run(binary, root, protocol, 1, 0, anonymous_idle_action(protocol), idle_seconds=1), phase=phase))
                else:
                    results.append(dict(run(binary, root, protocol, 2, 2, queued_cancel_action(protocol), max_in_flight=1, provider_budget=True, canceled_audits=1), phase=phase))
    print(json.dumps({"binary": str(binary), "sha256": hashlib.sha256(binary.read_bytes()).hexdigest(), "results": results, "glm_five": True, "provider_and_global_isolation": True, "first_admission_global_rollback": True, "balance_failover_full_target_zero_dispatch": True, "prior_audit_budget_and_held_stream_preserved": True, "anonymous_concurrent_last_slot_and_release": True, "stable_inflight_ttl_and_idle_release": True, "queued_cancel_leases_budget_and_unique_canceled_audit": True, "concurrent_reuse": True, "unlimited_to_limited": True, "remove_readd_held_sse": True, "privacy": True}))


if __name__ == "__main__":
    main()
