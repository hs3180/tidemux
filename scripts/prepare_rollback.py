#!/usr/bin/env python3
"""Create a new v0.3.1 configuration without changing source or user data."""
import argparse
import copy
import json
import math
import os
from pathlib import Path
import sys


class RollbackError(Exception):
    pass


def unique_object(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise RollbackError('configuration contains duplicate JSON keys')
        value[key] = item
    return value


def invalid_constant(_):
    raise RollbackError('configuration contains a non-JSON numeric value')


def finite_float(value):
    number = float(value)
    if not math.isfinite(number):
        raise RollbackError('configuration contains an unsupported numeric value')
    return number


def read_config(path):
    try:
        value = json.loads(path.read_text(encoding='utf-8'), object_pairs_hook=unique_object, parse_constant=invalid_constant, parse_float=finite_float)
    except (OSError, UnicodeError, json.JSONDecodeError) as error:
        raise RollbackError('cannot read a valid JSON configuration') from error
    if not isinstance(value, dict):
        raise RollbackError('configuration must be a JSON object')
    providers = value.get('providers', {})
    if not isinstance(providers, dict) or any(not isinstance(provider, dict) for provider in providers.values()):
        raise RollbackError('providers must contain profile objects')
    return value


def rollback_config(value):
    output = copy.deepcopy(value)
    removed_usage = 'usage_log' in output
    output.pop('usage_log', None)
    removed_caps = 0
    for provider in output.get('providers', {}).values():
        if 'max_active_sessions' in provider:
            removed_caps += 1
            del provider['max_active_sessions']
    return output, {'usage_log': removed_usage, 'providers.*.max_active_sessions': removed_caps}


def prepare(input_path, output_path):
    value = read_config(input_path)
    output, removed = rollback_config(value)
    descriptor = None
    created = False
    try:
        # O_EXCL also refuses existing symlinks and hard links. No destination,
        # including the input file, can be replaced by this tool.
        descriptor = os.open(output_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        created = True
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, 'w', encoding='utf-8') as stream:
            descriptor = None
            json.dump(output, stream, indent=2, allow_nan=False)
            stream.write('\n')
            stream.flush()
            os.fsync(stream.fileno())
    except OSError as error:
        if descriptor is not None:
            os.close(descriptor)
        if created:
            output_path.unlink(missing_ok=True)
        raise RollbackError('cannot create a new private output file; destination must not exist') from error
    return {'target_version': '0.3.1', 'removed_fields': removed,
            'disabled_features': ['provider session caps', 'usage JSONL export'],
            'source_preserved': True, 'user_data_modified': False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--input', required=True, type=Path, help='existing configuration; never modified')
    parser.add_argument('--output', required=True, type=Path, help='new v0.3.1 configuration; must not exist')
    args = parser.parse_args()
    try:
        receipt = prepare(args.input, args.output)
    except RollbackError as error:
        print(str(error), file=sys.stderr)
        return 1
    # Field names/counts only: never serialize configuration values or secrets.
    print(json.dumps(receipt))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
