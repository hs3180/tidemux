#!/usr/bin/env python3
"""Exercise public 0.3.1 -> candidate -> 0.3.1 with retained config/ledger data.

Only temporary HOME, a Keychain double and loopback upstreams are used. This
checks package compatibility on one Mac; public installation is a separate gate.
"""
import argparse
import copy
from contextlib import closing
import hashlib
from http.server import BaseHTTPRequestHandler
import json
import os
from pathlib import Path
import re
import shutil
import signal
import sqlite3
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

from test_install_upgrade_rollback import find_archive, package_binary, verify_manifest
from test_runtime_logs_package import Server, free_port

BASELINE_SHA = '3542be1640dbbec8ecb39bbb373545b8f0ae7561fc5517228b9acf7db5f461e1'
BASELINE_SOURCE = '4886a58acd1ad5d12e1044b972e700b94082ada1'
GATEWAY_KEY = 'rollback-private-gateway-key'
PROVIDER_KEY = 'rollback-private-provider-key'
SESSION = 'rollback-private-session'
PROMPT = 'rollback-private-prompt'


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def check_build(archive, version, expected_source):
    with tarfile.open(archive) as bundle:
        data = bundle.extractfile(f'tidemux-{version}/BUILD.txt').read().decode()
    if f'commit: {expected_source}\n' not in data or f'vcs.revision={expected_source}' not in data or 'vcs.modified=false' not in data:
        raise RuntimeError('package BUILD does not identify the expected clean source')
    return data


def wait_for(predicate, reason):
    deadline = time.monotonic() + 12
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(.05)
    raise RuntimeError(reason)


def audits(path):
    with closing(sqlite3.connect(path)) as db:
        if db.execute('PRAGMA integrity_check').fetchone() != ('ok',):
            raise RuntimeError('ledger integrity check failed')
        return dict(db.execute('SELECT id,record_json FROM request_audit'))


