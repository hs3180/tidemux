#!/usr/bin/env python3
"""Collect the same package-produced session files twice into isolated loopback ES.

--records is the expected JSON array emitted by test_usage_logs_package.py.
No production configuration, index, registry or credential is accessed.
"""
import argparse
from datetime import datetime
import json
import hashlib
import os
from pathlib import Path
import subprocess
import time
from urllib.error import HTTPError
from urllib.parse import urlparse
from urllib.request import Request, urlopen

from runtime_event_checks import validate_runtime_events

ROOT = Path(__file__).resolve().parents[1]


def published_events(path):
    """Return output deltas, excluding the cumulative shutdown report."""
    counts = {"total": 0, "acked": 0, "duplicates": 0}
    for line in path.read_text().splitlines():
        try:
            entry = json.loads(line)
        except ValueError:
            continue
        if not entry.get("message", "").startswith("Non-zero metrics"):
            continue
        events = entry.get("monitoring", {}).get("metrics", {}).get("libbeat", {}).get("output", {}).get("events", {})
        for key in counts:
            counts[key] += events.get(key, 0)
    return counts


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--binary", type=Path)
    source.add_argument("--fixture", type=Path, help="directory prepared from the Darwin package")
    parser.add_argument("--logs", required=True, type=Path)
    parser.add_argument("--records", required=True, type=Path)
    parser.add_argument("--es-url", required=True)
    parser.add_argument("--index-prefix", required=True)
    parser.add_argument("--evidence", required=True, type=Path)
    parser.add_argument("--filebeat", default="filebeat")
    args = parser.parse_args()
    parsed = urlparse(args.es_url)
    if (parsed.scheme != "http" or parsed.hostname not in ("127.0.0.1", "localhost", "::1")
            or parsed.username or parsed.password or parsed.path not in ("", "/") or parsed.query or parsed.fragment):
        raise SystemExit("Use an explicitly isolated, credential-free loopback HTTP ES fixture")
    if not args.index_prefix.startswith("tidemux-smoke-") or any(
            c not in "abcdefghijklmnopqrstuvwxyz0123456789-" for c in args.index_prefix):
        raise SystemExit("Use a fresh lowercase tidemux-smoke-* prefix")
    root = args.evidence.resolve()
    root.mkdir(parents=True, exist_ok=False)
    records = json.loads(args.records.read_text())
    if args.binary:
        version = subprocess.check_output([str(args.binary.resolve()), "version"], text=True).strip()
        binary_sha256 = hashlib.sha256(args.binary.read_bytes()).hexdigest()
    else:
        metadata = json.loads((args.fixture / "fixture.json").read_text())
        version, binary_sha256 = metadata["binary_version"], metadata["binary_sha256"]
    validate_runtime_events(records, version, one_instance=False)
    expected = {record["event_id"]: record for record in records}
    actual = [json.loads(line) for path in args.logs.glob("projects/tidemux/*.jsonl")
              for line in path.read_text().splitlines()]
    if len(actual) != len(expected) or {r["event_id"]: r for r in actual} != expected:
        raise RuntimeError("Log files disagree with the package gate's expected records")

    def es(method, path, value=None):
        request = Request(args.es_url.rstrip("/") + path,
                          None if value is None else json.dumps(value).encode(),
                          {"Content-Type": "application/json"}, method=method)
        with urlopen(request, timeout=10) as response:
            return json.load(response)

    es_version = es("GET", "/")["version"]["number"]
    if es_version != "8.19.5":
        raise RuntimeError("Expected ES 8.19.5, got " + es_version)
    if es("GET", "/_cat/indices/" + args.index_prefix + "*?format=json"):
        raise RuntimeError("Index namespace is already in use")
    template = json.loads((ROOT / "scripts/fixtures/elasticsearch/index-template.json").read_text())
    template["index_patterns"] = [args.index_prefix + "*"]
    es("PUT", "/_index_template/" + args.index_prefix, template)
    source = (ROOT / "scripts/fixtures/elasticsearch/filebeat.yml").read_text()
    config = root / "filebeat.yml"
    config.write_text("\n".join(line for line in source.splitlines()
                                if "api_key:" not in line and "ssl.certificate_authorities:" not in line) + "\n")
    env = dict(os.environ, TIDEMUX_LOG_PATH=str(args.logs.resolve() / "projects/tidemux/*.jsonl"),
               TIDEMUX_ES_URL=args.es_url, TIDEMUX_ES_INDEX_PREFIX=args.index_prefix)
    tool_version = subprocess.check_output([args.filebeat, "version"], text=True).strip()
    subprocess.run([args.filebeat, "test", "config", "-c", str(config)], env=env, check=True)
    documents = []
    replay_output = []
    for replay in (1, 2):
        log_path = root / ("filebeat-replay-%d.log" % replay)
        log = log_path.open("w")
        process = subprocess.Popen([args.filebeat, "-e", "-c", str(config),
                                    "-E", "logging.metrics.period=1s",
                                    "--path.data", str(root / ("data-%d" % replay)),
                                    "--path.logs", str(root / ("logs-%d" % replay))],
                                   env=env, stdout=log, stderr=subprocess.STDOUT)
        try:
            deadline = time.monotonic() + 60
            while time.monotonic() < deadline:
                if process.poll() is not None:
                    raise RuntimeError("Filebeat exited before ingestion: %s" % process.returncode)
                try:
                    es("POST", "/" + args.index_prefix + "*/_refresh")
                    documents = es("POST", "/" + args.index_prefix + "*/_search",
                                   {"size": len(expected) + 1, "version": True,
                                    "query": {"match_all": {}}})["hits"]["hits"]
                    output = published_events(log_path)
                    # Filebeat 9.x uses create: a replay is a duplicate (409)
                    # and leaves _version=1. Index can instead yield version 2.
                    if (len(documents) == len(expected) and {hit["_id"] for hit in documents} == expected.keys()
                            and output["total"] >= len(expected)
                            and output["acked"] + output["duplicates"] >= len(expected)
                            and all(hit["_version"] in (1, replay) for hit in documents)):
                        replay_output.append(output)
                        break
                except HTTPError as error:
                    if error.code != 404:
                        raise
                time.sleep(.5)
            else:
                raise RuntimeError("Filebeat ingestion/replay timed out")
        finally:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
            log.close()
    for hit in documents:
        source = hit["_source"]
        if source["tidemux"] != expected[hit["_id"]]:
            raise RuntimeError("Collector changed the persisted event")
        native = datetime.fromisoformat(source["tidemux"]["timestamp"].replace("Z", "+00:00"))
        indexed = datetime.fromisoformat(source["@timestamp"].replace("Z", "+00:00"))
        if native != indexed:
            raise RuntimeError("Collector changed event time")
    fields = ("input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens")
    aggregation = es("POST", "/" + args.index_prefix + "*/_search", {"size": 0, "aggs": {
        field: {"sum": {"field": "tidemux.message.usage." + field}} for field in fields}})
    counts = {field: sum(r["message"].get("usage", {}).get(field, 0) for r in records) for field in fields}
    if any(aggregation["aggregations"][field]["value"] != count for field, count in counts.items()):
        raise RuntimeError("Indexed usage totals disagree with the package records")
    known = sum("usage" in r["message"] for r in records)
    coverage = es("POST", "/" + args.index_prefix + "*/_count", {
        "query": {"exists": {"field": "tidemux.message.usage.input_tokens"}}})["count"]
    if coverage != known:
        raise RuntimeError("Collector fabricated or removed usage")
    for name, data in (("documents", documents), ("aggregation", aggregation)):
        (root / (name + ".json")).write_text(json.dumps(data, indent=2) + "\n")
    result = {"passed": True, "binary_version": version, "binary_sha256": binary_sha256, "filebeat": tool_version,
              "elasticsearch": es_version, "distinct_events": len(expected), "replays": 2,
              "replay_output": replay_output, "document_versions": sorted({hit["_version"] for hit in documents}),
              "exact_record_and_event_time": True, "known_usage_records": known, "tokens": counts}
    (root / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result))


if __name__ == "__main__":
    main()
