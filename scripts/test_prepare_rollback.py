#!/usr/bin/env python3
"""Check rollback transformation, privacy, strict JSON and no-overwrite behavior."""
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest

from prepare_rollback import RollbackError, prepare, read_config, rollback_config


class RollbackTests(unittest.TestCase):
    def test_only_new_fields_are_removed_and_source_is_preserved(self):
        value = {'access_token_keychain': {'service': 'private-service', 'account': 'private-account'},
                 'max_active_sessions': 21, 'auto_chain': [{'provider': 'one', 'model': 'm'}],
                 'usage_log': {'enabled': True, 'directory': '/private-usage'},
                 'providers': {'one': {'max_active_sessions': 5, 'budget': {'currency': 'USD'}, 'upstream_keychains': [{'service': 'upstream-private', 'account': 'a'}]}, 'two': {'max_active_sessions': 0, 'supported_models': ['other']}},
                 'future_field': {'untouched': True}}
        expected = json.loads(json.dumps(value)); del expected['usage_log']
        for provider in expected['providers'].values(): del provider['max_active_sessions']
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary); source, output = root / 'source.json', root / 'output.json'
            original = json.dumps(value).encode(); source.write_bytes(original)
            receipt = prepare(source, output)
            self.assertEqual(source.read_bytes(), original)
            self.assertEqual(json.loads(output.read_text()), expected)
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
            self.assertEqual(receipt['removed_fields'], {'usage_log': True, 'providers.*.max_active_sessions': 2})
            self.assertNotIn('private', json.dumps(receipt))

    def test_existing_destinations_and_input_aliases_are_refused(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary); source = root / 'source.json'; source.write_text('{}')
            for kind in ('same', 'regular', 'symlink', 'hardlink'):
                output = root / kind
                if kind == 'same': output = source
                elif kind == 'regular': output.write_text('preserve me')
                elif kind == 'symlink': output.symlink_to(source)
                else: os.link(source, output)
                before = output.read_bytes()
                with self.assertRaises(RollbackError): prepare(source, output)
                self.assertEqual(output.read_bytes(), before)
                self.assertEqual(source.read_text(), '{}')

    def test_ambiguous_invalid_json_is_refused_without_output(self):
        for content in ('{"providers":{},"providers":{}}', '{"x":NaN}', '{"x":1e999}', '[]', '{"providers":[]}', '{"providers":{"p":[]}}', '{bad'):
            with tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary); source = root / 'source.json'; output = root / 'output.json'; source.write_text(content)
                with self.assertRaises(RollbackError): prepare(source, output)
                self.assertFalse(output.exists())
                self.assertEqual(source.read_text(), content)

    def test_cli_receipt_never_echoes_values(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary); source, output = root / 'in.json', root / 'out.json'
            source.write_text('{"usage_log":{"directory":"sensitive-sentinel"},"providers":{"sensitive-ref":{"max_active_sessions":5,"opaque":"sensitive-sentinel"}}}')
            result = subprocess.run([sys.executable, str(Path(__file__).with_name('prepare_rollback.py')), '--input', str(source), '--output', str(output)], text=True, capture_output=True)
            self.assertEqual(result.returncode, 0)
            self.assertNotIn('sensitive', result.stdout + result.stderr)
            self.assertEqual(json.loads(output.read_text())['providers']['sensitive-ref']['opaque'], 'sensitive-sentinel')


if __name__ == '__main__':
    unittest.main()
