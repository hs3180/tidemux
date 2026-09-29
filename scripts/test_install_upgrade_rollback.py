#!/usr/bin/env python3
"""Test real 0.2.2 package installation, upgrade and rollback offline.

Supply the locally built candidate directory and downloaded public v0.2.1
release assets. The installer receives both versions from a mock curl command;
no release URL is contacted.
"""
import argparse
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]
INSTALLER = ROOT / "scripts" / "install.sh"


def find_archive(directory, version):
    archives = sorted(directory.glob(f"tidemux_{version}*_darwin_arm64.tar.gz"))
    if len(archives) != 1:
        raise RuntimeError(f"expected exactly one {version} arm64 archive in {directory}, found {len(archives)}")
    return archives[0]


def verify_manifest(directory, archive):
    manifest = directory / "SHA256SUMS"
    if not manifest.is_file():
        raise RuntimeError(f"missing SHA256SUMS in {directory}")
    matches = [line.split() for line in manifest.read_text(encoding="utf-8").splitlines()
               if len(line.split()) == 2 and line.split()[1] == archive.name]
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    if len(matches) != 1 or matches[0][0] != digest:
        raise RuntimeError(f"archive checksum does not match {manifest.name}: {archive.name}")
    return digest


def package_binary(archive, version, destination):
    expected = f"tidemux-{version}/tidemux"
    with tarfile.open(archive, "r:gz") as bundle:
        try:
            content = bundle.extractfile(expected).read()
        except (KeyError, AttributeError) as error:
            raise RuntimeError(f"{archive.name} does not contain {expected}") from error
    destination.write_bytes(content)
    destination.chmod(0o755)


def run_installer(version, install_dir, mock_bin, mock_releases, root):
    home = root / f"home-{version}-{install_dir.name}"
    home.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ)
    env.update({
        "PATH": str(mock_bin) + ":/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin",
        "HOME": str(home),
        "TIDEMUX_INSTALL_DIR": str(install_dir),
        "TIDEMUX_VERSION": version,
        "TIDEMUX_MOCK_RELEASES": str(mock_releases),
        "LANG": "C.UTF-8",
    })
    result = subprocess.run(["sh", str(INSTALLER)], cwd=root, env=env, text=True, capture_output=True)
    if result.returncode != 0:
        raise RuntimeError(f"install.sh {version} failed with {result.returncode}: {result.stderr or result.stdout}")
    return result


def check_version(binary, expected):
    result = subprocess.run([str(binary), "version"], text=True, capture_output=True, check=True)
    actual = result.stdout.strip()
    if actual != expected:
        raise RuntimeError(f"installed binary reports {actual!r}; expected {expected}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--candidate-dir", required=True, type=Path, help="local release.py output directory for 0.2.2")
    parser.add_argument("--baseline-dir", required=True, type=Path, help="directory containing public v0.2.1 SHA256SUMS and archive")
    args = parser.parse_args()
    candidate_dir = args.candidate_dir.resolve()
    baseline_dir = args.baseline_dir.resolve()
    candidate = find_archive(candidate_dir, "0.2.2")
    baseline = find_archive(baseline_dir, "0.2.1")
    candidate_sha = verify_manifest(candidate_dir, candidate)
    baseline_sha = verify_manifest(baseline_dir, baseline)

    with tempfile.TemporaryDirectory(prefix="tidemux-install-upgrade-rollback.") as temporary:
        root = Path(temporary)
        releases = root / "mock-releases"
        for version, source_dir, archive in (("0.2.1", baseline_dir, baseline), ("0.2.2", candidate_dir, candidate)):
            version_dir = releases / version
            version_dir.mkdir(parents=True)
            shutil.copyfile(source_dir / "SHA256SUMS", version_dir / "SHA256SUMS")
            shutil.copyfile(archive, version_dir / archive.name)

        mock_bin = root / "mock-bin"
        mock_bin.mkdir()
        curl = mock_bin / "curl"
        curl.write_text(
            "#!/bin/sh\n"
            "url= dest=\n"
            "while [ \"$#\" -gt 0 ]; do\n"
            "  case \"$1\" in -o) shift; dest=$1 ;; https://*) url=$1 ;; esac\n"
            "  shift\n"
            "done\n"
            "version=$(printf '%s' \"$url\" | sed -n 's#.*releases/download/v\\([^/]*\\)/.*#\\1#p')\n"
            "[ -n \"$version\" ] && [ -n \"$dest\" ] || exit 2\n"
            "cp \"$TIDEMUX_MOCK_RELEASES/$version/${url##*/}\" \"$dest\"\n",
            encoding="utf-8",
        )
        curl.chmod(0o755)

        clean_install = root / "clean install"
        clean_install.mkdir()
        run_installer("0.2.2", clean_install, mock_bin, releases, root)
        check_version(clean_install / "tidemux", "0.2.2")

        upgrade_install = root / "upgrade and rollback"
        upgrade_install.mkdir()
        old_binary = upgrade_install / "tidemux"
        package_binary(baseline, "0.2.1", old_binary)
        check_version(old_binary, "0.2.1")
        run_installer("0.2.2", upgrade_install, mock_bin, releases, root)
        check_version(old_binary, "0.2.2")
        run_installer("0.2.1", upgrade_install, mock_bin, releases, root)
        check_version(old_binary, "0.2.1")
        if list(upgrade_install.glob(".tidemux-install.*")) or list(clean_install.glob(".tidemux-install.*")):
            raise RuntimeError("installer left a temporary executable behind")

        print(
            "0.2.2 clean install, 0.2.1 -> 0.2.2 upgrade, and 0.2.2 -> 0.2.1 rollback passed "
            f"using local mock assets (v0.2.1 sha256={baseline_sha}; v0.2.2 sha256={candidate_sha})."
        )


if __name__ == "__main__":
    main()
