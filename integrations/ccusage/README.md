# TideMux source adapter for ccusage

This maintained adapter is compiled into the actual upstream Rust `ccusage`
binary. It is available from this repository now and does not depend on an
unmerged external pull request. It is not present in the upstream npm release.

Supported source: ccusage commit
[`e12b7dd9c14494808057df1897d07edc999081eb`](https://github.com/ccusage/ccusage/tree/e12b7dd9c14494808057df1897d07edc999081eb),
release baseline 20.0.26, fork build **20.0.26+tidemux.1**, adapter schema 1.
`upstream.patch` records the minimal workspace, dispatch and lockfile changes;
`adapter/` contains the source crate. Upstream is MIT (`LICENSE.ccusage`);
adapter modifications are covered by TideMux's root Apache-2.0 license. Cargo's pinned
upstream dependencies retain their own licenses; distributing builds requires
retaining the source tree and applicable dependency notices/SBOM.

Build prerequisites: Python 3.12+, Git, Rust **1.97.1** (the pinned upstream
toolchain), a C/C++ build toolchain for upstream SQLite/crypto dependencies,
and network access to the official GitHub archive, crates.io and the upstream
locked LiteLLM pricing snapshot. The TideMux adapter never looks up pricing;
the snapshot build dependency belongs to ccusage's existing client commands.
Gateway requests never invoke ccusage or depend on these tools/network services.

Install into an isolated versioned prefix (no sudo required):

```sh
python3 integrations/ccusage/install.py \
  --prefix "$HOME/.local/ccusage-tidemux-20.0.26.1" \
  --build-dir /tmp/ccusage-tidemux-build
"$HOME/.local/ccusage-tidemux-20.0.26.1/bin/ccusage" tidemux --help
```

The installer verifies the pinned archive SHA256, applies the exact patch,
tests the source adapter, uses Cargo's checked-in lockfile, installs the actual
binary and both project licenses, and emits `share/ccusage-tidemux/BUILD.json`.
That directory also contains the locked runtime/build dependency license list.
It refuses a nonempty build directory or an existing binary target. An existing
archive can be supplied with `--source-archive`; `--profile dev` is for isolated
test builds. `--cargo` can name an isolated Rust toolchain's Cargo executable.
The build tree is retained for license review and source reproducibility.

```sh
ccusage tidemux session --path /absolute/path/to/usage --json
ccusage tidemux daily --path /absolute/path/to/usage --json
ccusage tidemux monthly --path /absolute/path/to/usage --json
ccusage tidemux aggregate --path /absolute/path/to/usage --json
```

This explicit source command reads only `usage-<20-digit-sequence>.jsonl` in the
dedicated directory. The default/client `--all` reports remain unchanged: do not
add their totals to the TideMux report for the same routed requests. Dates use
UTC; session rows with `group: null` contain requests with no persisted client
session identity. Currency (including `null`) partitions every report.

Token sums use checked integers. A partial/unknown column's `total` is null;
`known_subtotal`, `known_requests` and `unknown_requests` show its coverage.
`estimated_cost` and `matched_supplier_amount` are independent columns, never
combined or substituted. Provider-reported tokens do not establish a bill.
The highest revision replaces a request's supplier snapshot; equal duplicates
are ignored, conflicting revisions fail. An incomplete final line is ignored
with an explicit report flag; malformed complete records fail without echoing
the private payload. Reports cover retained records, not the complete ledger.

Package verification:

```sh
python3 scripts/test_ccusage_package.py --binary /path/to/tidemux \
  --ccusage /path/to/installed/bin/ccusage --evidence-dir /tmp/usage-evidence
```

See [the gateway configuration and privacy contract](../../docs/ccusage.md).
