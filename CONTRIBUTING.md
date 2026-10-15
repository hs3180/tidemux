# Contributing

Read the [user guide](docs/guide.md), [core design](docs/design.md) and
[code of conduct](CODE_OF_CONDUCT.md). Use plain English for docs, CLI output,
comments and PRs. Keep providers/currencies configurable and preserve unknown
usage, privacy and third-party attribution.

## Development

Requires Go 1.27+ and macOS arm64 for native package checks.

```sh
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go vet ./...
go test -race ./...
gofmt -l cmd internal
```

Formatting output must be empty. Use focused tests for changed behavior and the
affected package gates in `.github/workflows/ci.yml`. For a source installation,
build `./cmd/tidemux` to your chosen binary directory; preserve existing
configuration, Keychain and ledger data.

## Pull requests

Keep each PR focused, explain its behavior, validation and limits, and use the
[pull request template](.github/PULL_REQUEST_TEMPLATE.md). Follow
[AGENTS](AGENTS.md) for human approval of the latest commit and explicit
per-PR merge authorization. New commits need renewed review.

Put user instructions in `docs/guide.md` and architectural contracts in
`docs/design.md`. Prefer CLI help for option lists. Merge repeated explanations;
keep task progress, review notes, test evidence and release transactions in
issue/PR attachments or a private workspace outside the source tree.
Update dependency notices through `scripts/licenses.py`; preserve license,
privacy, security and SPDX/SBOM material.

## Releases

1. Align the source version, installer default, changelog and version-pinned
   installation example. Run the development checks and required CI gates.
2. From a clean committed tree, run `python3 scripts/release.py --build-id commit`.
   It creates a macOS arm64 archive, SHA256SUMS, BUILD, SPDX and formula in an
   immutable `dist/<version>+<commit>/` directory. Scripts/fixtures do not ship.
3. Verify every checksum, binary version, BUILD source commit, SBOM and archive
   contents. Extract into a fresh directory; check its Markdown links and run
   every required package gate against that exact binary.
4. Finish installed-client, official ccusage and actual collector checks below.
   Verify installation/reinstallation and data-preserving uninstall in isolated
   directories. Keep source/archive/binary hashes and results outside the repo.
5. After the scoped PRs are human-approved and explicitly authorized for merge,
   align the release with actual main and requalify if the source changed.
   With separate publication authorization, create the version tag/release,
   upload the five tested assets and verify their downloaded checksums and the
   version-pinned installer. Published assets and tags are immutable.
6. Submit the generated formula to `hs3180/homebrew-tap` as a separate reviewed
   PR. Verify real Homebrew installation after its authorized merge. Production
   installation requires separate authorization and preserves local data.

The required package gates cover protocol/tools, routing/recovery, reload,
capacity, budgets, startup and JSONL. For actual installed Claude/Kilo/Hermes:

```sh
python3 scripts/test_client_recovery_package.py --binary /path/to/extracted/tidemux --evidence /path/to/private/evidence
```

Require all three clients and both capacity scopes. Record their actual versions,
read/edit/shell-test, recovery and continuation. Loopback scripted models verify
client/gateway behavior, not reasoning or paid invoices; interactive TUIs and
complete dsh/Codex workflows are outside acceptance.

Also require all three installed clients under both shared-model strategies:

```sh
python3 scripts/test_shared_model_launch_package.py --binary /path/to/extracted/tidemux --installed-clients --evidence /path/to/private/shared-launch-evidence
```

The fixture supplies a stable session header; it does not certify that each
client generates that header itself. Verify ambiguity, binding, recovery and
actual upstream/key attempt counts.

Run the unmodified official ccusage executable on the same package's session
logs and verify known counts and exclusion of unknown usage. Run actual
Filebeat/Logstash against isolated ES with fresh namespaces and registries;
check types/time/usage, replay IDs, parse failures and mapping DLQ. Production
ES tests require explicit authorization and a dedicated namespace.
The Docker Logstash gate gives image download a separate five-minute bound;
each config validation retains its 90-second bound. Partial command output and
cleanup diagnostics remain in the supplied evidence directory on failures.
Build and CI artifact upload do not publish a release. If public installation
fails, resolve it in a new qualified version before announcing delivery.
