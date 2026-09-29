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
keys=['TIDEMUX_GATEWAY_TOKEN','ANTHROPIC_API_KEY','ANTHROPIC_BASE_URL','ANTHROPIC_MODEL','CLAUDE_CODE_SIMPLE','CLAUDE_CONFIG_DIR','KILO_CONFIG_CONTENT','HERMES_HOME']
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
            self.wfile.write(json.dumps({'data': self.server.models}).encode())

        def log_message(self, *args):
            pass

    server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Discovery)
    server.models = [{'id': 'test-model'}]
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        url = 'http://127.0.0.1:' + str(server.server_port)
        for name in ['claude', 'kilo', 'hermes']:
            config = root / (name + '.json')
            config.write_text(json.dumps({
                'listen_addr': '127.0.0.1:' + str(server.server_port),
                'base_url': 'https://unused.example/v1',
                'upstream_id': 'test', 'anthropic_version': '2023-06-01',
                'upstream_keychain': {'service': 'must-not-read', 'account': 'test'},
                'access_token_keychain': {'service': 'test.gateway', 'account': 'test'},
                'max_in_flight': 1, 'ledger_path': str(root / 'unused.db')}))
            env = dict(os.environ, PATH=str(mock) + ':' + os.environ['PATH'], KILO_CONFIG_CONTENT='{}')
            before = config.read_bytes()
            result = subprocess.run([binary, name, '--config', str(config), '--model', 'legacy/test-model',
                                     '--executable', str(child),
                                     '--', 'argument with spaces', '--client-option'],
                                    env=env, capture_output=True, text=True, timeout=15, check=True)
            record = json.loads(result.stdout)
            assert record['args'][-2:] == ['argument with spaces', '--client-option']
            assert record['env']['TIDEMUX_GATEWAY_TOKEN'] == 'synthetic-local-token'
            assert config.read_bytes() == before
            if name == 'claude':
                assert record['env']['ANTHROPIC_BASE_URL'] == url
                assert record['env']['ANTHROPIC_API_KEY'] == 'synthetic-local-token'
                assert record['env']['ANTHROPIC_MODEL'] == 'legacy/test-model'
                assert record['env']['CLAUDE_CODE_SIMPLE'] == '1'
                assert record['args'] == ['argument with spaces', '--client-option']
            elif name == 'kilo':
                data = json.loads(record['env']['KILO_CONFIG_CONTENT'])
                assert data['model'] == 'tidemux-local/legacy/test-model'
                assert data['provider']['tidemux-local']['options']['baseURL'] == url + '/v1'
                assert 'synthetic-local-token' not in record['env']['KILO_CONFIG_CONTENT']
            else:
                data = Path(record['env']['HERMES_HOME']) / 'config.yaml'
                assert 'synthetic-local-token' not in data.read_text()
                assert json.loads(data.read_text())['providers']['tidemux-local']['transport'] == 'chat_completions'
            print('Direct client handoff passed:', name)
        assert len(calls) == 3

        # Exercise the installed candidate's launcher with the multi-provider
        # bare-ID contract. All client protocols select the unique provider
        # scope, and Kilo/Hermes receive that provider's declared capabilities.
        server.models = [{'id': 'glm/glm-5.3'}, {'id': 'secondary/other-model'}]
        providers = {
            'glm': {
                'protocol': 'anthropic', 'base_url': 'https://glm.example/v1',
                'anthropic_version': '2023-06-01', 'upstream_id': 'glm',
                'upstream_keychain': {'service': 'test.provider', 'account': 'glm'},
                'supported_models': ['glm-5.3'],
                'model_capabilities': {'context_tokens': 65536, 'max_output_tokens': 8192},
            },
            'secondary': {
                'protocol': 'openai', 'base_url': 'https://secondary.example/v1',
                'upstream_id': 'secondary',
                'upstream_keychain': {'service': 'test.provider', 'account': 'secondary'},
                'supported_models': ['other-model'],
                'model_capabilities': {'context_tokens': 32768, 'max_output_tokens': 4096},
            },
        }
        for name in ['claude', 'kilo', 'hermes']:
            config = root / (name + '-scoped.json')
            config.write_text(json.dumps({
                'listen_addr': '127.0.0.1:' + str(server.server_port),
                'access_token_keychain': {'service': 'test.gateway', 'account': 'test'},
                'max_in_flight': 1, 'ledger_path': str(root / (name + '-scoped.db')),
                'providers': providers,
            }))
            result = subprocess.run([binary, name, '--config', str(config), '--model', 'glm-5.3',
                                     '--executable', str(child), '--', 'bare-model-argument'],
                                    env=env, capture_output=True, text=True, timeout=15, check=True)
            record = json.loads(result.stdout)
            assert record['args'][-1] == 'bare-model-argument'
            if name == 'claude':
                assert record['env']['ANTHROPIC_MODEL'] == 'glm-5.3'
            elif name == 'kilo':
                data = json.loads(record['env']['KILO_CONFIG_CONTENT'])
                selected = data['provider']['tidemux-local']['models']['glm-5.3']['limit']
                assert data['model'] == 'tidemux-local/glm-5.3'
                assert selected == {'context': 65536, 'output': 8192}
            else:
                data = json.loads((Path(record['env']['HERMES_HOME']) / 'config.yaml').read_text())
                assert data['model']['default'] == 'glm-5.3'
                assert data['model']['context_length'] == 65536
            print('Unique bare-model provider scope passed:', name)

        ambiguous = root / 'ambiguous.json'
        ambiguous_providers = json.loads(json.dumps(providers))
        ambiguous_providers['secondary']['supported_models'] = ['glm-5.3']
        ambiguous.write_text(json.dumps({
            'listen_addr': '127.0.0.1:' + str(server.server_port),
            'access_token_keychain': {'service': 'test.gateway', 'account': 'test'},
            'max_in_flight': 1, 'ledger_path': str(root / 'ambiguous.db'),
            'providers': ambiguous_providers,
        }))
        result = subprocess.run([binary, 'claude', '--config', str(ambiguous), '--model', 'glm-5.3',
                                 '--executable', str(child)],
                                env=env, capture_output=True, text=True, timeout=15)
        assert result.returncode != 0 and 'matches multiple provider scopes' in result.stderr
        assert result.stdout == ''
        print('Ambiguous bare-model scope rejected before client launch')

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
