#!/usr/bin/env python3
"""Build and install the pinned real ccusage binary with the TideMux adapter."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import tarfile
from urllib.request import urlopen

COMMIT = "e12b7dd9c14494808057df1897d07edc999081eb"
ARCHIVE_SHA256 = "51e6c3a62ed93c323c0dc9c46f53ebc92e757233a96ceac35c40df2d6cc31502"
VERSION = "20.0.26+tidemux.1"
URL = f"https://codeload.github.com/ccusage/ccusage/tar.gz/{COMMIT}"

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--prefix", required=True, type=Path, help="isolated install directory")
    parser.add_argument("--build-dir", required=True, type=Path, help="new empty build directory")
    parser.add_argument("--source-archive", type=Path, help="optional already downloaded pinned archive")
    parser.add_argument("--cargo", default="cargo")
    parser.add_argument("--profile", choices=("release", "dev"), default="release")
    args = parser.parse_args()
    integration = Path(__file__).resolve().parent
    build = args.build_dir.resolve()
    if build.exists() and any(build.iterdir()):
        parser.error("--build-dir must be new or empty; never overwrite an upstream checkout")
    build.mkdir(parents=True, exist_ok=True)
    archive = build / "upstream.tar.gz"
    if args.source_archive:
        shutil.copyfile(args.source_archive, archive)
    else:
        with urlopen(URL, timeout=60) as response, archive.open("wb") as target:
            shutil.copyfileobj(response, target)
    if hashlib.sha256(archive.read_bytes()).hexdigest() != ARCHIVE_SHA256:
        raise RuntimeError("pinned ccusage archive checksum mismatch")
    with tarfile.open(archive) as source:
        source.extractall(build, filter="data")
    source = build / f"ccusage-{COMMIT}"
    subprocess.run(["git", "apply", "--check", str(integration / "upstream.patch")], cwd=source, check=True)
    subprocess.run(["git", "apply", str(integration / "upstream.patch")], cwd=source, check=True)
    shutil.copytree(integration / "adapter", source / "rust/adapters/tidemux")
    import os
    env = dict(os.environ, CCUSAGE_VERSION=VERSION)
    env.pop("CARGO_TARGET_DIR", None) # Keep this build in its explicit isolated tree.
    cargo = [args.cargo, "--manifest-path", "rust/Cargo.toml"]
    subprocess.run([cargo[0], "test"] + cargo[1:] + ["--locked", "-p", "ccusage-adapter-tidemux", "--features", "ccusage-core/fetch-litellm-pricing"], cwd=source, env=env, check=True)
    command = [cargo[0], "build"] + cargo[1:] + ["--locked", "-p", "ccusage", "--features", "fetch-litellm-pricing"]
    if args.profile == "release": command.append("--release")
    subprocess.run(command, cwd=source, env=env, check=True)
    target = source / "rust/target" / ("release" if args.profile == "release" else "debug") / "ccusage"
    prefix = args.prefix.resolve()
    (prefix / "bin").mkdir(parents=True, exist_ok=True)
    output = prefix / "bin/ccusage"
    if output.exists(): raise RuntimeError("install target already exists; use a new versioned prefix")
    shutil.copy2(target, output)
    licenses = prefix / "share/ccusage-tidemux"
    licenses.mkdir(parents=True, exist_ok=True)
    shutil.copy2(integration / "LICENSE.ccusage", licenses / "LICENSE.ccusage")
    shutil.copy2(integration.parents[1] / "LICENSE", licenses / "LICENSE.tidemux")
    shutil.copy2(integration.parents[1] / "NOTICE", licenses / "NOTICE.tidemux")
    dependencies = subprocess.run([cargo[0], "tree"] + cargo[1:] + ["--locked", "-p", "ccusage", "--edges", "normal,build", "--features", "fetch-litellm-pricing", "--format", "{p} {l}"],
                                  cwd=source, env=env, check=True, capture_output=True, text=True)
    (licenses / "DEPENDENCY-LICENSES.txt").write_text(dependencies.stdout)
    receipt = {"version": VERSION, "upstream_commit": COMMIT, "upstream_url": URL,
               "archive_sha256": ARCHIVE_SHA256, "adapter_patch_sha256": hashlib.sha256((integration / "upstream.patch").read_bytes()).hexdigest(),
               "binary_sha256": hashlib.sha256(output.read_bytes()).hexdigest(), "profile": args.profile,
               "source_directory": str(source), "adapter_schema": 1,
               "adapter_files": {"adapter/" + str(p.relative_to(source / "rust/adapters/tidemux")): hashlib.sha256(p.read_bytes()).hexdigest()
                                 for p in sorted((source / "rust/adapters/tidemux").rglob("*")) if p.is_file()},
               "cargo_lock_sha256": hashlib.sha256((source / "rust/Cargo.lock").read_bytes()).hexdigest()}
    (licenses / "BUILD.json").write_text(json.dumps(receipt, indent=2) + "\n")
    subprocess.run([str(output), "tidemux", "--help"], check=True)
    print(json.dumps(receipt, indent=2))

if __name__ == "__main__": main()
