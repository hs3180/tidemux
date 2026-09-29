# TideMux 0.2.2 release plan

The public `v0.2.1` tag and assets are immutable. This release completes the
expanded 0.2.1 delivery goal and ships its post-tag changes as a new immutable
patch version; it does not replace the published 0.2.1 package.

## Included delivery gates

- **#52 — bare model IDs:** route a bare upstream ID when exactly one configured
  provider scope can serve it. If scopes overlap, return `model_ambiguous`
  without sending the request upstream. Preserve explicit `REF/MODEL`,
  namespaced upstream IDs where their scope is known, and cross-protocol
  conversion. Client protocol does not choose the provider.
- **#57 and #59 — budget recovery/accounting:** include the provider budget reset
  command, safe diagnostics, and the known-cost settlement fix for audit append
  failures. Verify both features in the packaged binary; both fixes merged after
  the 0.2.1 tag.
- **#61 — provider addition:** `provider add` must say that an already-running
  gateway needs a restart before the new provider is available. Document the
  same lifecycle behavior.
- **#62 — protocol auto cold start:** a provider whose authenticated `/models`
  probe is inconclusive must not prevent other resolved providers from starting.
  The unresolved provider is omitted from model listings, explicit requests
  return `provider_unavailable`, and logs give a provider-scoped explicit
  protocol command. If no provider resolves, startup fails with actionable
  diagnostics. Detection remains bounded, non-billable, and behavior-based.
- **#53 — Claude Code context management:** Anthropic `context_management`
  cannot be represented by OpenAI Chat Completions. Return the stable
  `unsupported_request_feature` code and `context_management` parameter path,
  with a field-specific message that Claude Code displays; document native
  Anthropic routing and client-side disablement where supported. Keep native
  Anthropic forwarding unchanged.
- Retain and verify the already released **#31, #47, #51, and #54** behavior.
- Complete **#38** install/config acceptance for Claude Code, Codex, Hermes and
  dsh; document the supported operation and exact evidence for each.

## Acceptance and release checks

- Verify model routing for bare and qualified IDs across both client protocols,
  unique and ambiguous scopes, model allowlists, and slash-containing upstream
  IDs. Confirm ambiguous requests never reach an upstream. Verify the Claude,
  Kilo and Hermes launchers accept bare IDs and select model capabilities from
  the same unique provider scope.
- Verify a resolved Anthropic `auto` probe starts; an inconclusive provider is
  isolated while other providers serve; all-inconclusive configuration fails
  with each provider reference and the explicit override command.
- Verify provider addition with a running-gateway fixture produces the restart
  instruction and that a restart makes the provider discoverable and routable.
- Verify audit-append failure preserves known cost, unknown cost still fails
  closed, and the budget reset command changes only the selected provider and
  rolling window.
- Use Claude Code with a local mock to verify native Anthropic context
  management passes through, while the OpenAI-compatible route returns the
  documented error and precise field path without sending the request upstream.
- Run `gofmt`, `CGO_ENABLED=0 go test ./...`, `CGO_ENABLED=0 go vet ./...`,
  `go test -race ./...`, provider-add PTY, shell syntax, installer, client
  command and license checks.
- Build from the reviewed commit with `scripts/release.py`; verify SHA256,
  SBOM, BUILD.txt and the generated Homebrew formula. Test the packaged binary,
  clean macOS arm64 install, 0.2.1-to-0.2.2 upgrade and rollback.
- Use only existing user-authorized production credentials for limited live
  client checks. Do not read or print provider secrets or billing details.

Publish only after every gate passes: merge reviewed source, create immutable
`v0.2.2`, upload the tested archive, `SHA256SUMS`, SPDX SBOM and `BUILD.txt`,
update the Homebrew tap from the generated formula, then verify public download
and installation. If any gate fails, keep the release goal open and do not
publish or announce availability.
