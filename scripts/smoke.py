#!/usr/bin/env python3
"""Offline smoke for one clean candidate: package, install, CLI, API and audit."""
import argparse
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import select
import signal
import sqlite3
import subprocess
import sys
import tarfile
import tempfile
import threading
from urllib.error import HTTPError
from urllib.parse import unquote, urlsplit
from urllib.request import Request, urlopen

ROOT = Path(__file__).resolve().parents[1]
GATEWAY_KEY = "smoke-gateway-secret"
PROVIDER_KEY = "smoke-provider-secret"
PROMPT = "smoke-private-prompt"
CONTENT = "smoke-private-content"
SESSION = "smoke-private-session"


def check(condition, message):
    if not condition:
        raise RuntimeError(message)


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def run(command, env, success=True):
    result = subprocess.run(command, env=env, capture_output=True, text=True, timeout=15)
    check((result.returncode == 0) == success, "Unexpected CLI result: " + " ".join(map(str, command[:3])))
    return result.stdout


def unpack(assets, destination):
    archives = list(assets.glob("*.tar.gz"))
    check(len(archives) == 1, "Expected one candidate archive")
    archive = archives[0]
    manifest = {}
    for line in (assets / "SHA256SUMS").read_text().splitlines():
        sha, name = line.split()
        check(Path(name).name == name and name not in manifest, "Invalid checksum filename")
        check(digest(assets / name) == sha, "Candidate checksum mismatch")
        manifest[name] = sha
    check(set(manifest) == {archive.name, "BUILD.txt", "sbom.spdx.json", "tidemux.rb"}, "Incomplete asset manifest")
    with tarfile.open(archive) as bundle:
        for member in bundle.getmembers():
            path = Path(member.name)
            check(not path.is_absolute() and ".." not in path.parts, "Unsafe archive path")
            check(not set(path.parts) & {"scripts", "examples", "fixtures"} and not member.name.endswith("_test.go"), "Test material in package")
            target = destination / path
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                check(member.isfile(), "Unexpected archive member type")
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(bundle.extractfile(member).read())
                target.chmod(member.mode & 0o777)
    binary, = destination.glob("tidemux-*/tidemux")
    package = binary.parent
    build = (assets / "BUILD.txt").read_text()
    check(build == (package / "BUILD.txt").read_text(), "Packaged BUILD mismatch")
    commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    check(f"commit: {commit}\n" in build and "vcs.modified=false" in build, "Package must come from current clean commit")
    sbom = json.loads((assets / "sbom.spdx.json").read_text())
    check(sbom == json.loads((package / "sbom.spdx.json").read_text()), "Packaged SBOM mismatch")
    own = next(p for p in sbom["packages"] if p["SPDXID"] == "SPDXRef-TideMux")
    check(own["sourceInfo"] == "git commit " + commit and own["checksums"][0]["checksumValue"] == digest(binary), "SBOM source/binary mismatch")
    formula = (assets / "tidemux.rb").read_text()
    check(archive.name in formula and digest(archive) in formula, "Formula archive mismatch")
    selected = set(re.findall(r'"([^"\n]+)"', formula.split("pkgshare.install", 1)[1].split("\n  end", 1)[0]))
    check({p.name for p in package.glob("*.md")} <= selected, "Formula omits package manuals")
    subprocess.run(["ruby", "-c", str(assets / "tidemux.rb")], check=True, capture_output=True)
    for document in package.rglob("*.md"):
        for link in re.findall(r'\]\(([^\s)]+)\)', document.read_text()):
            target = urlsplit(link.strip("<>"))
            if not target.scheme and target.path:
                check((document.parent / unquote(target.path)).exists(), "Broken package Markdown link")
    return archive, binary, own["versionInfo"]


