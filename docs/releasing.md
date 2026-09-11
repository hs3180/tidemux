# Release procedure

The local 0.1.0 release passed live-provider verification. Public distribution requires the [acceptance conditions](mvp-acceptance.md), including
live validation of both protocols. Candidate artifacts do not imply those gates
passed. Release and tap permissions are required; never replace an existing tag
or version's assets.

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
a partial final directory. Original tags and assets remain untouched until the
repair has completed all gates. Never replace already public assets.

The script packages but never publishes. CI uploads candidate workflow artifacts
only, not GitHub Releases.

## Live gate

On macOS use [verify_live.py](../scripts/verify_live.py) as described in the demo
for an authorized minimal call per protocol. It requires a running gateway and
verified per-model prices. It checks returned usage, matching request ID, and
cost arithmetic. Retain sanitized evidence outside the public repository; it
must contain no key or full message content. Provider invoice reconciliation
and any claimed savings require separate evidence.

## Publish once all gates pass

The intended source repository is `hs3180/tidemux`, and the personal tap is
`hs3180/homebrew-tidemux`; verify ownership/visibility before creating them.
An authenticated maintainer must:

1. Push the reviewed commit and its annotated version tag.
2. Enable and verify private vulnerability reporting and a private community
   incident contact. Replace the pre-release placeholders in SECURITY / conduct
   docs and ensure those changes are included in the candidate before release.
3. Create a draft GitHub Release for the tag with exact tested assets,
   SHA256SUMS, SPDX JSON and BUILD.txt. Review download checksums.
4. Publish the release, copy the generated formula to the tap's
   `Formula/tidemux.rb`, and push the tap commit.
5. On clean macOS arm64, install via the tap, check the version and run doctor,
   serve and an authorized live request. Check persistence and uninstall behavior.
   Only then mark public delivery complete and add verified install instructions.

If download/install fails after publication, flag the release as unusable and
pause announcements. Fix via a new candidate/version; do not silently replace
assets or delete users' data. For later upgrades retain the last verified version
and document database compatibility. Candidate v2 retains v1 legacy tables.