def consistent_backup(source, destination):
    descriptor = os.open(destination, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    os.close(descriptor)
    # A WAL source may need its -shm recreated after graceful close. Open an
    # ordinary SQLite handle with query_only; perform no application writes or
    # TideMux startup/recovery. Explicit close avoids holding stale WAL handles.
    with closing(sqlite3.connect(source)) as original, closing(sqlite3.connect(destination)) as backup:
        original.execute('PRAGMA query_only=ON')
        original.backup(backup)
        if backup.execute('PRAGMA integrity_check').fetchone() != ('ok',):
            raise RuntimeError('SQLite backup integrity check failed')
    if audits(source) != audits(destination):
        raise RuntimeError('consistent backup lost committed audit history')


class Upstream(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def reply(self, value):
        data = json.dumps(value).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.reply({'object': 'list', 'data': [{'id': 'fixture'}]})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        with self.server.lock:
            self.server.calls += 1
        if self.headers.get('Authorization') != 'Bearer ' + PROVIDER_KEY:
            raise RuntimeError('Keychain reference did not retain its fixture credential')
        response = {'id': 'mock', 'object': 'chat.completion', 'model': body['model'], 'choices': [{'index': 0, 'message': {'role': 'assistant', 'content': 'ok'}, 'finish_reason': 'stop'}]}
        if '-local-estimate' not in json.dumps(body['messages']):
            response['usage'] = {'prompt_tokens': 3, 'completion_tokens': 2, 'prompt_tokens_details': {'cached_tokens': 0}}
        self.reply(response)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--baseline-dir', required=True, type=Path, help='public v0.3.1 archive and SHA256SUMS')
    candidate_input = parser.add_mutually_exclusive_group(required=True)
    candidate_input.add_argument('--candidate-dir', type=Path, help='clean combined candidate archive and SHA256SUMS')
    candidate_input.add_argument('--candidate-binary', type=Path, help='development preview only; never counts as package acceptance')
    parser.add_argument('--candidate-version', default='0.3.2')
    parser.add_argument('--candidate-source', required=True, help='full source commit expected in BUILD.txt')
    parser.add_argument('--evidence-dir', required=True, type=Path)
    args = parser.parse_args()
    evidence = args.evidence_dir.resolve()
    evidence.mkdir(parents=True, exist_ok=True)
    baseline = find_archive(args.baseline_dir.resolve(), '0.3.1')
    baseline_sha = verify_manifest(args.baseline_dir.resolve(), baseline)
    if baseline_sha != BASELINE_SHA:
        raise RuntimeError('baseline archive does not match the public v0.3.1 release')
    baseline_build = check_build(baseline, '0.3.1', BASELINE_SOURCE)
    candidate_sha = None
    candidate = None
    if args.candidate_dir:
        candidate = find_archive(args.candidate_dir.resolve(), args.candidate_version)
        candidate_sha = verify_manifest(args.candidate_dir.resolve(), candidate)
        candidate_build = check_build(candidate, args.candidate_version, args.candidate_source)
    else:
        candidate_build = subprocess.check_output(['go', 'version', '-m', str(args.candidate_binary.resolve())], text=True)
        if f'vcs.revision={args.candidate_source}' not in candidate_build:
            raise RuntimeError('development preview source does not match its build metadata')
    with tempfile.TemporaryDirectory(prefix='tidemux-upgrade-rollback.') as temporary:
        root = Path(temporary)
        home = root / 'home'
        config = home / 'Library/Application Support/TideMux/config.json'
        config.parent.mkdir(parents=True)
        fake = root / 'bin'; fake.mkdir()
        secrets = root / 'keychain-double.json'
        secrets.write_text(json.dumps({'gateway/local': GATEWAY_KEY, 'provider/local': PROVIDER_KEY}))
        secret_digest = digest(secrets)
        security = fake / 'security'
        security.write_text('''#!/usr/bin/env python3
import json,os,sys
a=sys.argv[1:]
if a==['show-keychain-info']:sys.exit(0)
if not a or a[0]!='find-generic-password':sys.exit(1)
k=a[a.index('-s')+1]+'/'+a[a.index('-a')+1]
d=json.load(open(os.environ['TIDEMUX_TEST_KEYCHAIN_DB']))
if k not in d:sys.exit(1)
print(d[k])
'''); security.chmod(0o700)
        env = dict(os.environ, HOME=str(home), PATH=str(fake) + ':' + os.environ.get('PATH', ''), TIDEMUX_TEST_KEYCHAIN_DB=str(secrets))
        old, new, installed = root / 'public-0.3.1', root / 'candidate', root / 'installed'
        package_binary(baseline, '0.3.1', old)
        rollback_helper = root / 'prepare_rollback.py'
        if candidate:
            package_binary(candidate, args.candidate_version, new)
            with tarfile.open(candidate) as bundle:
                rollback_helper.write_bytes(bundle.extractfile(f'tidemux-{args.candidate_version}/tools/prepare_rollback.py').read())
        else:
            shutil.copyfile(args.candidate_binary.resolve(), new); new.chmod(0o700)
            shutil.copyfile(Path(__file__).with_name('prepare_rollback.py'), rollback_helper)
        upstream = Server(('127.0.0.1', 0), Upstream)
        upstream.lock = threading.Lock(); upstream.calls = 0
        threading.Thread(target=upstream.serve_forever, daemon=True).start()
        port = free_port(); url = f'http://127.0.0.1:{port}'
        ledger = root / 'ledger.db'; output = root / 'usage'
        price = {'currency': 'USD', 'source': 'mock', 'version': 'old', 'input_cache_miss_per_million': 1, 'input_cache_hit_per_million': .1, 'output_per_million': 2}
        value = {'listen_addr': f'127.0.0.1:{port}', 'max_in_flight': 4, 'max_active_sessions': 4, 'ledger_path': str(ledger), 'access_token_keychain': {'service': 'gateway', 'account': 'local'}, 'routing': {'shared_model_strategy': 'random'}, 'auto_chain': [{'provider': 'mock', 'model': 'fixture'}], 'providers': {'mock': {'protocol': 'openai', 'base_url': f'http://127.0.0.1:{upstream.server_port}/v1', 'upstream_keychains': [{'service': 'provider', 'account': 'local'}], 'supported_models': ['fixture'], 'prices': {'fixture': price}, 'budget': {'currency': 'USD', 'five_hour_limit': 100, 'weekly_limit': 100, 'alert_threshold': .8, 'mode': 'hard'}}}}
        config.write_text(json.dumps(value)); config.chmod(0o600)
        process = None
        def cli(binary, arguments, stage_env=env):
            result = subprocess.run([str(binary), *arguments], env=stage_env, capture_output=True, text=True, timeout=20)
            if result.returncode:
                raise RuntimeError('isolated CLI failed: ' + result.stderr)
            return result.stdout
        def stop():
            nonlocal process
            if process:
                process.send_signal(signal.SIGTERM); process.wait(timeout=10)
                if process.returncode:
                    raise RuntimeError('gateway graceful stop failed')
                process = None
        def start(binary, version):
            nonlocal process
            shutil.copyfile(binary, installed); installed.chmod(0o700)
            if digest(installed) != digest(binary) or cli(installed, ['version']).strip() != version:
                raise RuntimeError('installed executable does not match its accepted package')
            process = subprocess.Popen([str(installed), 'serve', '--config', str(config)], env=env, stdout=(root / 'stdout').open('a'), stderr=(root / 'stderr').open('a'))
            def ready():
                if process.poll() is not None:
                    raise RuntimeError('isolated gateway failed startup')
                try:
                    with urlopen(Request(url + '/v1/models', headers={'Authorization': 'Bearer ' + GATEWAY_KEY}), timeout=1) as response:
                        return response.status == 200
                except (URLError, TimeoutError):
                    return False
            wait_for(ready, 'gateway did not start')
            cli(installed, ['doctor', '--config', str(config)])
        def request(protocol, session=SESSION, expected=200, estimated=False):
            body = {'model': 'mock/fixture', 'max_tokens': 32, 'messages': [{'role': 'user', 'content': PROMPT + ('-local-estimate' if estimated else '')}]}
            headers = {'Authorization': 'Bearer ' + GATEWAY_KEY, 'Content-Type': 'application/json'}
            if protocol == 'anthropic':
                body['metadata'] = {'user_id': json.dumps({'session_id': session})}
            else:
                headers['X-TideMux-Session-ID'] = session
            path = '/v1/messages' if protocol == 'anthropic' else '/v1/chat/completions'
            try:
                response = urlopen(Request(url + path, json.dumps(body).encode(), headers), timeout=10)
            except HTTPError as error:
                response = error
            with response:
                response.read()
                if response.status != expected:
                    raise RuntimeError('isolated request status differs from expected')
                return response.headers.get('X-TideMux-Request-ID')
        def report(binary, wanted, stage_env=env):
            result = json.loads(cli(binary, ['billing', '--from', '2000-01-01T00:00:00Z', '--details', '--json'], stage_env))
            rows = {row['id']: row for row in result['requests']}
            if rows.keys() != wanted.keys():
                raise RuntimeError('binary cannot read all retained old/new audit rows')
            for key, raw in wanted.items():
                record = json.loads(raw)
                for field in ('provider_ref', 'model', 'status', 'input_tokens', 'output_tokens', 'currency', 'estimated_cost', 'price_snapshot'):
                    if rows[key].get(field) != record.get(field):
                        raise RuntimeError('legacy read changed historical pricing/accounting fields')
            return len(rows)
        def log_files():
            return {path.name: digest(path) for path in output.iterdir() if path.is_file()}
        def usage_records():
            records = {}
            for path in output.glob('*.jsonl'):
                for line in path.read_bytes().splitlines(keepends=True):
                    if line.endswith(b'\n'):
                        record = json.loads(line)
                        records[record['request_id']] = record
            return records
        try:
            start(old, '0.3.1')
            request('openai'); request('anthropic')
            original = audits(ledger)
            report(installed, original)
            # SQLite backup API includes committed WAL data without raw file-copy races.
            consistent_backup(ledger, root / 'before-upgrade.db')
            stop()
            (root / 'config-0.3.1.json').write_bytes(config.read_bytes())
            upgraded = copy.deepcopy(value)
            upgraded['providers']['mock']['max_active_sessions'] = 2
            upgraded['providers']['mock']['prices']['fixture'].update(version='new', output_per_million=200)
            upgraded['usage_log'] = {'enabled': True, 'directory': str(output), 'poll_seconds': 1}
            config.write_text(json.dumps(upgraded))
            start(new, args.candidate_version)
            new_sources = {request('openai'): 'provider', request('anthropic'): 'provider', request('openai', estimated=True): 'local_estimate'}
            new_ids = list(new_sources)
            with upstream.lock: before_refusal = upstream.calls
            request('openai', SESSION + '-third', 429)
            with upstream.lock:
                if upstream.calls != before_refusal:
                    raise RuntimeError('new provider cap refusal dispatched upstream')
            current = audits(ledger)
            if not original.items() <= current.items():
                raise RuntimeError('upgrade rewrote historical audit records')
            for key in new_ids:
                record = json.loads(current[key])
                if not re.fullmatch('[0-9a-f]{64}', record.get('session_group', '')) or record.get('usage_source') != new_sources[key]:
                    raise RuntimeError('candidate did not persist its new optional audit metadata: protocol=' + str(record.get('protocol')) + ', group_length=' + str(len(record.get('session_group', ''))) + ', usage_source=' + str(record.get('usage_source')))
            wait_for(lambda: set(new_ids) <= usage_records().keys(), 'new audit metadata was not exported to usage JSONL')
            exported = usage_records()
            for key in new_ids:
                if exported[key]['session_group'] != json.loads(current[key])['session_group'] or exported[key]['usage_source'] != new_sources[key]:
                    raise RuntimeError('usage JSONL does not retain actual new audit metadata')
            report(installed, current)
            stop()
            consistent_backup(ledger, root / 'latest-upgrade.db')
            usage_before = log_files()
            key = Path(str(ledger) + '.usage-key')
            key_digest = digest(key)
            usage_sidecars = {path: digest(path) for path in root.glob('ledger.db.usage-*') if path.is_file()}
            config_source = root / 'config-0.3.2.json'; config_source.write_bytes(config.read_bytes())
            source_digest = digest(config_source)
            # Each extension independently prevents the old strict decoder reading config.
            for only in ('usage_log', 'max_active_sessions'):
                incompatible = copy.deepcopy(upgraded)
                if only == 'usage_log': incompatible['providers']['mock'].pop('max_active_sessions')
                else: incompatible.pop('usage_log')
                path = root / (only + '-not-compatible.json'); path.write_text(json.dumps(incompatible))
                result = subprocess.run([str(old), 'provider', 'list', '--config', str(path)], env=env, capture_output=True, text=True, timeout=10)
                if result.returncode == 0:
                    raise RuntimeError('baseline strict-decoder incompatibility was not reproduced')
                compatible = copy.deepcopy(incompatible)
                if only == 'usage_log': compatible.pop('usage_log')
                else: compatible['providers']['mock'].pop('max_active_sessions')
                control = root / (only + '-compatible-control.json'); control.write_text(json.dumps(compatible))
                cli(old, ['provider', 'list', '--config', str(control)])
            rolled_back = root / 'config-rollback.json'
            helper = subprocess.run([sys.executable, str(rollback_helper), '--input', str(config_source), '--output', str(rolled_back)], capture_output=True, text=True, timeout=10)
            if helper.returncode:
                raise RuntimeError('packaged rollback helper failed')
            receipt = json.loads(helper.stdout)
            expected_config = copy.deepcopy(upgraded); expected_config.pop('usage_log'); expected_config['providers']['mock'].pop('max_active_sessions')
            if json.loads(rolled_back.read_text()) != expected_config or digest(config_source) != source_digest:
                raise RuntimeError('rollback transformation changed other settings or source')
            config.write_bytes(rolled_back.read_bytes())
            report(old, current)
            # Restore-path read uses the latest consistent backup, preserving new history.
            backup_home = root / 'backup-home'
            backup_config = backup_home / 'Library/Application Support/TideMux/config.json'; backup_config.parent.mkdir(parents=True)
            backup_value = copy.deepcopy(expected_config); backup_value['ledger_path'] = str(root / 'latest-upgrade.db')
            backup_config.write_text(json.dumps(backup_value))
            report(old, current, dict(env, HOME=str(backup_home)))
            start(old, '0.3.1')
            for index in range(3): request('openai', SESSION + '-rolled-back-' + str(index))
            stop()
            final = audits(ledger)
            if not current.items() <= final.items() or len(final) != len(current) + 3:
                raise RuntimeError('rollback lost or rewrote old/new accounting history')
            report(installed, final)
            if digest(secrets) != secret_digest or digest(key) != key_digest or log_files() != usage_before or any(digest(path) != value for path, value in usage_sidecars.items()):
                raise RuntimeError('rollback modified Keychain fixture, usage identity or retained export files')
            with closing(sqlite3.connect(ledger)) as db:
                charges = db.execute('SELECT state FROM budget_charges').fetchall()
            with upstream.lock: dispatches = upstream.calls
            if len(charges) != len(final) or len(final) != dispatches or any(state != 'settled' for state, in charges):
                raise RuntimeError('upgrade/rollback dispatch, unique audit and settled-budget totals disagree')
            retained = ''.join(final.values()) + (root / 'stderr').read_text() + ''.join(path.read_text() for path in output.glob('*.jsonl'))
            if any(marker in retained for marker in (SESSION, PROMPT, GATEWAY_KEY, PROVIDER_KEY)):
                raise RuntimeError('accounting/export/runtime logs leaked private fixture values')
        finally:
            if process and process.poll() is None:
                process.kill(); process.wait()
            upstream.shutdown(); upstream.server_close()
        result = {'acceptance_stage': 'clean_package' if candidate else 'development_preview', 'final_package_acceptance': candidate is not None, 'baseline_version': '0.3.1', 'baseline_source': BASELINE_SOURCE, 'baseline_archive_sha256': baseline_sha, 'baseline_binary_sha256': digest(old), 'candidate_version': args.candidate_version, 'candidate_source': args.candidate_source, 'candidate_archive_sha256': candidate_sha, 'candidate_binary_sha256': digest(new), 'config_extensions_reproduced_and_removed': receipt['removed_fields'], 'other_settings_keychain_refs_preserved': True, 'sqlite_backup_integrity': True, 'old_binary_reads_new_audit_metadata': True, 'historic_record_json_preserved': True, 'retained_usage_files_unchanged': True, 'both_protocols': True, 'unique_dispatch_audit_budget_settlement': len(final), 'physical_mac_count': 1, 'paid_upstream_requests': 0}
        (evidence / 'upgrade-rollback.json').write_text(json.dumps(result, indent=2) + '\n')
        (evidence / 'baseline-BUILD.txt').write_text(baseline_build)
        (evidence / 'candidate-BUILD.txt').write_text(candidate_build)
        print(json.dumps(result))


if __name__ == '__main__':
    main()
