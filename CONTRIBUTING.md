# Contributing to TideMux

Thank you for contributing to TideMux. Read the [project overview](README.md),
privacy boundaries and [code of conduct](CODE_OF_CONDUCT.md) before you start.

## Language and conventions

English is the primary language for documentation, CLI output, code comments,
tests, issue templates and pull requests. Write for an international audience:
use plain English, explicit units and unambiguous dates such as `2026-09-12`.
Store timestamps as Unix time and state the time zone when displaying dates.

Keep providers and currencies configurable. Do not infer a user's region,
currency or time zone from their provider or model. DeepSeek is a quick-start
example, not a required provider. Preserve third-party names, license texts and
historical database identifiers when needed for attribution or compatibility.

## What we look for

- Bug reports with clear steps to reproduce the problem.
- Focused improvements that keep the tool **simple by design** - we prefer
  solving a real, proven need over speculative features.
- Changes that respect user privacy, with no telemetry or undisclosed data collection.

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

## Scope

Keep contributions within the [documented product scope](README.md#compatibility).
Cloud control planes, multi-tenancy and features that collect user data are
outside the current release scope. Discuss substantial changes before opening
a large pull request.
