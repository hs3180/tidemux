# Software bill of materials

No release SBOM has been generated or published yet. The source dependency
versions are pinned in [go.mod](go.mod) and [go.sum](go.sum).

Before publishing a binary, generate an SPDX JSON SBOM from the actual build
and attach it with the release. Record the source commit, exact Go toolchain,
target platform, module versions, licenses, and checksums. Verify that the SBOM
matches the distributed asset and include the required license texts.

This document is a release requirement, not evidence of a generated SBOM,
hermetic build, or reproducible-build guarantee. See
[third-party notices](THIRD_PARTY_NOTICES.md) for the current declaration status.
