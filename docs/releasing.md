# Release procedure

Before publishing a release, pass the applicable automated, packaging and
runtime checks. Candidate artifacts do not imply those gates passed. Release
and tap permissions are required; never replace a published tag or published
version's assets.

## Prepare and build

1. Run `gofmt -l cmd internal`, `CGO_ENABLED=0 go test ./...`,
   `CGO_ENABLED=0 go vet ./...`, and `go test -race ./...`.
2. Run `python3 scripts/licenses.py` and review dependency versions/notices.
3. Update the source `version`, CHANGELOG and any affected user documentation.
   Commit all reviewed files; do not include local credentials, config or
   databases.
4. Run `python3 scripts/release.py` from a clean Git tree. It builds arm64 with
   CGO disabled, includes build metadata, docs, license texts and SPDX JSON, then
   emits a tarball, SHA256SUMS and matching `tidemux.rb` formula.
5. Verify `shasum -a 256 -c SHA256SUMS` in the output directory, extract into a
   fresh directory, and run the packaged binary's version / doctor / serve /
   billing checks. Preserve source commit and test results in private evidence.

`dist/<version>/` is immutable. Use a distinct build ID only for an unpublished
same-version candidate; BUILD.txt records its source commit and build ID. An
existing output directory is refused, and failed builds do not publish a partial
final directory. Never rewrite or replace a published tag or asset.

The script packages but never publishes. CI uploads candidate workflow artifacts
only, not GitHub Releases.

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
6. On clean macOS arm64, install via the tap, check the version and run doctor.
   Verify the gateway starts and stops cleanly with a local test configuration,
   then check persistence and uninstall behavior. Only then mark public delivery
   complete and add verified install instructions.

If download/install fails after publication, flag the release as unusable and
pause announcements. Fix via a new candidate/version; do not silently replace
assets or delete users' data. For upgrades, retain a tested rollback path and
document database compatibility before changing persisted formats.
