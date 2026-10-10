# Release procedure

Submit independent scopes as separate PRs with behavior, validation and known
limits. Each PR must wait for human approval of its latest commit and explicit
user authorization to merge that PR. New commits require renewed review.
Never auto-merge, use a merge queue or directly push main. Review and merge
rules are in [repository AGENTS](https://github.com/hs3180/tidemux/blob/main/AGENTS.md).
Public publication and production installation require explicit authorization.

## Prepare and build

1. Run `gofmt -l cmd internal`, `CGO_ENABLED=0 go test ./...`,
   `CGO_ENABLED=0 go vet ./...`, and `go test -race ./...`.
2. Run `python3 scripts/check_docs.py`, installer/CLI checks and the applicable
   checks in `.github/workflows/ci.yml`. Update dependency notices through
   `scripts/licenses.py` when needed and review their changes.
3. Align source version, installer default, CHANGELOG and pinned install
   documentation. Commit only reviewed source and lasting manuals.
4. From a clean tree, run `python3 scripts/release.py`. It builds macOS arm64
   with CGO disabled and emits the archive, SHA256SUMS, BUILD, SPDX and formula.
   Docs/licenses are distributed; test scripts/fixtures are excluded.
5. Verify all SHA256SUMS, archive contents, binary version and BUILD source
   commit. Extract into a fresh directory and run all required package gates
   against that exact binary. Record source/archive/binary hashes and results
   in private evidence outside the repository.
6. Check local installation and uninstall data preservation with isolated
   directories/profiles. Finish the installed-client and collector checks below.

`dist/<version>/` is immutable. `--build-id commit` selects a distinct unpublished
same-version candidate and records its source. Existing output directories are
refused. Failed builds leave no partial final directory. The script packages;
CI uploads workflow artifacts. Neither publishes a GitHub release.

## Runtime checks

The workflow lists the required protocol/tool, routing/recovery, reload,
capacity, budget, startup and JSONL package gates. All final acceptance must use
one clean combined source commit and its exact extracted archive.

Install Claude Code, Kilo CLI and Hermes Agent separately, then run:

```sh
python3 scripts/test_client_recovery_package.py --binary /path/to/extracted/tidemux --evidence /path/to/private/evidence
```

All three clients and both gateway/provider capacity scopes are required.
Diagnostic client/scope subsets do not satisfy the full gate. The script uses
synthetic Keychain responses, isolated child profiles and loopback model
fixtures. Record actual client versions, read/edit/shell-test completion,
new-conversation recovery, persistent continuation, wire errors and audits.
These results do not certify model reasoning or paid-provider invoices.

Run the actual official ccusage executable against the package's session logs;
verify known token/cache totals and exclusion of unknown usage. Run actual
Filebeat and Logstash against isolated Elasticsearch with fresh namespaces and
registries; verify timestamps, typed fields, known/unknown usage, replay IDs,
parse-failure retention and mapping DLQ. CI transfers real Darwin package logs
to isolated Linux collectors. Production ES testing is permitted only with
explicit authorization and a dedicated test namespace.

## Publish after approval

After all scoped PRs have human approval and explicit merge authorization,
qualify the resulting main commit again if its source differs from the tested
candidate. Publication permission is the last step after exact assets and
release notes are ready for review.

With explicit authorization, create the annotated version tag and draft release
in `hs3180/tidemux`, attach the exact tested five assets, verify downloaded
checksums and publish. Do not replace a published tag or version's assets.
Validate the version-pinned installer against those uploaded assets. Keep
private vulnerability reporting and the project policy files available.

Submit the generated `Formula/tidemux.rb` to `hs3180/homebrew-tap` as a separate
PR; apply the same human-review and specific merge-authorization rules. Verify
Homebrew installation on macOS arm64, version/doctor/startup, persistence and
uninstall after the tap is available. A production upgrade is a separate action
requiring authorization. Preserve configuration, Keychain and audit data.

If public download/install fails, report the release as unusable and stop
announcements. Correct it with a new qualified version; preserve published
assets and users' data.
