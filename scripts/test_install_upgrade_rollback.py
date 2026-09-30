#!/usr/bin/env python3
"""Test release package installation, upgrade and rollback offline.

Supply the locally built candidate directory and public baseline release
assets. The installer receives both versions from a mock curl command; no
release URL is contacted.
"""
import argparse
import hashlib
import json
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


def run_config_command(binary, *args):
    result = subprocess.run([str(binary), *args], text=True, capture_output=True)
    if result.returncode != 0:
        raise RuntimeError(f"{binary.name} {' '.join(args)} failed: {result.stderr or result.stdout}")
    return result


def check_rollback_config(candidate_binary, baseline_binary, root):
    config = {
        "listen_addr": "127.0.0.1:18765",
        "max_in_flight": 1,
        "ledger_path": str(root / "ledger.db"),
        "access_token_keychain": {"service": "test.gateway", "account": "local"},
        "providers": {
            "provider-a": {
                "protocol": "openai",
                "base_url": "https://api.example.invalid/v1",
                "upstream_id": "provider-a",
                "upstream_keychain": {"service": "test.provider", "account": "provider-a"},
                "supported_models": ["model-one", "model-two"],
            }
        },
    }
    config_path = root / "config.json"
    config_path.write_text(json.dumps(config), encoding="utf-8")
    run_config_command(
        candidate_binary, "auto-chain", "set", "provider-a", "model-one",
        "provider-a", "model-two", "--config", str(config_path),
    )
    run_config_command(candidate_binary, "routing", "set", "--shared-model-strategy", "random", "--billing-exhaustion-failover=true", "--config", str(config_path))
    run_config_command(candidate_binary, "provider", "error-map", "add", "provider-a", "--code", "upstream_busy", "--status", "503", "--category", "temporarily_unavailable", "--config", str(config_path))
    enabled = json.loads(config_path.read_text(encoding="utf-8"))
    expected_auto_chain = [
        {"provider": "provider-a", "model": "model-one"},
        {"provider": "provider-a", "model": "model-two"},
    ]
    if (
        enabled.get("auto_chain") != expected_auto_chain
        or "routing" not in enabled
        or enabled["providers"]["provider-a"].get("error_code_mappings", [{}])[0].get("category") != "temporarily_unavailable"
    ):
        raise RuntimeError("candidate did not persist the optional 0.3.0 routing settings")

    run_config_command(candidate_binary, "auto-chain", "clear", "--config", str(config_path))
    run_config_command(candidate_binary, "routing", "set", "--shared-model-strategy", "off", "--billing-exhaustion-failover=false", "--config", str(config_path))
    run_config_command(candidate_binary, "provider", "error-map", "remove", "provider-a", "--code", "upstream_busy", "--status", "503", "--config", str(config_path))
    rolled_back = json.loads(config_path.read_text(encoding="utf-8"))
    if "routing" in rolled_back or "auto_chain" in rolled_back:
        raise RuntimeError("candidate rollback commands left 0.3.0 routing fields in the config")
    run_config_command(baseline_binary, "provider", "list", "--config", str(config_path))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--candidate-dir", required=True, type=Path, help="local release.py output directory")
    parser.add_argument("--baseline-dir", required=True, type=Path, help="directory containing baseline SHA256SUMS and archive")
    parser.add_argument("--candidate-version", default="0.2.2", help="candidate version (default: 0.2.2)")
    parser.add_argument("--baseline-version", default="0.2.1", help="baseline version (default: 0.2.1)")
    args = parser.parse_args()
    candidate_dir = args.candidate_dir.resolve()
    baseline_dir = args.baseline_dir.resolve()
    candidate = find_archive(candidate_dir, args.candidate_version)
    baseline = find_archive(baseline_dir, args.baseline_version)
    candidate_sha = verify_manifest(candidate_dir, candidate)
    baseline_sha = verify_manifest(baseline_dir, baseline)

    with tempfile.TemporaryDirectory(prefix="tidemux-install-upgrade-rollback.") as temporary:
        root = Path(temporary)
        releases = root / "mock-releases"
        for version, source_dir, archive in ((args.baseline_version, baseline_dir, baseline), (args.candidate_version, candidate_dir, candidate)):
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
        run_installer(args.candidate_version, clean_install, mock_bin, releases, root)
        check_version(clean_install / "tidemux", args.candidate_version)

        upgrade_install = root / "upgrade and rollback"
        upgrade_install.mkdir()
        old_binary = upgrade_install / "tidemux"
        baseline_binary = root / f"tidemux-{args.baseline_version}"
        package_binary(baseline, args.baseline_version, baseline_binary)
        shutil.copyfile(baseline_binary, old_binary)
        old_binary.chmod(0o755)
        check_version(old_binary, args.baseline_version)
        run_installer(args.candidate_version, upgrade_install, mock_bin, releases, root)
        check_version(old_binary, args.candidate_version)
        check_rollback_config(old_binary, baseline_binary, root)
        run_installer(args.baseline_version, upgrade_install, mock_bin, releases, root)
        check_version(old_binary, args.baseline_version)
        run_config_command(old_binary, "provider", "list", "--config", str(root / "config.json"))
        if list(upgrade_install.glob(".tidemux-install.*")) or list(clean_install.glob(".tidemux-install.*")):
            raise RuntimeError("installer left a temporary executable behind")

        print(
            f"{args.candidate_version} clean install, {args.baseline_version} -> {args.candidate_version} upgrade, "
            f"{args.candidate_version} -> {args.baseline_version} rollback, and config rollback passed "
            f"using local mock assets (baseline sha256={baseline_sha}; candidate sha256={candidate_sha})."
        )


if __name__ == "__main__":
    main()
