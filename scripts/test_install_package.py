#!/usr/bin/env python3
"""Install/reinstall one actual candidate archive and check data preservation.

Only release downloads are served from local assets; the macOS installer and
binary are real. Uses a temporary HOME/destination and no production Keychain.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import tarfile
import tempfile


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--version", default="0.3.3")
    args = parser.parse_args()
    assets = args.directory.resolve()
    archives = list(assets.glob("*.tar.gz"))
    if len(archives) != 1:
        raise RuntimeError("Expected one immutable candidate archive")
    archive = archives[0]
    for line in (assets / "SHA256SUMS").read_text().splitlines():
        expected, name = line.split()
        if Path(name).name != name or digest(assets / name) != expected:
            raise RuntimeError("Release manifest mismatch")
    with tarfile.open(archive) as bundle:
        packaged = bundle.extractfile(f"tidemux-{args.version}/tidemux").read()
    binary_hash = hashlib.sha256(packaged).hexdigest()
    formula = (assets / "tidemux.rb").read_text()
    if archive.name not in formula or digest(archive) not in formula or f"v{args.version}/" not in formula:
        raise RuntimeError("Formula and verified archive disagree")
    subprocess.run(["ruby", "-c", str(assets / "tidemux.rb")], check=True, capture_output=True)
    installer = Path(__file__).with_name("install.sh").resolve()
    with tempfile.TemporaryDirectory(prefix="tidemux-install-package.") as temporary:
        root = Path(temporary)
        home = root / "home"
        home.mkdir()
        data = home / "Library" / "Application Support" / "TideMux"
        data.mkdir(parents=True)
        config = data / "config.json"
        config.write_text(json.dumps({"listen_addr": "127.0.0.1:4000", "max_in_flight": 1,
                                     "ledger_path": str(data / "ledger.db"),
                                     "access_token_keychain": {"service": "test.gateway", "account": "preserved"},
                                     "providers": {"a": {"protocol": "openai", "base_url": "https://provider.invalid/v1",
                                                         "upstream_keychain": {"service": "test.provider", "account": "preserved"}}}}))
        with sqlite3.connect(data / "ledger.db") as db:
            db.execute("CREATE TABLE preserved_history (value TEXT)")
            db.execute("INSERT INTO preserved_history VALUES ('existing-history')")
        for name in ("logs/session.jsonl", "statements/statement.csv", "reports/report.html", "keychain-fixture.json"):
            path = data / name
            path.parent.mkdir(exist_ok=True)
            path.write_text("preserved-private-state")
        original = {str(p.relative_to(data)): digest(p) for p in data.rglob("*") if p.is_file()}
        mock = root / "mock"
        mock.mkdir()
        curl = mock / "curl"
        curl.write_text('''#!/bin/sh
url= dest=
while [ "$#" -gt 0 ]; do
  case "$1" in -o) shift; dest=$1 ;; https://*) url=$1 ;; esac
  shift
done
case "$url" in "https://github.com/hs3180/tidemux/releases/download/v$TIDEMUX_VERSION/"*) ;; *) exit 2;; esac
cp "$TIDEMUX_LOCAL_ASSETS/${url##*/}" "$dest"
''')
        curl.chmod(0o755)
        security = mock / "security"
        security.write_text('#!/bin/sh\nprintf invoked >> "$TIDEMUX_SECURITY_MARKER"\ncase "$*" in *test.gateway*) echo fixture-gateway-secret;; *test.provider*) echo fixture-provider-secret;; *) exit 1;; esac\n')
        security.chmod(0o755)
        destination = root / "install with spaces"
        env = dict(os.environ, HOME=str(home), PATH=str(mock) + ":" + os.environ["PATH"],
                   TIDEMUX_VERSION=args.version, TIDEMUX_INSTALL_DIR=str(destination),
                   TIDEMUX_LOCAL_ASSETS=str(assets), TIDEMUX_SECURITY_MARKER=str(root / "security-invoked"))

        def verify_data():
            if {str(p.relative_to(data)): digest(p) for p in data.rglob("*") if p.is_file()} != original:
                raise RuntimeError("Install/uninstall modified existing user data")

        def install():
            marker = root / "security-invoked"
            prior_access = marker.read_bytes() if marker.exists() else b""
            subprocess.run(["sh", str(installer)], env=env, check=True, capture_output=True, text=True)
            binary = destination / "tidemux"
            if digest(binary) != binary_hash or subprocess.check_output([str(binary), "version"], text=True).strip() != args.version:
                raise RuntimeError("Installed binary differs from qualified package")
            if (marker.read_bytes() if marker.exists() else b"") != prior_access:
                raise RuntimeError("Installer accessed Keychain")
            subprocess.run([str(binary), "doctor", "--config", str(config)], env=env, check=True, capture_output=True, text=True)
            if list(destination.glob(".tidemux-install.*")):
                raise RuntimeError("Installer retained a temporary executable")
            verify_data()

        install()
        install()
        (destination / "tidemux").unlink()
        verify_data()
        install()
    print(json.dumps({"passed": True, "version": args.version, "archive_sha256": digest(archive),
                      "binary_sha256": binary_hash, "real_archive_installer": True, "reinstall": True,
                      "manual_uninstall_preserves_data": True, "installer_keychain_untouched": True, "doctor_uses_synthetic_keychain": True,
                      "formula_matches_archive": True, "public_download_and_homebrew_install": "pending publication"}))


if __name__ == "__main__":
    main()
