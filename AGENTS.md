# Working in TideMux

These rules apply to the repository and its worktrees, alongside workspace rules.

## Review and authorization

- Submit independent scopes as separate PRs with behavior, validation and limits.
- Merge only after a human approves the latest commit and the user explicitly
  authorizes that PR's merge. New commits require renewed human review.
- Tests, CI, bots and other agents do not replace human approval. Never auto-merge,
  queue a merge, directly push main or bypass review through another interface.
- General completion requests do not authorize merges or dependent release steps.
  Public publication and production upgrades require explicit authorization.
  Continue authorized independent work while awaiting review.

## Source and documentation

- CLI/startup code lives in `cmd/tidemux`; runtime stages are in `internal/`.
  Read [core design](docs/design.md) and the affected code before changing contracts.
- Keep only two lasting manuals: [user guide](docs/guide.md) and core design.
  Add explanations to the existing task/contract section; remove duplication.
  README is the quick start, CLI help is the option reference, and
  [CONTRIBUTING](CONTRIBUTING.md) owns development/release procedures.
- Keep synthetic inputs in `scripts/fixtures/`. Tests/fixtures and execution
  records must not enter release packages. Evidence, progress, review notes and
  historical transactions belong in issue/PR attachments or a private workspace.
- Generate immutable `dist/` assets from a clean commit with `scripts/release.py`.
  Never patch generated assets or replace a published tag.

## Contracts and verification

Preserve admitted request views, session/capacity/budget semantics, reload epoch
and connection-generation fences. Recheck health after queuing; never replay
output that may have reached a client. Credentials stay in Keychain. Exclude
secrets, bodies, raw sessions, private paths and provider errors from diagnostics.
Keep unknown usage/cost and currencies distinct; counters never replace SQLite.

Run focused meaningful tests, required Go test/vet/race and affected package gates.
Run `python3 scripts/check_docs.py` after Markdown edits. Final delivery uses one
clean extracted archive with source/binary/archive hashes and actual tool versions;
follow [release checks](CONTRIBUTING.md#releases). Update generated notices via
`scripts/licenses.py`; preserve privacy, security, licenses and SBOM documents.
