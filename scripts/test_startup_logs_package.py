#!/usr/bin/env python3
"""Check early serve failures in a real candidate binary using isolated fixtures.

Uses fake security/Keychain credentials and loopback listeners only. It never
reads the user's Keychain, configuration or ledger, or contacts a real provider.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import time

from runtime_event_checks import validate_runtime_events


PRIVATE_PATH = "startup_private_path_sentinel"
PRIVATE_ARGUMENT = "startup_private_argument_sentinel"
PRIVATE_CONFIG = "startup_private_config_sentinel"
PRIVATE_KEY = "startup_private_credential_sentinel"
GATEWAY_KEY = "startup_fake_gateway_key"
PROVIDER_KEY = "startup_fake_provider_key"


def check_events(stdout, stderr, root, version):
    for marker in (str(root), PRIVATE_PATH, PRIVATE_ARGUMENT, PRIVATE_CONFIG,
                   PRIVATE_KEY, GATEWAY_KEY, PROVIDER_KEY):
        if marker in stdout or marker in stderr:
            raise RuntimeError("private fixture input reached process output")
    events = []
    for line in stderr.splitlines():
        if not line.strip():
            continue
        event = json.loads(line)
        if not isinstance(event, dict) or not all(
                field in event for field in ("timestamp", "level", "msg", "schema_version", "event")):
            raise RuntimeError("serve stderr does not match the JSON event envelope")
        if event["schema_version"] != 2:
            raise RuntimeError("unexpected runtime schema version")
        events.append(event)
    if events:
        validate_runtime_events(events, version)
    return events


def check_failure(binary, args, env, root, stage, code, version):
    result = subprocess.run([str(binary), "serve", *args], env=env, cwd=root,
                            capture_output=True, text=True, timeout=20)
    events = check_events(result.stdout, result.stderr, root, version)
    failures = [event for event in events if event.get("event") == "gateway_start"
                and event.get("outcome") == "error"]
    if result.returncode != 1 or len(failures) != 1:
        raise RuntimeError("startup did not produce exactly one failure and exit 1")
    event = failures[0]
    if event.get("startup_stage") != stage or event.get("error_code") != code:
        raise RuntimeError("wrong startup failure stage/code: " + json.dumps(event))
    if any(event.get("event") == "gateway_start" and event.get("outcome") != "error"
           for event in events) or any(event.get("event") == "gateway_shutdown" for event in events):
        raise RuntimeError("failed startup also emitted successful lifecycle events")
    if "docs/runtime-logging.md" not in result.stdout:
        raise RuntimeError("startup did not provide safe human recovery guidance")
    return {"stage": stage, "error_code": code, "exit_code": result.returncode,
            "one_terminal_failure": True, "stderr_json": True, "privacy": True}, result.stderr


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--evidence", type=Path)
    args = parser.parse_args()
    if sys.platform != "darwin":
        raise SystemExit("This package gate requires the macOS candidate binary.")
    binary = args.binary.resolve()
    if not binary.is_file() or not os.access(binary, os.X_OK):
        raise SystemExit("--binary must be an executable candidate")
    version = subprocess.check_output([str(binary), "version"], text=True).strip()
    checks = {}
    failure_logs = []
    process = None
    with tempfile.TemporaryDirectory(prefix="tidemux-startup-logs.") as temporary:
        root = Path(temporary)
        fake_bin = root / "bin"
        fake_bin.mkdir()
        security = fake_bin / "security"
        security.write_text(
            "#!/bin/sh\n"
            'case "${TIDEMUX_TEST_SECURITY_MODE:-ok}" in\n'
            f"  failed) printf '{PRIVATE_KEY}\\n' >&2; exit 1;;\n"
            f"  empty) printf ' \\n'; exit 0;;\n"
            f"  same) printf '{PRIVATE_KEY}\\n'; exit 0;;\n"
            "esac\n"
            f'case "$*" in *test.gateway*) printf \'{GATEWAY_KEY}\\n\';; '
            f"*test.provider*) printf '{PROVIDER_KEY}\\n';; *) exit 1;; esac\n",
            encoding="utf-8")
        security.chmod(0o755)
        home = root / PRIVATE_PATH
        home.mkdir()
        env = {"PATH": str(fake_bin) + ":/usr/bin:/bin", "HOME": str(home),
               "TMPDIR": str(root), "LANG": "C.UTF-8"}
        config = {
            "listen_addr": "127.0.0.1:0", "protocol": "openai",
            "base_url": "http://127.0.0.1:1/v1", "model": "synthetic-model",
            "upstream_id": "synthetic-provider", "max_in_flight": 1,
            "ledger_path": str(home / "ledger.db"),
            "access_token_keychain": {"service": "test.gateway", "account": PRIVATE_KEY},
            "upstream_keychain": {"service": "test.provider", "account": PRIVATE_KEY},
        }

        def run_failure(name, options, stage, code, mode="ok"):
            metadata, logs = check_failure(binary, options, dict(env, TIDEMUX_TEST_SECURITY_MODE=mode),
                                           root, stage, code, version)
            checks[name] = metadata
            failure_logs.append(logs)

        for name, options in (
                ("unknown_flag", ["--" + PRIVATE_ARGUMENT]),
                ("unknown_flag_newline", ["--" + PRIVATE_ARGUMENT + "\ninjected"]),
                ("missing_flag_value", ["--config"]),
                ("empty_config_path", ["--config="]),
                ("positional_argument", [PRIVATE_ARGUMENT]),
                ("extra_argument", ["--config", str(home / "missing"), PRIVATE_ARGUMENT])):
            run_failure(name, options, "arguments", "invalid_arguments")
        run_failure("default_missing_config", [], "config", "config_load_failed")
        run_failure("missing_config", ["--config", str(home / "missing")], "config", "config_load_failed")
        run_failure("config_is_directory", ["--config", str(home)], "config", "config_load_failed")
        path = home / "config.json"
        for name, data in (
                ("malformed_config", '{"' + PRIVATE_CONFIG + '":'),
                ("unsupported_field", json.dumps(dict(config, **{PRIVATE_CONFIG: PRIVATE_CONFIG}))),
                ("invalid_config", json.dumps(dict(config, listen_addr=PRIVATE_CONFIG)))):
            path.write_text(data, encoding="utf-8")
            run_failure(name, ["--config", str(path)], "config", "config_load_failed")
        path.write_text(json.dumps(config), encoding="utf-8")
        for mode in ("failed", "empty", "same"):
            run_failure("credentials_" + mode, ["--config", str(path)],
                        "credentials", "credential_resolution_failed", mode)
        no_provider = {key: value for key, value in config.items() if key not in
                       ("protocol", "base_url", "model", "upstream_id", "upstream_keychain")}
        path.write_text(json.dumps(no_provider), encoding="utf-8")
        run_failure("no_provider", ["--config", str(path)], "providers", "provider_initialization_failed")
        path.write_text(json.dumps(dict(config, ledger_path=str(home / "missing-parent" / "ledger.db"))), encoding="utf-8")
        run_failure("ledger_missing_parent", ["--config", str(path)], "ledger", "ledger_initialization_failed")
        corrupted = home / "corrupted-ledger.db"
        corrupted.write_text(PRIVATE_CONFIG, encoding="utf-8")
        path.write_text(json.dumps(dict(config, ledger_path=str(corrupted))), encoding="utf-8")
        run_failure("ledger_corrupted", ["--config", str(path)], "ledger", "ledger_initialization_failed")
        with socket.socket() as occupied:
            occupied.bind(("127.0.0.1", 0))
            occupied.listen()
            address = f"127.0.0.1:{occupied.getsockname()[1]}"
            path.write_text(json.dumps(dict(config, listen_addr=address)), encoding="utf-8")
            run_failure("listener_occupied", ["--config", str(path)], "listener", "listener_bind_failed")
        for flag in ("-h", "--help"):
            result = subprocess.run([str(binary), "serve", "--config", str(home / "missing"), flag],
                                    env=env, cwd=root, capture_output=True, text=True, timeout=10)
            check_events(result.stdout, result.stderr, root, version)
            if result.returncode != 0 or result.stderr or "usage: tidemux serve" not in result.stdout:
                raise RuntimeError("serve help did not succeed solely on stdout")
        checks["help"] = {"exit_code": 0, "stdout_only": True, "no_config_or_keychain_read": True}
        # A successful gateway must retain the normal lifecycle, and a second
        # owner must fail at ledger rather than producing a plain-text fallback.
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            port = reservation.getsockname()[1]
        path.write_text(json.dumps(dict(config, listen_addr=f"127.0.0.1:{port}")), encoding="utf-8")
        try:
            process = subprocess.Popen([str(binary), "serve", "--config", str(path)], env=env, cwd=root,
                                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            for _ in range(100):
                if process.poll() is not None:
                    raise RuntimeError("success fixture exited before listening")
                try:
                    with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                        break
                except OSError:
                    time.sleep(0.05)
            else:
                raise RuntimeError("success fixture did not become ready")
            run_failure("ledger_already_owned", ["--config", str(path)], "ledger", "ledger_initialization_failed")
            process.send_signal(signal.SIGTERM)
            stdout, stderr = process.communicate(timeout=15)
            events = check_events(stdout, stderr, root, version)
            if (process.returncode != 0 or "TideMux listening on http://127.0.0.1:" not in stdout
                    or sum(event.get("event") == "gateway_start" for event in events) != 1
                    or any(event.get("outcome") == "error" for event in events)
                    or sum(event.get("event") == "gateway_shutdown" and event.get("outcome") == "success"
                           for event in events) != 1):
                raise RuntimeError("normal start/shutdown lifecycle regressed")
            checks["normal_lifecycle"] = {"exit_code": 0, "stderr_json": True, "privacy": True}
            # Clean stop must release ownership for another process.
            with socket.socket() as occupied:
                occupied.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                occupied.bind(("127.0.0.1", port))
                occupied.listen()
                run_failure("ownership_released", ["--config", str(path)], "listener", "listener_bind_failed")
        finally:
            if process and process.poll() is None:
                process.kill()
                process.communicate(timeout=5)
        doctor = subprocess.run([str(binary), "doctor", "--config", str(home / "missing")],
                                env=env, cwd=root, capture_output=True, text=True, timeout=10)
        if doctor.returncode != 1 or doctor.stdout or doctor.stderr != "tidemux: cannot read config\n":
            raise RuntimeError("doctor's existing human-facing failure behavior changed")
        checks["other_cli_preserved"] = {"exit_code": 1, "human_stderr_preserved": True}
    result = {"binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
              "checks": checks, "passed": True, "production_access": False}
    failure_instances = [json.loads(logs.splitlines()[0])["service"]["instance"]["id"] for logs in failure_logs]
    if len(set(failure_instances)) != len(failure_instances):
        raise RuntimeError("process restart reused runtime instance identity")
    checks["event_identity"] = {"binary_version_matches": True, "restart_namespaces_distinct": True}
    if args.evidence:
        args.evidence.mkdir(parents=True, exist_ok=True)
        (args.evidence / "package-result.json").write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
        (args.evidence / "startup-runtime.jsonl").write_text("".join(failure_logs), encoding="utf-8")
    print(json.dumps(result))


if __name__ == "__main__":
    main()
