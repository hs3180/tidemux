# TideMux 0.2.1 release plan

This release addresses the highest-priority provider-routing failures while
keeping the named-provider boundary introduced in 0.2.0.

## Issue scope

| Priority | Issue | Release outcome |
| --- | --- | --- |
| P0 | [#31](https://github.com/hs3180/tidemux/issues/31) | Map exact provider error codes to safe, stable categories; return the right OpenAI or Anthropic error envelope without exposing provider messages. |
| P0 | [#47](https://github.com/hs3180/tidemux/issues/47) | Configure mappings per provider and honor bounded same-key `Retry-After` waits/retries before any same-provider key failover. |
| P0 | [#51](https://github.com/hs3180/tidemux/issues/51) | Identify protocols from authenticated model-list response shapes; retain explicit overrides and fail clearly on ambiguous/unknown schemas. |
| P0 | [#54](https://github.com/hs3180/tidemux/issues/54) | Preserve documented Anthropic Messages content blocks and citations on native Anthropic routes; return a precise field path when a block cannot be translated to OpenAI. |
| P1 | [#38](https://github.com/hs3180/tidemux/issues/38) | Document repeatable install and setup workflows for Claude Code, Codex, Hermes and dsh, including local credential handling and client compatibility limits. |

Defer #52, #53, #32, #40, #41, #37, #16, #17 and #8. Remove #6 from the old 0.2.0
milestone and defer it to its own future plan. These items are outside the
0.2.1 acceptance gate.

## Compatibility invariants

- Keep every provider's endpoint and upstream protocol in one named profile.
- Keep model selection explicit as `REF/MODEL`; never fall back to another
  named provider.
- Preserve existing provider profiles, key groups and Keychain references,
  model scopes, settings, budgets and audit history.
- Treat provider messages as untrusted. Never infer classifications from free
  text or return raw upstream messages.
- Retry only within the selected profile's key group, with a bounded deadline;
  never replay after response content may have reached the client.
- Do not claim direct Codex routing; TideMux does not implement Responses API.

## Release gates and order

1. Confirm the release base is the published v0.2.0 commit, map every open
   issue and pull request to this scope or an explicit deferral, and move #6
   off the 0.2.0 milestone.
2. Complete error mapping, rate-limit retries, schema-based protocol detection
   and native Anthropic content preservation. Cover both client protocols and
   both upstream protocols, safe mapped stream errors, cancellation and
   field-specific cross-protocol errors.
3. Complete the #38 guide and copyable prompt. Check fresh install, upgrade and
   repeated setup paths; document direct dsh, Claude and Hermes configuration
   and Codex's routing limit.
4. Run formatting, CGO-disabled tests, vet, race tests, provider-add PTY,
   installer and client-command checks, release packaging and license checks.
5. Review the final binary and arm64 archive, checksum, Homebrew formula and
   local install/rollback behavior. Verify installed Claude Code, Kilo and
   Hermes workflows with authorized real upstream accounts when available.
6. Merge the reviewed release change, publish v0.2.1 and update the Homebrew
   tap only after the tagged artifact checksum is public. Verify download,
   checksum and a clean macOS arm64 install from the published release.
7. Update the release status with test evidence and final issue/PR disposition.
   Do not call the release complete until the public install verification and
   issue mapping are recorded.
