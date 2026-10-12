#!/usr/bin/env python3
"""Check shared launch, real gateway dispatch, affinity and diagnostics.

Default mode uses a disposable child to inspect launcher handoff and send real
HTTP requests to the packaged gateway. --installed-clients runs all three real
clients' read/edit/test and continuation workflows under both strategies. The
fixture supplies a stable session header; provider responses and credentials
are scripted loopback fixtures, with isolated client profiles.
"""
import argparse
from copy import deepcopy
import hashlib
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import threading
from urllib.request import Request

from test_availability_package import exchange
from test_client_reliability_package import GATEWAY_KEY, PROVIDER_KEY, SESSION_MARKER, running_gateway
from test_client_recovery_package import MODEL, READ_MARKER, DONE, CONTINUED, Observer, Upstream, execute, environment
from test_context_management_claude import Server

PRIVATE_ERROR = "shared-launch-private-upstream-error"


def configure(config, strategy):
    base = config["providers"]["anthropic"]
    config["providers"] = {}
    for ref, cost, context in (("a", 1, 128000), ("b", 2, 65536)):
        config["providers"][ref] = dict(deepcopy(base), upstream_id=ref,
            base_url=base["base_url"].replace("/v1", "/" + ref + "/v1"), supported_models=[MODEL],
            model_capabilities={"context_tokens": context, "max_output_tokens": 8192 * cost},
            error_code_mappings=[{"upstream_code": "model_missing", "http_status": 404, "category": "model_not_found"}],
            prices={MODEL: {"currency": "USD", "source": "fixture", "version": "1",
                           "input_cache_hit_per_million": cost, "input_cache_miss_per_million": cost,
                           "output_per_million": cost}})
    config["routing"] = {"shared_model_strategy": strategy}


class Provider(Upstream):
    def do_POST(self):
        if self.server.scene == "fail_a" and self.path.startswith("/a/"):
            body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
            with self.server.lock:
                self.server.posts += 1
                self.server.requests.append(body)
            self.send_response(404)
            payload = json.dumps({"error": {"code": "model_missing", "message": PRIVATE_ERROR}}).encode()
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
        else:
            super().do_POST()


class StableSessionObserver(Observer):
    def forward(self):
        if self.command == "POST":
            if self.headers.get("X-TideMux-Session-ID"):
                self.headers.replace_header("X-TideMux-Session-ID", SESSION_MARKER)
            else:
                self.headers.add_header("X-TideMux-Session-ID", SESSION_MARKER)
        super().forward()


CHILD = '''#!/usr/bin/env python3
import json,os,sys
from pathlib import Path
from urllib.request import Request,urlopen
client,session=sys.argv[-2:]
if client=='claude':
 model=os.environ['ANTHROPIC_MODEL']; url=os.environ['ANTHROPIC_BASE_URL']; path='/v1/messages'
elif client=='kilo':
 config=json.loads(os.environ['KILO_CONFIG_CONTENT']); model=config['model'].removeprefix('tidemux-local/')
 provider=config['provider']['tidemux-local']; url=provider['options']['baseURL'].removesuffix('/v1'); path='/v1/chat/completions'
 if '/' not in model: assert 'limit' not in provider['models'][model]
else:
 config=json.loads((Path(os.environ['HERMES_HOME'])/'config.yaml').read_text()); model=config['model']['default']
 url=config['providers']['tidemux-local']['api'].removesuffix('/v1'); path='/v1/chat/completions'
 if '/' not in model: assert 'context_length' not in config['model']
assert 'local-private-provider-key' not in json.dumps(dict(os.environ))
for _ in range(2):
 body={'model':model,'max_tokens':16,'messages':[{'role':'user','content':'shared-launch-private-body'}]}
 headers={'Authorization':'Bearer '+os.environ['TIDEMUX_GATEWAY_TOKEN'],'Content-Type':'application/json','X-TideMux-Session-ID':session}
 with urlopen(Request(url+path,json.dumps(body).encode(),headers),timeout=10) as response:
  assert response.status==200
  assert 'PROBE_OK' in response.read().decode()
print(json.dumps({'model':model,'requests':2,'candidate_limits_omitted':'/' not in model}))
'''


