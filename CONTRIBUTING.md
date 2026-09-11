# Contributing to TideMux

Thanks for considering contributing. TideMux is built in the open, Local
first, transparent, and explainable - please read the project's documented scope
and privacy boundaries (README) and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) first.

## What we look for

- Bug reports and reproducible issues (over diagnoses without repro).
- Focused improvements that keep the tool **simple by design** - we prefer
  solving a real, proven need over speculative features.
- Respect for the privacy / Local-first boundary (no telemetry, no silent
  collection).

## Get started

1. **Check open issues first** - comment if you plan to work on an existing
   one to avoid duplicate effort.
2. **Fork & branch** - work on a descriptive branch (e.g. `fix/limiter-race`).
3. **Keep PRs small and single-concern**:
   - One issue / one change per PR.
   - Split large changes; each PR must be independently reviewable.
4. **Run the checks** before submitting:
   - `go build ./...` and `go test ./...`
   - `go vet ./...` and `gofmt -l .` (must be clean)
   - Add/update tests for behavior you change.

## Pull request template

Use [PULL_REQUEST_TEMPLATE.md](.github/PULL_REQUEST_TEMPLATE.md) - it asks for
what reviewers need: the context, the change, how it was verified, and any
trade-offs.

## Commit & PR etiquette

- Write clear commit messages (why, not just what).
- Address each review comment individually; if you disagree, explain why
  rather than ignoring it.
- Be patient and kind - healthy collaboration is the goal.

## Not on the table

Per the project guardrails, TideMux does **not** pursue monetization, scale
tactics that trade user trust, cloud control planes / multi-tenancy, or
anything that breaks the documented privacy boundaries. Contributions should respect those
boundaries.