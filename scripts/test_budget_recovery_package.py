#!/usr/bin/env python3
"""Verify packaged budget reset and known-cost recovery behavior offline.

The test runs the extracted macOS candidate with fake Keychain credentials and
a loopback OpenAI-compatible provider. It checks provider/window reset scoping,
refusal while the gateway is listening, and known-cost settlement when audit
append fails. Unknown settlement fail-closed behavior is covered by
test_budget_reservation_package.py.
"""
import argparse
import json
import os
from pathlib import Path
import socket
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.error import HTTPError
from urllib.request import Request, urlopen

GATEWAY_KEY = "local-gateway-key"
PROVIDER_KEY = "local-provider-key"


class Server(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True


class UpstreamHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def do_GET(self):
        self._write(200, {"object": "list", "data": [{"id": "custom-model", "object": "model"}]})

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        self.server.posts.append({
            "path": self.path,
            "body": json.loads(body),
            "fake_auth": self.headers.get("Authorization") == "Bearer " + PROVIDER_KEY,
        })
        self._write(200, {
            "id": "chatcmpl_budget_recovery",
            "object": "chat.completion",
            "created": 1,
            "model": "custom-model",
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": "local budget recovery response"},
                "finish_reason": "stop",
            }],
            "usage": {"prompt_tokens": 1, "completion_tokens": 1},
        })

    def _write(self, status, value):
        payload = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def start_server():
    server = Server(("127.0.0.1", 0), UpstreamHandler)
    server.posts = []
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def start_gateway(binary, config_path, port, env, root, name):
    log = (root / f"{name}.stderr").open("wb")
    process = subprocess.Popen(
        [str(binary), "serve", "--config", str(config_path)],
        cwd=root, env=env, stdout=subprocess.DEVNULL, stderr=log,
    )
    for _ in range(120):
        if process.poll() is not None:
            log.close()
            details = (root / f"{name}.stderr").read_text(encoding="utf-8", errors="replace")
            raise RuntimeError(f"packaged gateway exited during startup: {details[-2000:]}")
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                return process, log
        except OSError:
            time.sleep(0.05)
    process.terminate()
    process.wait(timeout=5)
    log.close()
    raise RuntimeError("packaged gateway did not become ready")


def stop_gateway(process, log):
    process.terminate()
    try:
        process.wait(timeout=8)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)
    log.close()


