#!/usr/bin/env python3
"""Exercise guided provider setup and Keychain unlock behavior through a PTY."""
import http.server
import json
import os
import pathlib
import pty
import select
import signal
import subprocess
import tempfile
import threading
import time

ROOT = pathlib.Path(__file__).resolve().parents[1]


class ModelsHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({"object": "list", "data": [{"id": "deepseek-flash", "object": "model"}]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args):
        pass


server = http.server.HTTPServer(("127.0.0.1", 0), ModelsHandler)
threading.Thread(target=server.serve_forever, daemon=True).start()
endpoint = f"http://127.0.0.1:{server.server_port}/v1"

with tempfile.TemporaryDirectory(prefix="tidemux-provider-pty-") as temp:
    root = pathlib.Path(temp)
    binary = os.environ.get("TIDEMUX_TEST_BINARY", str(root / "tidemux"))
    if "TIDEMUX_TEST_BINARY" not in os.environ:
        subprocess.run(["go", "build", "-o", binary, "./cmd/tidemux"], cwd=ROOT, check=True)
    fake = root / "bin"
    fake.mkdir()
    (fake / "security").write_text(r'''#!/usr/bin/env python3
import sys,os,json,shlex,pathlib,getpass
p=pathlib.Path(os.environ['TIDEMUX_TEST_KEYCHAIN_DB']);marker=p.with_suffix('.unlocked')
mode=os.environ['TIDEMUX_TEST_UNLOCK'];d=json.loads(p.read_text()) if p.exists() else {};a=sys.argv[1:]
if a==['-i']:
 a=shlex.split(sys.stdin.read());key=a[a.index('-s')+1]+'/'+a[a.index('-a')+1]
 if key in d:sys.exit(1)
 d[key]=bytes.fromhex(a[a.index('-X')+1]).decode();p.write_text(json.dumps(d))
elif a==['show-keychain-info']:
 sys.exit(0 if mode=='unlocked' or marker.exists() else 1)
elif a==['unlock-keychain']:
 if mode=='unlocked':sys.exit(5)
 try:password=getpass.getpass('System unlock password: ')
 except (KeyboardInterrupt,EOFError):sys.exit(1)
 if mode=='failure' or password!='synthetic-login-password':sys.exit(1)
 marker.touch()
elif a[0]=='find-generic-password':
 key=a[a.index('-s')+1]+'/'+a[a.index('-a')+1]
 if key not in d:sys.exit(1)
 print(d[key])
elif a[0]=='delete-generic-password':
 key=a[a.index('-s')+1]+'/'+a[a.index('-a')+1];d.pop(key,None);p.write_text(json.dumps(d))
else:sys.exit(2)
''')
    (fake / "security").chmod(0o700)
    (fake / "launchctl").write_text("#!/bin/sh\nexit 0\n")
    (fake / "launchctl").chmod(0o700)

    for mode in ["unlocked", "success", "failure", "cancel"]:
        db = root / (mode + "-secrets.json")
        user_directory = root / mode / "user"
        config = user_directory / "Library" / "Application Support" / "TideMux" / "config.json"
        env = dict(os.environ, HOME=str(user_directory), PATH=str(fake) + ":" + os.environ["PATH"],
                   TIDEMUX_TEST_KEYCHAIN_DB=str(db), TIDEMUX_TEST_UNLOCK=mode)
        secret = b"synthetic-pty-secret-not-real"
        password = b"synthetic-login-password"
        pid, fd = pty.fork()
        if pid == 0:
            os.execve(binary, [binary, "provider", "add"], env)

        captured = b""
        endpoint_sent = key_sent = schedule_sent = unlocked = False
        deadline = time.monotonic() + 20
        status = None
        try:
            while time.monotonic() < deadline:
                ready, _, _ = select.select([fd], [], [], 0.1)
                if ready:
                    try:
                        chunk = os.read(fd, 8192)
                    except OSError:
                        chunk = b""
                    captured += chunk
                    if not unlocked and b"System unlock password:" in captured:
                        time.sleep(0.05)
                        os.write(fd, b"\x03" if mode == "cancel" else password + b"\n")
                        unlocked = True
                    if not endpoint_sent and b"Provider API Base URL:" in captured:
                        time.sleep(0.05)
                        os.write(fd, endpoint.encode() + b"\n")
                        endpoint_sent = True
                    if not key_sent and b"Provider API key (hidden):" in captured:
                        time.sleep(0.05)
                        os.write(fd, secret + b"\n")
                        key_sent = True
                    if not schedule_sent and b"Daily report notification time" in captured:
                        time.sleep(0.05)
                        os.write(fd, b"08:30\n" if mode == "success" else b"\n")
                        schedule_sent = True
                done, result = os.waitpid(pid, os.WNOHANG)
                if done:
                    status = result
                    break
            if status is None:
                os.kill(pid, signal.SIGKILL)
                os.waitpid(pid, 0)
                raise RuntimeError("provider add timed out")
        finally:
            os.close(fd)

        assert secret not in captured and password not in captured, "credential echoed"
        if mode != "unlocked":
            assert b"Your login keychain is locked." in captured, "missing Keychain unlock guidance"
        if mode == "success":
            assert b"Keychain unlocked. Continuing setup." in captured, "missing unlock confirmation"
        if mode in ["failure", "cancel"]:
            assert status != 0 and not endpoint_sent and not config.exists() and not db.exists(), "failed unlock mutated configuration"
        else:
            assert status == 0, "provider add failed with synthetic Keychain"
            assert unlocked == (mode == "success"), "unnecessary or missing unlock prompt"
            assert endpoint_sent and key_sent and schedule_sent, "missing guided setup prompt"
            assert b"Select a model as local-provider/MODEL" in captured, "missing explicit provider/model guidance"
            contents = config.read_text()
            assert secret.decode() not in contents
            c = json.loads(contents)
            p = c["providers"]["local-provider"]
            assert "model" not in p and p["base_url"] == endpoint
            assert p["protocol"] == "openai" and "default_providers" not in c
            if mode == "success":
                assert c["report_schedule"] == {"time": "08:30", "channel": "macos"}
            else:
                assert c.get("report_schedule", {}) == {}
            assert config.stat().st_mode & 0o777 == 0o600
            values = json.loads(db.read_text())
            assert len(values) == 2 and secret.decode() in values.values()
            gateway_values = [value for value in values.values() if value != secret.decode()]
            assert len(gateway_values) == 1 and len(gateway_values[0]) == 64, "gateway key was not randomly generated"
        print("PTY provider add passed: " + mode)

server.shutdown()