def diagnostics(binary, gateway):
    result = subprocess.run([str(binary), "gateway", "availability", "--json", "--config", str(gateway["config_file"])],
                            env=gateway["env"], capture_output=True, text=True, timeout=10, check=True)
    for private in (GATEWAY_KEY, PROVIDER_KEY, SESSION_MARKER, PRIVATE_ERROR, str(gateway["root"])):
        if private in result.stdout:
            raise RuntimeError("Private data appeared in shared launch diagnostics")
    report = json.loads(result.stdout)
    attempts = sum(key["counters"]["http_attempts"] for provider in report["providers"] for key in provider["key_pool"]["keys"])
    assert attempts == gateway["upstream"].posts, (attempts, gateway["upstream"].posts)
    with sqlite3.connect(gateway["ledger"]) as db:
        persisted = "\n".join(db.iterdump())
    logs = (gateway["root"] / "stderr").read_text()
    for private in (GATEWAY_KEY, PROVIDER_KEY, SESSION_MARKER, PRIVATE_ERROR, "shared-launch-private-body"):
        assert private not in persisted + logs
    return report


def audits(gateway):
    with sqlite3.connect(gateway["ledger"]) as db:
        return [json.loads(row[0]) for row in db.execute("SELECT record_json FROM request_audit ORDER BY rowid")]


def check_handoff(binary, client, strategy):
    with running_gateway(binary, upstream_handler=Provider, configure=lambda c: configure(c, strategy)) as gateway:
        root, upstream = gateway["root"], gateway["upstream"]
        upstream.requests, upstream.failures, upstream.scene = [], [], "probe"
        child = root / "bin/client"
        child.write_text(CHILD.replace("local-private-provider-key", PROVIDER_KEY))
        child.chmod(0o700)
        original = gateway["config_file"].read_bytes()

        def launch(model=MODEL, path=None, session=SESSION_MARKER):
            return subprocess.run([str(binary), client, "--config", str(path or gateway["config_file"]),
                                   "--model", model, "--executable", str(child), "--", client, session],
                                  env=gateway["env"], capture_output=True, text=True, timeout=20)

        first = launch()
        assert first.returncode == 0, first.stderr
        assert json.loads(first.stdout)["model"] == MODEL
        rows = audits(gateway)
        assert len(rows) == 2 and rows[0]["provider_ref"] == rows[1]["provider_ref"]
        if strategy == "price_priority":
            assert rows[0]["provider_ref"] == "a"
        disabled = dict(deepcopy(gateway["config"]), routing={})
        off = root / "off.json"
        off.write_text(json.dumps(disabled))
        rejected = launch(path=off)
        assert rejected.returncode != 0 and "matches multiple provider scopes" in rejected.stderr
        assert upstream.posts == 2
        explicit = launch("a/" + MODEL, session=SESSION_MARKER + "-explicit")
        assert explicit.returncode == 0, explicit.stderr
        assert all(row["provider_ref"] == "a" for row in audits(gateway)[2:])
        upstream.scene = "fail_a"
        request = Request(gateway["url"] + "/v1/messages", json.dumps({"model": "a/" + MODEL,
            "max_tokens": 16, "messages": [{"role": "user", "content": "shared-launch-private-body"}]}).encode(),
            {"Authorization": "Bearer " + GATEWAY_KEY, "Content-Type": "application/json"})
        assert exchange(request)[0] == 404
        resumed = launch()
        assert resumed.returncode == 0, resumed.stderr
        rows = audits(gateway)
        assert len(rows) == upstream.posts == 7
        assert all(row["provider_ref"] == "b" for row in rows[-2:])
        report = diagnostics(binary, gateway)
        a = next(provider for provider in report["providers"] if provider["ref"] == "a")
        model = next(item for item in a["models"] if item["model"] == MODEL)
        assert model["state"] == "cooling" and model["reason"] == "model_not_found"
        assert a["key_pool"]["eligible"] == 1
        assert gateway["config_file"].read_bytes() == original
        assert all(request["model"] == MODEL for request in upstream.requests)
        return dict(client=client, strategy=strategy, passed=True, shared_requests_and_affinity=True,
                    default_ambiguity_rejected_without_dispatch=True, explicit_scope=True,
                    cooled_model_skipped=True, actual_key_attempts=7, client_mode="disposable_http_child")


