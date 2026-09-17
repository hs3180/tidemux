#!/usr/bin/env python3
"""Test direct CLI dispatch and credential/profile handoff with local doubles."""
import http.server
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading

ROOT = Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory(prefix='tidemux-client-command-') as temp:
    root = Path(temp)
    binary = os.environ.get('TIDEMUX_TEST_BINARY', str(root / 'tidemux'))
    if 'TIDEMUX_TEST_BINARY' not in os.environ:
        subprocess.run(['go', 'build', '-o', binary, './cmd/tidemux'], cwd=ROOT, check=True)
    mock = root / 'bin'
    mock.mkdir()
    security = mock / 'security'
    security.write_text('''#!/bin/sh
case "$*" in
  *test.gateway*) printf 'synthetic-local-token';;
  *) exit 9;;
esac
''')
    security.chmod(0o700)
    child = mock / 'client'
    child.write_text('''#!/usr/bin/env python3
import json,os,sys
keys=['TIDEMUX_GATEWAY_TOKEN','ANTHROPIC_API_KEY','ANTHROPIC_BASE_URL','CLAUDE_CONFIG_DIR','KILO_CONFIG_CONTENT','HERMES_HOME']
print(json.dumps({'args':sys.argv[1:],'env':{k:os.environ.get(k) for k in keys}}))
''')
    child.chmod(0o700)
    calls = []

    class Discovery(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            assert self.path == '/v1/models'
            assert self.headers.get('Authorization') == 'Bearer synthetic-local-token'
            calls.append(self.path)
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b'{"data":[{"id":"test-model"}]}')

        def log_message(self, *args):
            pass

    server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Discovery)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        url = 'http://127.0.0.1:' + str(server.server_port)
        for name in ['claude', 'kilo', 'hermes']:
            config = root / (name + '.json')
            config.write_text(json.dumps({
                'listen_addr': '127.0.0.1:' + str(server.server_port),
                'protocol': 'anthropic' if name == 'claude' else 'openai',
                'base_url': 'https://unused.example/v1', 'model': 'test-model',
                'upstream_id': 'test', 'anthropic_version': '2023-06-01',
                'upstream_keychain': {'service': 'must-not-read', 'account': 'test'},
                'access_token_keychain': {'service': 'test.gateway', 'account': 'test'},
                'max_in_flight': 1, 'ledger_path': str(root / 'unused.db')}))
            env = dict(os.environ, PATH=str(mock) + ':' + os.environ['PATH'], KILO_CONFIG_CONTENT='{}')
            before = config.read_bytes()
            result = subprocess.run([binary, name, '--config', str(config), '--executable', str(child),
                                     '--', 'argument with spaces', '--client-option'],
                                    env=env, capture_output=True, text=True, timeout=15, check=True)
            record = json.loads(result.stdout)
            assert record['args'][-2:] == ['argument with spaces', '--client-option']
            assert record['env']['TIDEMUX_GATEWAY_TOKEN'] == 'synthetic-local-token'
            assert config.read_bytes() == before
            if name == 'claude':
                assert record['env']['ANTHROPIC_BASE_URL'] == url
                assert record['env']['ANTHROPIC_API_KEY'] == 'synthetic-local-token'
                assert record['args'][:2] == ['--model', 'test-model']
            elif name == 'kilo':
                data = json.loads(record['env']['KILO_CONFIG_CONTENT'])
                assert data['model'] == 'tidemux-local/test-model'
                assert data['provider']['tidemux-local']['options']['baseURL'] == url + '/v1'
                assert 'synthetic-local-token' not in record['env']['KILO_CONFIG_CONTENT']
            else:
                data = Path(record['env']['HERMES_HOME']) / 'config.yaml'
                assert 'synthetic-local-token' not in data.read_text()
                assert json.loads(data.read_text())['providers']['tidemux-local']['transport'] == 'chat_completions'
            print('Direct client handoff passed:', name)
        assert len(calls) == 3
        for old in ['connect', 'launch']:
            result = subprocess.run([binary, old, 'claude', '--help'], capture_output=True, timeout=5)
            assert result.returncode != 0
        for args in [['ledger'], ['ledger', '--diagnostics']]:
            result = subprocess.run([binary, *args, '--config', str(config)], capture_output=True, timeout=5)
            assert result.returncode != 0 and result.stdout == b''
        print('Removed command rejection passed: connect, launch, ledger')
    finally:
        server.shutdown()
        server.server_close()
