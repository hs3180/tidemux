# Working in TideMux

These instructions apply to this repository and its worktrees. Follow any
workspace-wide instructions as well.

## Human review and merge authorization

- Submit independent changes as separate PRs, with necessary validation,
  concrete behavior changes and known limits.
- Wait for a human to review and approve the latest PR commit. Merge only after
  the user explicitly authorizes merging that specific PR.
- Tests, CI, conflict-free status, bots and other agents do not replace human
  review or authorize a merge. New commits require another human review.
- Do not enable auto-merge, use a merge queue, directly push the main branch or
  bypass this requirement through another API or CLI.
- General requests to finish work or deliver a version do not authorize PR
  merges or release steps that depend on them. Continue authorized independent
  work while waiting. Public releases and production upgrades need their own
  explicit authorization.

## File ownership

- `cmd/tidemux`: CLI, secure setup, launchers and process startup.
- `internal/gateway`: immutable request views, routing, provider/key availability,
  capacity, reservations and config publication.
- `internal/adapter`: upstream HTTP attempts, native forwarding, conversion,
  streaming limits and logical-call audit settlement.
- `internal/ledger`, `internal/observability`, `internal/limiter`: accounting,
  canonical JSONL records and concurrency/session contracts.
- `docs/`: lasting user manuals, core design and reusable developer procedures.
  Keep README concise and link to the relevant manual.
- `scripts/fixtures/`: synthetic test inputs, including collector configuration.
  Fixtures and tests must stay out of distributed archives and the formula.
- `dist/`: immutable generated candidates. Generate from a clean commit using
  `scripts/release.py`; never patch generated assets or replace a published tag.
- Execution logs, test evidence, review notes, task progress and historical
  release transactions belong in issue/PR attachments or a private workspace
  outside this repository. Do not add them to manuals or release archives.

## Preserve contracts

Read [core design](docs/design.md) and the affected user manual before changing
behavior. Requests admitted before a reload keep their credential, policy,
price, reservation and audit snapshot. Fence old availability/probe results by
publication epoch and connection generation. Recheck health after the queue,
before dispatch. Never replay output that may have reached a streaming client.

Gateway credentials, provider credentials and webhook secrets stay in Keychain.
Do not print secrets, original request/response content, raw session IDs,
private paths or raw provider errors in shareable diagnostics. Preserve unknown
usage/cost and currency separation. Operational counters do not replace SQLite
accounting or produce additional assistant usage records.

## Verification

Use focused meaningful tests for changed behavior, then the required Go
test/vet/race checks and affected package gates. Keep Go formatting clean.
Run `python3 scripts/check_docs.py` after Markdown changes. For final delivery,
use one exact clean extracted archive, record its source and binary/archive
hashes, and follow [release procedure](docs/releasing.md). Native collectors and
installed-client checks must record their actual versions and limitations.

Change dependency notices through `scripts/licenses.py`; review generated
licenses/SPDX metadata. Preserve privacy, security, license and SBOM documents.