def check_installed(binary, client, strategy, evidence):
    executable = shutil.which(client)
    if not executable:
        raise RuntimeError("Required installed client not found: " + client)
    with running_gateway(binary, upstream_handler=Provider, configure=lambda c: configure(c, strategy)) as gateway:
        root, upstream = gateway["root"], gateway["upstream"]
        upstream.requests, upstream.failures, upstream.scene, upstream.workflow_step = [], [], "workflow", 0
        work = root / "workspace"
        work.mkdir()
        gateway["work"] = upstream.work = work.resolve()
        (work / "fixture.txt").write_text(READ_MARKER + "\n")
        (work / "calc.py").write_text("def add(a, b):\n    return a - b\n")
        test = "import unittest\nfrom calc import add\nclass CalcTest(unittest.TestCase):\n    def test_add(self):\n        self.assertEqual(add(2, 3), 5)\n"
        (work / "test_calc.py").write_text(test)
        observer = Server(("127.0.0.1", 0), StableSessionObserver)
        observer.gateway_port = int(gateway["url"].rsplit(":", 1)[1])
        observer.requests = []
        threading.Thread(target=observer.serve_forever, daemon=True).start()
        config = deepcopy(gateway["config"])
        config["listen_addr"] = f"127.0.0.1:{observer.server_port}"
        path = root / "launch.json"
        path.write_text(json.dumps(config))
        original = path.read_bytes()
        try:
            version = subprocess.check_output([executable, "--version"], env=environment(gateway), text=True).strip().splitlines()[0]
            workflow = execute(binary, client, Path(executable), path, gateway,
                "Read fixture.txt. Fix add in calc.py without changing test_calc.py. Run python3 -m unittest -v. Work only in this fixture folder.", model=MODEL)
            assert workflow["exit_code"] == 0 and not workflow["timed_out"] and DONE in workflow["stdout"], workflow
            assert (work / "calc.py").read_text() == "def add(a, b):\n    return a + b\n"
            assert (work / "test_calc.py").read_text() == test and not upstream.failures
            upstream.scene = "continue"
            continuation = execute(binary, client, Path(executable), path, gateway,
                "Continue the prior fixture conversation.", continuing=True, model=MODEL)
            assert continuation["exit_code"] == 0 and not continuation["timed_out"] and CONTINUED in continuation["stdout"], continuation
            assert not upstream.failures and path.read_bytes() == original
            assert all(item["body"]["model"] == MODEL and item["status"] == 200 for item in observer.requests)
            rows = audits(gateway)
            providers = {row["provider_ref"] for row in rows}
            assert len(rows) == upstream.posts and len(providers) == 1
            if strategy == "price_priority":
                assert providers == {"a"}
            diagnostics(binary, gateway)
            result = dict(client=client, version=version, strategy=strategy, passed=True,
                real_client_read_edit_test=True, persistent_continuation=True, bare_model_preserved=True,
                stable_session_header_from_fixture=True, provider_binding_preserved=True,
                actual_key_attempts=upstream.posts, client_mode="installed")
            if evidence:
                (evidence / (client + "-" + strategy + ".json")).write_text(json.dumps(result, indent=2) + "\n")
            return result
        finally:
            observer.shutdown()
            observer.server_close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--installed-clients", action="store_true")
    parser.add_argument("--evidence", type=Path)
    args = parser.parse_args()
    binary = args.binary.resolve()
    if args.evidence:
        args.evidence.mkdir(parents=True, exist_ok=True, mode=0o700)
    results = []
    check = check_installed if args.installed_clients else check_handoff
    for strategy in ("random", "price_priority"):
        for client in ("claude", "kilo", "hermes"):
            result = check(binary, client, strategy, args.evidence) if args.installed_clients else check(binary, client, strategy)
            results.append(result)
            print(json.dumps(result), flush=True)
    result = dict(passed=True, binary_sha256=hashlib.sha256(binary.read_bytes()).hexdigest(),
                  installed_clients=args.installed_clients, scenarios=results, privacy=True)
    if args.evidence:
        (args.evidence / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result))


if __name__ == "__main__":
    main()
