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
python3 scripts/test_install.py
sh -n scripts/install.sh
```

Formatting output must be empty. Keep core unit tests for protocol/stream/tool
boundaries, admission/routing/reload, accounting/budgets, configuration and
privacy. Prefer focused, deterministic cases over duplicate process-level suites.
The installer unit tests protect failure atomicity and unsupported targets.
For a source installation,
build `./cmd/tidemux` to your chosen binary directory; preserve existing
configuration, Keychain and ledger data.

## Pull requests

Keep each PR focused, explain its behavior, validation and limits, and use the
[pull request template](.github/PULL_REQUEST_TEMPLATE.md). Follow
[AGENTS](AGENTS.md) for human approval of the latest commit and explicit
per-PR merge authorization. New commits need renewed review.

Put user instructions in `docs/guide.md` and architectural contracts in
`docs/design.md`. A behavior rule has one normative home: the guide keeps an
operator summary and a link instead of copying the rule or its bounds. Do not
add the same complete specification to both manuals. Prefer CLI help for option
lists. Merge repeated explanations;
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
   the single offline smoke entry against that exact archive:

   ```sh
   python3 scripts/smoke.py --directory dist/<version>+<commit>
   ```

4. Keep source/archive/binary hashes, tool versions, validation results and any
   known limits outside the repo. The smoke uses temporary state, fake Keychain
   commands and loopback upstreams; it needs macOS arm64, Python 3 and Ruby, and
   does not need installed AI clients, paid providers or collection services.
5. After the scoped PRs are human-approved and explicitly authorized for merge,
   align the release with actual main and requalify if the source changed.
   With separate publication authorization, create the version tag/release,
   upload the five tested assets and verify their downloaded checksums and the
   version-pinned installer. Published assets and tags are immutable.
6. Submit the generated formula to `hs3180/homebrew-tap` as a separate reviewed
   PR. Verify real Homebrew installation after its authorized merge. Production
   installation requires separate authorization and preserves local data.

The smoke verifies manifests/SBOM, packaged manuals, install/reinstall and
data-preserving uninstall, doctor/provider/discovery/connectivity commands,
native and converted protocol requests, streaming completion, single dispatch on
upstream failure, audit identities/usage/unknown cost, JSONL privacy and orderly
shutdown. Its PTY helper checks hidden credential entry and unlock failure or
cancellation without touching a real Keychain. Detailed routing, capacity,
recovery and budget contracts stay in the core unit tests; do not duplicate their
matrices in package scripts. Core observability tests protect ccusage-compatible
records and exclusion of unknown usage; external-client, SDK and collector runs
are optional investigations, not release gates.
Build and CI artifact upload do not publish a release. If public installation
fails, resolve it in a new qualified version before announcing delivery.