def post(base_url, provider, token=GATEWAY_KEY):
    request = Request(
        base_url + "/v1/chat/completions",
        data=json.dumps({
            "model": f"{provider}/custom-model",
            "messages": [{"role": "user", "content": "package budget recovery check"}],
        }).encode(),
        headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urlopen(request, timeout=15) as response:
            return response.status, response.read().decode()
    except HTTPError as error:
        return error.code, error.read().decode()


def reset(binary, provider, window, config_path, env, root):
    result = subprocess.run(
        [str(binary), "provider", "budget", "reset", provider, "--window", window, "--config", str(config_path)],
        cwd=root, env=env, text=True, capture_output=True, timeout=15,
    )
    return result


def read_resets(ledger):
    with sqlite3.connect(ledger, timeout=5) as connection:
        connection.execute("PRAGMA busy_timeout=5000")
        return connection.execute(
            "SELECT provider_scope, window_key FROM budget_resets ORDER BY provider_scope, window_key"
        ).fetchall()


def budget_rows(ledger, provider=None):
    with sqlite3.connect(ledger, timeout=5) as connection:
        connection.execute("PRAGMA busy_timeout=5000")
        if provider is None:
            return connection.execute(
                "SELECT request_id, provider_scope, charged_amount, state FROM budget_charges ORDER BY charged_at_ms, request_id"
            ).fetchall()
        return connection.execute(
            "SELECT request_id, provider_scope, charged_amount, state FROM budget_charges WHERE provider_scope=? ORDER BY charged_at_ms, request_id",
            (provider,),
        ).fetchall()


def assert_blocked(status, body, provider, code):
    if status != 429 or code not in body:
        raise RuntimeError(f"{provider} did not fail closed with {code}: HTTP {status}, body={body}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path, help="extracted TideMux candidate binary")
    args = parser.parse_args()
    if sys.platform != "darwin":
        raise SystemExit("This acceptance check requires the packaged macOS binary.")
    binary = args.binary.resolve()
    if not binary.is_file() or not os.access(binary, os.X_OK):
        raise SystemExit("--binary must point to an executable packaged candidate")

    upstream = start_server()
    gateway = None
    gateway_log = None
    try:
        with tempfile.TemporaryDirectory(prefix="tidemux-budget-recovery-package.") as temporary:
            root = Path(temporary)
            fake_bin = root / "bin"
            fake_bin.mkdir()
            security = fake_bin / "security"
            security.write_text(
                "#!/bin/sh\ncase \"$*\" in "
                "*test.gateway*) printf 'local-gateway-key\\n';; "
                "*test.provider*) printf 'local-provider-key\\n';; "
                "*) exit 1;; esac\n",
                encoding="utf-8",
            )
            security.chmod(0o755)
            home = root / "home"
            home.mkdir()
            port = free_port()
            ledger = root / "ledger.db"
            config = {
                "listen_addr": f"127.0.0.1:{port}",
                "max_in_flight": 2,
                "ledger_path": str(ledger),
                "access_token_keychain": {"service": "test.gateway", "account": "local"},
                "providers": {},
            }
            for provider in ("p1", "p2"):
                config["providers"][provider] = {
                    "protocol": "openai",
                    "base_url": f"http://127.0.0.1:{upstream.server_port}/v1",
                    "upstream_id": provider,
                    "upstream_keychain": {"service": "test.provider", "account": "local"},
                    "supported_models": ["custom-model"],
                    "prices": {"custom-model": {
                        "currency": "USD", "source": "local-mock", "version": "1",
                        "input_cache_hit_per_million": 0,
                        "input_cache_miss_per_million": 1,
                        "output_per_million": 1,
                    }},
                    "budget": {
                        "currency": "USD", "five_hour_limit": 1,
                        "weekly_limit": 2, "alert_threshold": 0.8, "mode": "hard",
                    },
                }
            config_path = root / "config.json"
            config_path.write_text(json.dumps(config), encoding="utf-8")
            env = {
                "PATH": str(fake_bin) + ":/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin",
                "HOME": str(home),
                "TMPDIR": str(root),
                "LANG": "C.UTF-8",
            }
            base_url = f"http://127.0.0.1:{port}"

            gateway, gateway_log = start_gateway(binary, config_path, port, env, root, "gateway-before-reset")
            now_ms = int(time.time() * 1000)
            with sqlite3.connect(ledger, timeout=5) as connection:
                connection.execute("PRAGMA busy_timeout=5000")
                connection.executemany(
                    "INSERT INTO budget_charges (request_id,audit_id,charged_at_ms,provider_scope,currency,charged_amount,state) VALUES (?, '', ?, ?, 'USD', ?, 'settled')",
                    [
                        ("seed-p1-recent", now_ms - 60 * 60 * 1000, "p1", 1.1),
                        ("seed-p1-weekly", now_ms - 6 * 60 * 60 * 1000, "p1", 1.1),
                        ("seed-p2-recent", now_ms - 60 * 60 * 1000, "p2", 1.1),
                    ],
                )
                connection.commit()

            refused = reset(binary, "p1", "5h", config_path, env, root)
            if refused.returncode == 0 or "gateway is still listening" not in refused.stderr:
                raise RuntimeError(f"running gateway reset was not refused: {refused.returncode}, {refused.stderr}")
            if read_resets(ledger):
                raise RuntimeError("refused reset changed budget reset state")
            stop_gateway(gateway, gateway_log)
            gateway = None

            reset_5h = reset(binary, "p1", "5h", config_path, env, root)
            if reset_5h.returncode != 0 or "Reset provider p1 budget window 5h" not in reset_5h.stdout:
                raise RuntimeError(f"p1 5h reset failed: {reset_5h.stderr or reset_5h.stdout}")
            if read_resets(ledger) != [("p1", "5h")]:
                raise RuntimeError(f"5h reset changed an unexpected provider/window: {read_resets(ledger)}")

            gateway, gateway_log = start_gateway(binary, config_path, port, env, root, "gateway-after-5h-reset")
            p1_status, p1_body = post(base_url, "p1")
            assert_blocked(p1_status, p1_body, "p1", "budget_hard_limit")
            p2_status, p2_body = post(base_url, "p2")
            assert_blocked(p2_status, p2_body, "p2", "budget_hard_limit")
            if upstream.posts:
                raise RuntimeError("budget-blocked request unexpectedly reached the provider")
            stop_gateway(gateway, gateway_log)
            gateway = None

            reset_7d = reset(binary, "p1", "7d", config_path, env, root)
            if reset_7d.returncode != 0 or "Reset provider p1 budget window 7d" not in reset_7d.stdout:
                raise RuntimeError(f"p1 7d reset failed: {reset_7d.stderr or reset_7d.stdout}")
            expected_resets = [("p1", "5h"), ("p1", "7d")]
            if read_resets(ledger) != expected_resets:
                raise RuntimeError(f"7d reset changed an unexpected provider/window: {read_resets(ledger)}")

            gateway, gateway_log = start_gateway(binary, config_path, port, env, root, "gateway-after-7d-reset")
            p1_status, p1_body = post(base_url, "p1")
            if p1_status != 200 or "local budget recovery response" not in p1_body or len(upstream.posts) != 1:
                raise RuntimeError(f"p1 remained blocked after its 7d reset: HTTP {p1_status}, {p1_body}")
            p2_status, p2_body = post(base_url, "p2")
            assert_blocked(p2_status, p2_body, "p2", "budget_hard_limit")
            if len(upstream.posts) != 1:
                raise RuntimeError("p2 reset leaked from p1 or a blocked request reached upstream")
            if not all(item["fake_auth"] and item["path"] == "/v1/chat/completions" for item in upstream.posts):
                raise RuntimeError("upstream did not see only authenticated loopback provider requests")

            before_failure = {row[0] for row in budget_rows(ledger, "p1")}
            successful_rows = [
                row for row in budget_rows(ledger, "p1")
                if row[0] not in {"seed-p1-recent", "seed-p1-weekly"} and row[3] == "settled"
            ]
            if len(successful_rows) != 1 or successful_rows[0][2] <= 0:
                raise RuntimeError(f"expected one known successful package charge before audit-failure injection: {successful_rows}")
            known_cost = successful_rows[0][2]
            limit = 1.0
            threshold_charge = limit - 1.5 * known_cost
            if threshold_charge <= 0:
                raise RuntimeError(f"known test charge is too large to construct a budget-boundary case: {known_cost}")
            with sqlite3.connect(ledger, timeout=5) as connection:
                connection.execute("PRAGMA busy_timeout=5000")
                connection.execute(
                    "INSERT INTO budget_charges (request_id,audit_id,charged_at_ms,provider_scope,currency,charged_amount,state) VALUES ('seed-known-cost-threshold','',?,'p1','USD',?,'settled')",
                    (int(time.time() * 1000), threshold_charge),
                )
                connection.execute(
                    "CREATE TRIGGER fail_audit_append BEFORE INSERT ON request_audit BEGIN SELECT RAISE(FAIL, 'injected audit append failure'); END"
                )
                connection.commit()

            failed_status, failed_body = post(base_url, "p1")
            if failed_status != 500 or "audit_failed_do_not_retry_blindly" not in failed_body or len(upstream.posts) != 2:
                raise RuntimeError(
                    f"audit-append failure was not surfaced after provider dispatch: HTTP {failed_status}, "
                    f"body={failed_body}, upstream_posts={len(upstream.posts)}, known_cost={known_cost}, "
                    f"threshold_charge={threshold_charge}, p1_budget_rows={budget_rows(ledger, 'p1')}"
                )
            new_rows = [row for row in budget_rows(ledger, "p1") if row[0] not in before_failure and row[0] != "seed-known-cost-threshold"]
            if len(new_rows) != 1 or new_rows[0][3] != "settled" or new_rows[0][2] <= 0:
                raise RuntimeError(f"known cost after audit failure was not settled: {new_rows}")

            blocked_status, blocked_body = post(base_url, "p1")
            assert_blocked(blocked_status, blocked_body, "p1", "budget_hard_limit")
            if "budget_usage_unknown" in blocked_body or len(upstream.posts) != 2:
                raise RuntimeError("known-cost settlement became unknown usage or dispatched another request")
            settled = [row for row in budget_rows(ledger, "p1") if row[0] not in before_failure and row[0] != "seed-known-cost-threshold"]
            if len(settled) != 1 or settled[0][3] != "settled":
                raise RuntimeError(f"budget row changed after fail-closed follow-up: {settled}")

            print(json.dumps({
                "binary": str(binary),
                "running_gateway_reset_refused_without_state_change": True,
                "provider_scoped_5h_reset": True,
                "5h_reset_preserved_7d_limit": True,
                "provider_scoped_7d_reset": True,
                "other_provider_remained_blocked": True,
                "known_cost_before_failure": known_cost,
                "known_cost_after_audit_failure_state": settled[0][3],
                "known_cost_after_audit_failure_amount": settled[0][2],
                "followup_failed_closed_as": "budget_hard_limit",
                "unknown_usage_not_reported": True,
                "loopback_upstream_posts": len(upstream.posts),
                "fake_credentials_only": True,
            }, ensure_ascii=False))
    finally:
        if gateway is not None:
            stop_gateway(gateway, gateway_log)
        upstream.shutdown()
        upstream.server_close()


if __name__ == "__main__":
    main()
