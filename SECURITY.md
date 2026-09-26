# Security policy

## Reporting

Report suspected vulnerabilities privately through
[GitHub private vulnerability reporting](https://github.com/hs3180/tidemux/security/advisories/new).
Include the affected version, steps to reproduce and expected impact. Do not
include live credentials or private conversation data. Do not disclose
vulnerability details in public issues.

## Supported versions

Security fixes target the latest published release. Version 0.2.0 supports
macOS 15+ on Apple Silicon. No response-time guarantee is provided.

## Current boundary

- The gateway binds a loopback IP and requires a local Bearer token by default.
  Setting `listen_addr` to `0.0.0.0` enables all-interface LAN access and emits a
  warning. Enabling this mode with `tidemux gateway configure` requires a
  separate Gateway API key. Supply one or press Enter to generate it randomly.
  Traffic is plain HTTP and must be protected by a trusted network, firewall,
  and preferably a TLS-terminating reverse proxy.
- Provider and gateway credentials are read from macOS Keychain references.
- Configuration does not serialize runtime credentials. Errors avoid exposing
  upstream response bodies; full prompts and responses are not persisted.
- Requests are sent to the configured upstream. No telemetry or diagnostic
  upload feature is implemented.

See [protocol support](docs/protocols.md) for current gateway boundaries and
[client compatibility](docs/client-compatibility.md) for the tested 0.1.1
client matrix.

Security reports should cover the CLI, HTTP service, ledger, credentials,
and bundled dependencies. Public release materials must identify affected
versions, the reporting channel, and supported release versions.