class Upstream(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def reply(self, status, body, content_type="application/json", **headers):
        raw = (json.dumps(body) if isinstance(body, dict) else body).encode()
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(raw)))
        for key, value in headers.items():
            self.send_header(key, value)
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        self.reply(200, {"object": "list", "data": [{"id": "model", "object": "model"}]})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        anthropic = self.path.startswith("/anthropic/")
        authorized = self.headers.get("x-api-key") == PROVIDER_KEY if anthropic else self.headers.get("Authorization") == "Bearer " + PROVIDER_KEY
        with self.server.lock:
            self.server.posts.append((body, authorized))
        if body["messages"][0]["content"] == "rate-limit":
            self.reply(429, {"error": {"type": "rate_limit_error", "message": "Busy"}}, **{"Retry-After": "60"})
        elif body.get("stream"):
            if anthropic:
                frames = [
                    ("message_start", {"type": "message_start", "message": {"id": "smoke", "type": "message", "role": "assistant", "model": "model", "content": [], "usage": {"input_tokens": 3, "output_tokens": 0}}}),
                    ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
                    ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": CONTENT}}),
                    ("content_block_stop", {"type": "content_block_stop", "index": 0}),
                    ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn"}, "usage": {"output_tokens": 2}}),
                    ("message_stop", {"type": "message_stop"})]
                wire = "".join("event: " + event + "\ndata: " + json.dumps(data) + "\n\n" for event, data in frames)
            else:
                frames = [
                    {"id": "smoke", "model": "model", "choices": [{"index": 0, "delta": {"content": CONTENT}, "finish_reason": None}]},
                    {"id": "smoke", "model": "model", "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 3, "completion_tokens": 2}}]
                wire = "".join("data: " + json.dumps(frame) + "\n\n" for frame in frames) + "data: [DONE]\n\n"
            self.reply(200, wire, "text/event-stream")
        elif anthropic:
            self.reply(200, {"id": "smoke", "type": "message", "role": "assistant", "model": "model", "content": [{"type": "text", "text": CONTENT}], "stop_reason": "end_turn", "usage": {"input_tokens": 3, "output_tokens": 2}})
        else:
            self.reply(200, {"id": "smoke", "object": "chat.completion", "model": "model", "choices": [{"index": 0, "message": {"role": "assistant", "content": CONTENT}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 3, "completion_tokens": 2}})


def request(url, body=None, token=GATEWAY_KEY):
    headers = {"Authorization": "Bearer " + token, "Content-Type": "application/json", "X-TideMux-Session-ID": SESSION}
    req = Request(url, data=None if body is None else json.dumps(body).encode(), headers=headers)
    try:
        response = urlopen(req, timeout=10)
    except HTTPError as error:
        response = error
    with response:
        return response.status, response.read().decode(), response.headers.get("X-Tidemux-Request-Id")


def smoke(assets, root):
    archive, extracted, version = unpack(assets, root / "extracted")
    home, fake, destination = root / "home", root / "bin", root / "install with spaces"
    home.mkdir()
    fake.mkdir()
    data = home / "Library/Application Support/TideMux"
    data.mkdir(parents=True)
    marker = data / "existing-data"
    marker.write_text("preserve existing user data")
    security_marker = root / "security-calls"
    security = fake / "security"
    security.write_text('#!/bin/sh\nprintf x >> "$SMOKE_SECURITY_CALLS"\ncase "$*" in *smoke.gateway*) echo smoke-gateway-secret;; *smoke.provider*) echo smoke-provider-secret;; *) exit 1;; esac\n')
    security.chmod(0o700)
    curl = fake / "curl"
    curl.write_text('''#!/bin/sh
while [ "$#" -gt 0 ]; do
  case "$1" in -o) shift; dest=$1 ;; https://*) url=$1 ;; esac
  shift
done
case "$url" in "https://github.com/hs3180/tidemux/releases/download/v$TIDEMUX_VERSION/"*) ;; *) exit 2;; esac
cp "$SMOKE_ASSETS/${url##*/}" "$dest"
''')
    curl.chmod(0o700)
    env = {"HOME": str(home), "PATH": str(fake) + ":/usr/bin:/bin", "TMPDIR": str(root), "LANG": "C.UTF-8",
           "TIDEMUX_VERSION": version, "TIDEMUX_INSTALL_DIR": str(destination), "SMOKE_ASSETS": str(assets), "SMOKE_SECURITY_CALLS": str(security_marker)}
    binary = destination / "tidemux"
    for _ in range(2):
        run(["sh", str(ROOT / "scripts/install.sh")], env)
        check(digest(binary) == digest(extracted), "Installed binary differs from candidate")
        check(run([str(binary), "version"], env).strip() == version, "Installed version mismatch")
        check(marker.read_text() == "preserve existing user data" and not security_marker.exists(), "Installer changed data or accessed Keychain")
    binary.unlink()
    check(marker.exists(), "Uninstall removed user data")
    run(["sh", str(ROOT / "scripts/install.sh")], env)
    server = ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
    server.daemon_threads = True
    server.posts, server.lock = [], threading.Lock()
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    config = data / "config.json"
    ledger = data / "ledger.db"
    config.write_text(json.dumps({"listen_addr": "127.0.0.1:0", "ledger_path": str(ledger),
        "access_token_keychain": {"service": "smoke.gateway", "account": "fixture"},
        "providers": {p: {"protocol": p, "base_url": f"http://127.0.0.1:{server.server_port}/{p}/v1", "supported_models": ["model"],
            "upstream_keychain": {"service": "smoke.provider", "account": "fixture"}} for p in ("openai", "anthropic")}}))
    config.chmod(0o600)
    original = config.read_bytes()
    run([str(binary), "doctor", "--config", str(config)], env)
    run([str(binary), "provider", "list", "--config", str(config)], env)
    run([str(binary), "provider", "show", "openai", "--config", str(config)], env)
    process = None
    try:
        with (root / "runtime.jsonl").open("w") as stderr:
            process = subprocess.Popen([str(binary), "serve", "--config", str(config)], env=env, stdout=subprocess.PIPE, stderr=stderr, text=True)
            check(select.select([process.stdout], [], [], 10)[0], "Gateway readiness timed out")
            line = process.stdout.readline().strip()
            check(line.startswith("TideMux listening on http://127.0.0.1:"), "Unexpected gateway startup")
            url = line.removeprefix("TideMux listening on ")
            check_config = root / "check-config.json"
            snapshot = json.loads(original)
            snapshot["listen_addr"] = url.removeprefix("http://")
            check_config.write_text(json.dumps(snapshot))
            check_config.chmod(0o600)
            run([str(binary), "gateway", "check", "--config", str(check_config)], env)
            status, models, _ = request(url + "/v1/models")
            check(status == 200 and "openai/model" in models and "anthropic/model" in models, "Discovery failed")
            check(request(url + "/v1/models", token="incorrect")[0] == 401, "Authentication failed")
            ids = []
            cases = [(protocol, provider, streaming) for protocol in ("openai", "anthropic")
                     for provider in ("openai", "anthropic") for streaming in (False, True)]
            for protocol, provider, streaming in cases:
                path = "/v1/messages" if protocol == "anthropic" else "/v1/chat/completions"
                body = {"model": provider + "/model", "max_tokens": 16, "messages": [{"role": "user", "content": PROMPT}], "stream": streaming}
                status, response, request_id = request(url + path, body)
                check(status == 200 and CONTENT in response and request_id, "Generation failed")
                check(not streaming or ("message_stop" if protocol == "anthropic" else "[DONE]") in response, "Missing stream terminal")
                ids.append(request_id)
            check(len(server.posts) == len(cases) and all(authorized for _, authorized in server.posts), "Incorrect upstream dispatch/authentication")
            body = {"model": "openai/model", "messages": [{"role": "user", "content": "rate-limit"}]}
            status, _, request_id = request(url + "/v1/chat/completions", body)
            check(status == 429 and request_id and len(server.posts) == len(cases) + 1, "Upstream failure was lost or replayed")
            ids.append(request_id)
            process.send_signal(signal.SIGTERM)
            check(process.wait(timeout=15) == 0, "Gateway shutdown failed")
        with sqlite3.connect(ledger) as db:
            rows = [json.loads(row[0]) for row in db.execute("SELECT record_json FROM request_audit")]
        check(len(rows) == len(ids) and {r["id"] for r in rows} == set(ids), "HTTP/audit identities or row counts differ")
        check(sum(r["status"] == "ok" for r in rows) == len(cases), "Incorrect terminal audit results")
        check(all(r["input_tokens"] == 3 and r["output_tokens"] == 2 and r["estimated_cost"] is None for r in rows if r["status"] == "ok"), "Usage or unknown-cost semantics changed")
        records = [json.loads(line) for line in (root / "runtime.jsonl").read_text().splitlines()]
        terminals = [r for r in records if r.get("event") == "request_terminal"]
        check(len(terminals) == len(ids) and {r["requestId"] for r in terminals} == set(ids), "Terminal diagnostics do not join audit")
        usage = [json.loads(line) for p in data.glob("logs/projects/tidemux/*.jsonl") for line in p.read_text().splitlines()]
        counted = [r for r in usage if r.get("message", {}).get("usage")]
        check(len(counted) == len(cases), "Missing or duplicated usage records")
        logs = (root / "runtime.jsonl").read_text() + json.dumps(usage)
        check(all(value not in logs for value in (GATEWAY_KEY, PROVIDER_KEY, PROMPT, CONTENT, SESSION, str(home))), "Private data leaked into diagnostics")
        check(config.read_bytes() == original and marker.read_text() == "preserve existing user data", "Read-only CLI or runtime changed existing configuration")
        run([str(binary), "billing", "--details", "--json"], env)
    finally:
        if process is not None:
            if process.poll() is None:
                process.kill()
            process.wait(timeout=15)
            process.stdout.close()
        server.shutdown()
        server.server_close()
        worker.join(timeout=5)
    run([sys.executable, str(ROOT / "scripts/smoke_provider_setup.py")], dict(env, TIDEMUX_TEST_BINARY=str(binary)))
    return {"passed": True, "source": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
            "archive_sha256": digest(archive), "binary_sha256": digest(extracted), "version": version,
            "generation_posts": len(ids), "external_clients_or_services": False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", required=True, type=Path, help="Immutable release.py output directory")
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="tidemux-smoke.") as temporary:
        print(json.dumps(smoke(args.directory.resolve(), Path(temporary))))


if __name__ == "__main__":
    main()
