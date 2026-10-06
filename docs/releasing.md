# Release procedure

Before publishing a release, pass the applicable automated, packaging and
runtime checks. Candidate artifacts do not imply those gates passed. Release
and tap permissions are required; never replace a published tag or published
version's assets.

The current 0.3.2 work is an unpublished candidate based on public v0.3.1.
Use the [0.3.2 gate list](release-0.3.2.md) and
[upgrade compatibility contract](upgrade-compatibility.md). Independent PRs
require human review of their latest commits and explicit merge authorization.
Passing CI, agent review, a draft PR or a goal completion request cannot authorize
merging, an automatic merge queue, public release or production replacement.

## Prepare and build

1. Require empty `gofmt -l cmd internal` output. Run
   `CGO_ENABLED=0 go test -count=1 ./...`, `CGO_ENABLED=0 go vet ./...`,
   and `go test -race -count=1 ./...` with the `go.mod` toolchain; record its
   exact version and platform.
2. Run `python3 scripts/test_provider_add_pty.py`, `sh -n scripts/install.sh`,
   `python3 scripts/test_install.py`, `python3 scripts/test_client_commands.py`,
   `python3 scripts/test_prepare_rollback.py` and `python3 scripts/licenses.py`.
   Review dependency versions/notices and the optional pinned consumer's own
   build, tests, source/patch/lockfile provenance and license inventory.
3. Update the source `version`, CHANGELOG and any affected user documentation.
   Commit all reviewed files; do not include local credentials, config or
   databases.
4. Run `python3 scripts/release.py --build-id commit` from a clean Git tree for
   an unpublished candidate (omit the suffix only for the authorized final
   asset name). It builds arm64 with
   CGO disabled and includes build metadata, docs, licenses, SPDX JSON and
   collector reference examples. It emits a tarball, SHA256SUMS and matching `tidemux.rb` formula.
5. Verify `shasum -a 256 -c SHA256SUMS` in the output directory and extract once
   into a fresh directory. Run that same binary's complete applicable package,
   65-second real-clock stream, current real-client, actual consumer, isolated
   collector and public-baseline upgrade/rollback gates. Check version / doctor /
   serve / billing as well. Test harnesses run from the matching source checkout;
   the archive includes `tools/prepare_rollback.py` for operator rollback.
   Preserve source commit, binary/archive hashes, commands/exits, client/model/
   consumer versions and evidence paths. Mark missing or failed gates explicitly.

For 0.3.2, the public 0.3.1 → exact combined candidate → public 0.3.1 gate must
verify configuration conversion, retained Keychain references, consistent latest
SQLite backups and old-version reading of the new optional audit metadata.
Disabling a new field's value is insufficient for the older strict decoder;
remove `usage_log` and every provider-level `max_active_sessions`. Preserve the
gateway-wide limit, original config, latest ledger and private export state.
Never erase user data or substitute an older ledger that hides newer history.

Required 0.3.2 scopes are #115, #79, #116, #100, #117 and current documentation/
client acceptance. #102 is independent; do not add every possible logging
enhancement as a release gate. If merging, conflict resolution, version changes
or later fixes change the source, build a new immutable combined candidate and
repeat final acceptance. Earlier independent-branch passes remain development
evidence and cannot certify the final release archive.

`dist/<version>/` is immutable. Use a distinct build ID only for an unpublished
same-version candidate; BUILD.txt records its source commit and build ID. An
existing output directory is refused, and failed builds do not publish a partial
final directory. Never rewrite or replace a published tag or asset.

The script packages but never publishes. CI uploads candidate workflow artifacts
only, not GitHub Releases.

## Publish once all gates pass

The source repository is `hs3180/tidemux`, and the tap is
`hs3180/homebrew-tap`; verify the target and authorization before writes.
An authenticated maintainer must:

1. After the latest commits have human review and explicit merge authorization,
   confirm the merged release source matches the accepted build. Obtain any
   remaining public-release authorization before creating/pushing its annotated
   version tag. Do not bypass review through a direct main push or merge queue.
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

Record whether this is a new physical Mac or an isolated profile/prefix on the
same Mac; a new profile does not establish second-machine compatibility.
Keep candidate-passed, awaiting-review, merged, published and production-
verified states separate. Production upgrade requires its own authorization,
consistent binary/config/plist/consumer/ledger backup and actual service/GUI
Keychain, capacity/export, history and collector checks. Preserve Filebeat
registry and private usage identity/checkpoint files.

If download/install fails after publication, flag the release as unusable and
pause announcements. Fix via a new candidate/version; do not silently replace
assets or delete users' data. For upgrades, retain a tested rollback path and
document database compatibility before changing persisted formats.

## 0.3.1 gates

Use `docs/release-0.3.1.md` for the required reliability and collector gates.
Run each against the same clean, extracted archive that will be uploaded.
The ES destination must be an explicitly supplied isolated fixture; CI's
normal packaged checks do not contact a production Elasticsearch deployment.
Back up config and ledger with SQLite's online backup API before local upgrade;
verify table schema and historical audit rows after upgrade. Stop the old
process before replacing its executable, retain the old binary as a rollback
path, and verify the actual launchd service plus authenticated gateway access.
