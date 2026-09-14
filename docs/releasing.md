# Release procedure

The local 0.1.0 release passed live-provider verification. Public distribution requires the [acceptance conditions](mvp-acceptance.md), including
live validation of both protocols. Candidate artifacts do not imply those gates
passed. Release and tap permissions are required; never replace a published tag
or published version's assets.

## Prepare and build

1. Run `gofmt -l cmd internal`, `CGO_ENABLED=0 go test ./...`,
	`CGO_ENABLED=0 go vet ./...`, and `go test -race ./...`.
2. Run `python3 scripts/licenses.py` and review dependency versions/notices.
3. Update the source `version`, CHANGELOG and acceptance evidence. Commit all
   reviewed files; do not include local credentials, config or databases.
4. Run `python3 scripts/release.py` from a clean Git tree. It builds arm64 with
   CGO disabled, includes build metadata, docs, example config, license texts and
   SPDX JSON, then emits a tarball, SHA256SUMS and matching `tidemux.rb` formula.
5. Verify `shasum -a 256 -c SHA256SUMS` in the output directory, extract into a
   fresh directory, and run the packaged binary's version / doctor / serve /
   ledger checks. Preserve source commit and test results in private evidence.

`dist/<version>/` is immutable. For the unpublished 0.1.0 repair, retain the
application version and run `python3 scripts/release.py --build-id` to create
`dist/0.1.0+<commit-prefix>/` with a distinct archive filename. An explicit
`--build-id repair-N` is also supported. BUILD.txt records both commit and build
ID; compare binary SHA256 rather than version text when installing a same-version
repair. An existing output directory is refused, and failed builds do not publish
a partial final directory. For the final unpublished 0.1.0 freeze, preserve the original tag object under
`archive/v0.1.0-text-only` before assigning `v0.1.0` to the final reviewed commit.
Original asset directories remain unchanged. This local archival step does not
authorize replacing any published tag or asset.

The script packages but never publishes. CI uploads candidate workflow artifacts
only, not GitHub Releases.

## 0.1.1 acceptance additions

Before tagging 0.1.1, use a copy of a 0.1.0 ledger to verify migrations are
additive and `tidemux ledger` remains readable. Exercise a hard-limit request
against a local upstream and verify it receives no second call. Import a
de-identified statement CSV and confirm unmatched lines remain unmatched.
Generate a report, inspect its JSON for the absence of message bodies and
credentials, then test macOS notification delivery and an SMTP test mailbox.
Record delivery failure and retry behavior. Daily balance API checks require an
explicit authorized low-cost account; a balance delta is diagnostic only, never
proof of a particular request charge.

## Live gate

On macOS use [verify_live.py](../scripts/verify_live.py) as described in the demo
for an authorized minimal call per protocol. It requires a running gateway and
verified per-model prices. It checks returned usage, matching request ID, and
cost arithmetic. Retain sanitized evidence outside the public repository; it
must contain no key or full message content. Provider invoice reconciliation
and any claimed savings require separate evidence.

## Publish once all gates pass

The intended source repository is `hs3180/tidemux`, and the personal tap is
`hs3180/homebrew-tap`; verify ownership/visibility before creating them.
An authenticated maintainer must:

1. Push the reviewed commit and its annotated version tag.
2. Enable and verify private vulnerability reporting. Document the community
   contact-request process in the code of conduct. Ensure the policy changes
   are included in the candidate before release.
3. Ensure the tagged source includes `scripts/install.sh`. The README installer
   uses `hs3180/tidemux` and downloads `SHA256SUMS` plus the single arm64 archive
   from that version’s Release; commit-suffixed filenames are supported. Verify
   the installer against the uploaded assets before advertising it as available.
4. Create a draft GitHub Release for the tag with exact tested assets,
   SHA256SUMS, SPDX JSON and BUILD.txt. Review download checksums.
5. Publish the release, copy the generated formula to the tap's
   `Formula/tidemux.rb`, and push the tap commit.
6. On clean macOS arm64, install via the tap, check the version and run doctor,
   serve and an authorized live request. Check persistence and uninstall behavior.
   Only then mark public delivery complete and add verified install instructions.

If download/install fails after publication, flag the release as unusable and
pause announcements. Fix via a new candidate/version; do not silently replace
assets or delete users' data. For later upgrades retain the last verified version
and document database compatibility. Candidate v2 retains v1 legacy tables.
