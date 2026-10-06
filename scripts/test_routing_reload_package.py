#!/usr/bin/env python3
"""Verify reload routing/session priority and admitted snapshots in one package."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import copy
import hashlib
from http.server import BaseHTTPRequestHandler
import json
import os
from pathlib import Path
import signal
import sqlite3
import subprocess
import tempfile
import threading
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from test_hot_reload_package import wait_for, write_config
from test_shared_model_affinity_package import Server, free_port

GATEWAY_KEY = 'routing-private-gateway-key'
KEY = 'routing-private-provider-key'
PROMPT = 'routing-private-prompt'
SESSION = 'routing-private-session'


class Upstream(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

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
        self.reply({'object': 'list', 'data': [{'id': m} for m in ('one', 'two', 'shared')]})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        with self.server.lock:
            self.server.calls.append((self.path.split('/')[1], body['model'], self.headers.get('Authorization'), self.client_address))
        if body.get('stream'):
            first = b'data: {"choices":[{"index":0,"delta":{"content":"held"}}]}\n\n'
            last = b'data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}\n\ndata: [DONE]\n\n'
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.send_header('Content-Length', str(len(first) + len(last)))
            self.end_headers()
            self.wfile.write(first)
            self.wfile.flush()
            self.server.started.set()
            self.server.release.wait(40)
            self.wfile.write(last)
            self.wfile.flush()
        else:
            self.reply({'id': 'mock', 'object': 'chat.completion', 'model': body['model'], 'choices': [{'index': 0, 'message': {'role': 'assistant', 'content': 'ok'}, 'finish_reason': 'stop'}], 'usage': {'prompt_tokens': 3, 'completion_tokens': 2}})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', required=True, type=Path)
    binary = parser.parse_args().binary.resolve()
    with tempfile.TemporaryDirectory(prefix='tidemux-routing-reload.') as temporary:
        root = Path(temporary)
        fake = root / 'bin'
        fake.mkdir()
        secrets = root / 'secrets.json'
        secrets.write_text(json.dumps({'gateway/local': GATEWAY_KEY, 'provider/old': KEY, 'provider/new': KEY + '-new'}))
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
''')
        security.chmod(0o700)
        env = dict(os.environ, PATH=str(fake) + ':' + os.environ.get('PATH', ''), HOME=str(root / 'home'), TIDEMUX_TEST_KEYCHAIN_DB=str(secrets))
        upstream = Server(('127.0.0.1', 0), Upstream)
        upstream.lock = threading.Lock()
        upstream.calls = []
        upstream.started = threading.Event()
        upstream.release = threading.Event()
        threading.Thread(target=upstream.serve_forever, daemon=True).start()
        endpoint = f'http://127.0.0.1:{upstream.server_port}'
        port = free_port()
        url = f'http://127.0.0.1:{port}'
        config, ledger = root / 'config.json', root / 'ledger.db'
        prices = {'currency': 'USD', 'source': 'mock', 'version': 'old', 'input_cache_miss_per_million': 1, 'input_cache_hit_per_million': .1, 'output_per_million': 2}
        budget = {'currency': 'USD', 'five_hour_limit': 100, 'weekly_limit': 100, 'mode': 'hard', 'alert_threshold': .8}
        def provider(path, models):
            return {'protocol': 'openai', 'base_url': endpoint + '/' + path + '/v1', 'upstream_keychain': {'service': 'provider', 'account': 'old'}, 'supported_models': models, 'prices': {m: copy.deepcopy(prices) for m in models}, 'budget': copy.deepcopy(budget)}
        value = {'listen_addr': f'127.0.0.1:{port}', 'max_in_flight': 1, 'ledger_path': str(ledger), 'access_token_keychain': {'service': 'gateway', 'account': 'local'}, 'routing': {'shared_model_strategy': 'random'}, 'providers': {'a': provider('a', ['one', 'shared']), 'b': provider('b', ['two'])}, 'auto_chain': [{'provider': 'a', 'model': 'one'}, {'provider': 'b', 'model': 'two'}]}
        write_config(config, value)
        def call(model, session=SESSION, stream=False, path='/v1/chat/completions'):
            body = {'model': model, 'max_tokens': 32, 'stream': stream, 'messages': [{'role': 'user', 'content': PROMPT}]}
            req = Request(url + path, json.dumps(body).encode(), {'Authorization': 'Bearer ' + GATEWAY_KEY, 'Content-Type': 'application/json', 'X-TideMux-Session-ID': session})
            try:
                return urlopen(req, timeout=40)
            except HTTPError as error:
                return error
        def finish(model, expected, session=SESSION, path='/v1/chat/completions'):
            with call(model, session, path=path) as response:
                content = response.read()
                if response.status != 200:
                    raise RuntimeError('request rejected: ' + content.decode())
                request_id = response.headers['X-TideMux-Request-ID']
            with upstream.lock:
                actual = upstream.calls[-1][:2]
            if actual != expected:
                raise RuntimeError(f'session routing differs: model={model} actual={actual} expected={expected}')
            return request_id
        def status():
            with urlopen(Request(url + '/tidemux/config-status', headers={'Authorization': 'Bearer ' + GATEWAY_KEY}), timeout=5) as response:
                return json.load(response)
        def apply(next_value):
            write_config(config, next_value)
            revision = hashlib.sha256(config.read_bytes()).hexdigest()
            wait_for(lambda: status()['status'] == 'applied' and status()['applied_revision'] == revision, 'exact routing view')
        def pending():
            with sqlite3.connect(ledger) as db:
                return db.execute("SELECT COUNT(*) FROM budget_charges WHERE state='pending'").fetchone()[0]
        process = None
        try:
            with (root / 'stdout').open('w') as stdout, (root / 'stderr').open('w') as stderr:
                process = subprocess.Popen([str(binary), 'serve', '--config', str(config)], env=env, stdout=stdout, stderr=stderr)
                wait_for(lambda: 'listening' in (root / 'stdout').read_text(), 'startup', 10)
                apply(value)
                finish('auto', ('a', 'one'))
                finish('shared', ('a', 'shared'), SESSION + '-shared')
                next_value = copy.deepcopy(value)
                next_value['auto_chain'].reverse()
                apply(next_value)
                # Reorder changes new-session preference, preserving valid existing identity.
                finish('auto', ('a', 'one'))
                finish('auto', ('b', 'two'), SESSION + '-new', '/v1/messages')
                next_value['providers']['b']['supported_models'].append('shared')
                next_value['providers']['b']['prices']['shared'] = copy.deepcopy(prices)
                apply(next_value)
                finish('shared', ('a', 'shared'), SESSION + '-shared')
                with call('auto', stream=True) as held, ThreadPoolExecutor(max_workers=1) as pool:
                    if not upstream.started.wait(5):
                        raise RuntimeError('SSE did not start')
                    held_id = held.headers['X-TideMux-Request-ID']
                    while held.readline().strip():
                        pass
                    queued = pool.submit(lambda: finish('auto', ('a', 'one')))
                    wait_for(lambda: pending() == 2, 'admitted queued reservation', 5)
                    changed = copy.deepcopy(next_value)
                    changed['auto_chain'].reverse()
                    a = changed['providers']['a']
                    a['base_url'] = endpoint + '/a-new/v1'
                    a['upstream_keychain']['account'] = 'new'
                    a['budget']['currency'] = 'EUR'
                    for price in a['prices'].values():
                        price.update(currency='EUR', version='new', output_per_million=200)
                    apply(changed)
                    upstream.release.set()
                    tail = held.read().decode()
                    if '[DONE]' not in tail or 'event: error' in tail:
                        raise RuntimeError('reload interrupted old SSE')
                    queued_id = queued.result(timeout=10)
                finish('auto', ('a-new', 'one'), SESSION + '-fresh')
                # Rules-equivalent random candidates prefer compatible b before old a affinity.
                finish('shared', ('b', 'shared'), SESSION + '-shared')
                finish('a/shared', ('a-new', 'shared'), SESSION + '-explicit')
                removed = copy.deepcopy(changed)
                del removed['providers']['a']
                removed['auto_chain'] = [{'provider': 'b', 'model': 'two'}]
                apply(removed)
                finish('auto', ('b', 'two'))
                with call('a/one') as response:
                    if response.status != 404:
                        raise RuntimeError('removed provider remained eligible')
                restored = copy.deepcopy(removed)
                restored['providers']['a'] = copy.deepcopy(value['providers']['a'])
                restored['auto_chain'] = copy.deepcopy(value['auto_chain'])
                apply(restored)
                finish('auto', ('b', 'two'))
                finish('auto', ('a', 'one'), SESSION + '-readded')
                with sqlite3.connect(ledger) as db:
                    records = [json.loads(row[0]) for row in db.execute('SELECT record_json FROM request_audit')]
                    charges = db.execute('SELECT audit_id,currency,charged_amount,state FROM budget_charges').fetchall()
                with upstream.lock:
                    calls = list(upstream.calls)
                if len(records) != len(calls) or len({r['id'] for r in records}) != len(calls) or len(charges) != len(calls) or any(row[3] != 'settled' for row in charges):
                    raise RuntimeError('dispatch/audit/budget terminal counts disagree')
                old = [row for row in charges if row[0] in (held_id, queued_id)]
                if len(old) != 2 or any(row[1] != 'USD' or row[2] > .001 for row in old):
                    raise RuntimeError('queued/SSE admitted policy snapshot changed')
                if not any(p == 'a-new' and key == 'Bearer ' + KEY + '-new' for p, _, key, _ in calls):
                    raise RuntimeError('new endpoint/credential was not used')
                if process.poll() is not None:
                    raise RuntimeError('gateway exited during reload')
                process.send_signal(signal.SIGTERM)
                process.wait(timeout=10)
                logs = (root / 'stderr').read_text()
                if process.returncode or any(marker in logs for marker in (KEY, PROMPT, SESSION, GATEWAY_KEY)):
                    raise RuntimeError('shutdown failed or logs leaked private fixtures')
        finally:
            upstream.release.set()
            if process and process.poll() is None:
                process.kill()
                process.wait()
            upstream.shutdown()
            upstream.server_close()
    print(json.dumps({'binary': str(binary), 'one_process': True, 'chain_reorder_partial_remove_readd': True, 'rule_connection_affinity_priority': True, 'queued_sse_original_endpoint_keys_prices_budget': True, 'both_protocols': True, 'unique_dispatch_audit_settlement': True, 'privacy': True}))


if __name__ == '__main__':
    main()
