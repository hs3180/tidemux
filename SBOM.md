# Software bill of materials

`scripts/release.py` emits `sbom.spdx.json` (SPDX 2.3) for the actual darwin/arm64
build, including the binary SHA256, source commit, Go runtime and compiled Go
module versions / sums. `BUILD.txt` records the toolchain and Go build metadata.

Original dependency licenses and nested notices are bundled in `licenses/`.
See [third-party notices](THIRD_PARTY_NOTICES.md). Module license conclusions
remain `NOASSERTION` in SPDX where an aggregate legal conclusion is not made;
primary license declarations and full texts are supplied. The SBOM does not
claim that each dependency source file was individually analyzed.

No hosted release is claimed by this document. Local artifacts are generated
from a clean committed tree and never overwritten for the same version.
