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
            markers = [marker for marker in self.server.expected if marker in json.dumps(body['messages'])]
            if len(markers) != 1:
                raise RuntimeError('upstream request has no unique fixture identity')
            self.server.calls.append((markers[0], self.path.split('/')[1], body['model'], self.headers.get('Authorization')))
        if body.get('stream'):
            first = b'data: {"choices":[{"index":0,"delta":{"content":"held"}}]}\n\n'
            last = b'data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":0}}}\n\ndata: [DONE]\n\n'
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
            self.reply({'id': 'mock', 'object': 'chat.completion', 'model': body['model'], 'choices': [{'index': 0, 'message': {'role': 'assistant', 'content': 'ok'}, 'finish_reason': 'stop'}], 'usage': {'prompt_tokens': 3, 'completion_tokens': 2, 'prompt_tokens_details': {'cached_tokens': 0}}})


def verify(binary, client_path):
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
        upstream.expected = {}
        upstream.started = threading.Event()
        upstream.release = threading.Event()
        threading.Thread(target=upstream.serve_forever, daemon=True).start()
        endpoint = f'http://127.0.0.1:{upstream.server_port}'
        port = free_port()
        url = f'http://127.0.0.1:{port}'
        config, ledger = root / 'config.json', root / 'ledger.db'
        prices = {'currency': 'USD', 'source': 'mock', 'version': 'old', 'input_cache_miss_per_million': 1, 'input_cache_hit_per_million': .1, 'output_per_million': 2}
        new_prices = dict(prices, currency='EUR', source='mock-new', version='new', input_cache_miss_per_million=100, input_cache_hit_per_million=10, output_per_million=200)
        budget = {'currency': 'USD', 'five_hour_limit': 100, 'weekly_limit': 100, 'mode': 'hard', 'alert_threshold': .8}
        def provider(path, models):
            return {'protocol': 'openai', 'base_url': endpoint + '/' + path + '/v1', 'upstream_keychain': {'service': 'provider', 'account': 'old'}, 'supported_models': models, 'prices': {m: copy.deepcopy(prices) for m in models}, 'budget': copy.deepcopy(budget)}
        value = {'listen_addr': f'127.0.0.1:{port}', 'max_in_flight': 1, 'ledger_path': str(ledger), 'access_token_keychain': {'service': 'gateway', 'account': 'local'}, 'routing': {'shared_model_strategy': 'random'}, 'providers': {'a': provider('a', ['one', 'shared']), 'b': provider('b', ['two'])}, 'auto_chain': [{'provider': 'a', 'model': 'one'}, {'provider': 'b', 'model': 'two'}]}
        write_config(config, value)
        history = {}
        verified = []
        def register(expected, key=KEY, price=prices):
            with upstream.lock:
                marker = PROMPT + '-%04d' % (len(upstream.expected) + 1)
                # The fixture reports all usage needed for known costs: three
                # uncached input and two output tokens, with fixed old/new rates.
                upstream.expected[marker] = {'route': expected, 'key': key, 'price': copy.deepcopy(price), 'cost': .0007 if price == new_prices else .000007}
            return marker
        def dispatch(marker):
            with upstream.lock:
                matches = [call for call in upstream.calls if call[0] == marker]
            expected = upstream.expected[marker]
            if len(matches) != 1 or matches[0][1:] != (*expected['route'], 'Bearer ' + expected['key']):
                raise RuntimeError('fixture request endpoint/model/credential or unique dispatch differs')
        def snapshot():
            with sqlite3.connect('file:' + str(ledger) + '?mode=ro', uri=True) as db:
                rows = db.execute('SELECT id,protocol,model,status,input_tokens,output_tokens,estimated_cost,currency,record_json FROM request_audit').fetchall()
                charges = db.execute('SELECT request_id,audit_id,provider_scope,currency,charged_amount,state,charged_at_ms FROM budget_charges ORDER BY request_id').fetchall()
            return {row[0]: row[1:] for row in rows}, charges
        def check_history():
            rows, charges = snapshot()
            for audit_id, (original, charge) in history.items():
                if rows.get(audit_id) != original or [row for row in charges if row[1] == audit_id] != [charge]:
                    raise RuntimeError('reload rewrote a completed audit JSON/columns or budget charge')
        def finalized(marker, audit_id):
            def settled():
                rows, charges = snapshot()
                return audit_id in rows and any(row[1] == audit_id and row[5] == 'settled' for row in charges)
            wait_for(settled, 'exact request audit and settled budget charge', 5)
            dispatch(marker)
            rows, charges = snapshot()
            raw = rows[audit_id]
            audit = json.loads(raw[-1])
            expected = upstream.expected[marker]
            provider_ref = 'a' if expected['route'][0] == 'a-new' else expected['route'][0]
            if raw[:-1] != ('openai', expected['route'][1], 'ok', 3, 2, expected['cost'], expected['price']['currency']):
                raise RuntimeError('committed audit columns changed admitted usage/cost policy')
            if any(audit.get(field) != value for field, value in {'id': audit_id, 'protocol': 'openai', 'provider_ref': provider_ref, 'model': expected['route'][1], 'status': 'ok', 'input_tokens': 3, 'output_tokens': 2, 'cache_read_tokens': 0, 'price_snapshot': expected['price'], 'estimated_cost': expected['cost'], 'currency': expected['price']['currency']}.items()):
                raise RuntimeError('committed audit lost exact admitted price source/version/rates/estimate')
            matched = [row for row in charges if row[1] == audit_id]
            if len(matched) != 1 or not matched[0][0] or matched[0][1:6] != (audit_id, provider_ref, expected['price']['currency'], expected['cost'], 'settled'):
                raise RuntimeError('exact unique budget settlement differs from admitted policy')
            if audit_id in history:
                raise RuntimeError('fixture reused an audit identity')
            history[audit_id] = (raw, matched[0])
            verified.append({'audit_id': audit_id, 'endpoint': expected['route'][0], 'model': expected['route'][1], 'credential_generation': 'new' if expected['key'] != KEY else 'old', 'price_snapshot': audit['price_snapshot'], 'estimated_cost': audit['estimated_cost'], 'budget_charge': {'reservation_id': matched[0][0], 'provider_scope': matched[0][2], 'currency': matched[0][3], 'amount': matched[0][4], 'state': matched[0][5]}})
            check_history()
            return matched[0][0]
        def call(model, session=SESSION, stream=False, path=client_path, marker=PROMPT):
            body = {'model': model, 'max_tokens': 32, 'stream': stream, 'messages': [{'role': 'user', 'content': marker}]}
            req = Request(url + path, json.dumps(body).encode(), {'Authorization': 'Bearer ' + GATEWAY_KEY, 'Content-Type': 'application/json', 'X-TideMux-Session-ID': session})
            try:
                return urlopen(req, timeout=40)
            except HTTPError as error:
                return error
        def finish(model, expected, session=SESSION, key=KEY, price=prices):
            marker = register(expected, key, price)
            with call(model, session, marker=marker) as response:
                content = response.read()
                if response.status != 200:
                    raise RuntimeError('request rejected: ' + content.decode())
                request_id = response.headers['X-TideMux-Request-ID']
            finalized(marker, request_id)
            return request_id
        def status():
            with urlopen(Request(url + '/tidemux/config-status', headers={'Authorization': 'Bearer ' + GATEWAY_KEY}), timeout=5) as response:
                return json.load(response)
        def apply(next_value):
            write_config(config, next_value)
            revision = hashlib.sha256(config.read_bytes()).hexdigest()
            wait_for(lambda: status()['status'] == 'applied' and status()['applied_revision'] == revision, 'exact routing view')
            check_history()
        def pending():
            return [row for row in snapshot()[1] if row[5] == 'pending']
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
                finish('auto', ('b', 'two'), SESSION + '-new')
                next_value['providers']['b']['supported_models'].append('shared')
                next_value['providers']['b']['prices']['shared'] = copy.deepcopy(prices)
                apply(next_value)
                finish('shared', ('a', 'shared'), SESSION + '-shared')
                held_marker = register(('a', 'one'))
                with call('auto', stream=True, marker=held_marker) as held, ThreadPoolExecutor(max_workers=1) as pool:
                    if not upstream.started.wait(5):
                        raise RuntimeError('SSE did not start')
                    held_id = held.headers['X-TideMux-Request-ID']
                    dispatch(held_marker)
                    while held.readline().strip():
                        pass
                    queued = pool.submit(lambda: finish('auto', ('a', 'one')))
                    wait_for(lambda: len(pending()) == 2, 'admitted held and queued reservations', 5)
                    original_pending = pending()
                    if any(row[1] != '' or row[2:6] != ('a', 'USD', 0, 'pending') for row in original_pending):
                        raise RuntimeError('held/queued reservations do not use their admitted provider/budget')
                    changed = copy.deepcopy(next_value)
                    changed['auto_chain'].reverse()
                    a = changed['providers']['a']
                    a['base_url'] = endpoint + '/a-new/v1'
                    a['upstream_keychain']['account'] = 'new'
                    a['budget']['currency'] = 'EUR'
                    for price in a['prices'].values():
                        price.update(new_prices)
                    apply(changed)
                    if pending() != original_pending:
                        raise RuntimeError('reload changed the held/queued reservation identities or policies')
                    upstream.release.set()
                    tail = held.read().decode()
                    terminal = '[DONE]' if client_path == '/v1/chat/completions' else 'event: message_stop'
                    if terminal not in tail or 'event: error' in tail:
                        raise RuntimeError('reload interrupted old SSE')
                    queued_id = queued.result(timeout=10)
                    finalized(held_marker, held_id)
                    rows, charges = snapshot()
                    if {row[0] for row in charges if row[1] in (held_id, queued_id)} != {row[0] for row in original_pending}:
                        raise RuntimeError('old requests did not settle their original reservations exactly once')
                finish('auto', ('a-new', 'one'), SESSION + '-fresh', KEY + '-new', new_prices)
                # Rules-equivalent random candidates prefer compatible b before old a affinity.
                finish('shared', ('b', 'shared'), SESSION + '-shared')
                finish('a/shared', ('a-new', 'shared'), SESSION + '-explicit', KEY + '-new', new_prices)
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
                if set(history) != {record['id'] for record in records} or len(upstream.expected) != len(calls):
                    raise RuntimeError('dispatch/audit identities lack per-request assertions')
                check_history()
                for marker in upstream.expected:
                    dispatch(marker)
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
    return {'client_protocol': 'openai' if client_path == '/v1/chat/completions' else 'anthropic', 'one_process': True, 'requests': verified, 'dispatches': len(verified), 'historical_audit_json_and_charges_unchanged': True, 'held_queued_original_reservations_settled': True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', required=True, type=Path)
    binary = parser.parse_args().binary.resolve()
    results = [verify(binary, path) for path in ('/v1/chat/completions', '/v1/messages')]
    print(json.dumps({'binary': str(binary), 'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(), 'one_process_per_protocol': True, 'chain_reorder_partial_remove_readd': True, 'rule_connection_affinity_priority': True, 'queued_sse_original_endpoint_keys_prices_budget': True, 'per_request_dispatch_credentials_prices_and_charges': True, 'historical_audit_json_and_charges_unchanged': True, 'both_protocols': True, 'unique_dispatch_audit_settlement': True, 'privacy': True, 'results': results}))


if __name__ == '__main__':
    main()
