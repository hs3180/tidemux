#!/usr/bin/env python3
"""Offline installer checks: verified install and failures preserve existing files."""
import hashlib
import io
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

INSTALLER = Path(__file__).with_name('install.sh').resolve()


class InstallerTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.mock = self.root / 'mock'
        self.mock.mkdir()
        self.assets = self.root / 'assets'
        self.assets.mkdir()
        self.dest = self.root / 'install with spaces'
        self.dest.mkdir()
        self.existing = self.dest / 'tidemux'
        self.existing.write_text('old installation')
        self.env = dict(os.environ, PATH=str(self.mock) + ':' + os.environ['PATH'],
                        TIDEMUX_INSTALL_DIR=str(self.dest), TIDEMUX_VERSION='0.1.0',
                        TEST_ASSETS=str(self.assets))
        self.command('uname', '#!/bin/sh\ncase "$1" in -s) echo Darwin;; -m) echo arm64;; esac\n')
        self.command('sw_vers', '#!/bin/sh\necho 15.7.4\n')
        self.command('curl', '''#!/bin/sh
while [ "$#" -gt 0 ]; do
  case "$1" in
    https://*) url=$1 ;;
    -o) shift; dest=$1 ;;
  esac
  shift
done
cp "$TEST_ASSETS/${url##*/}" "$dest"
''')
        self.asset = self.assets / 'tidemux_0.1.0_abcdef123456_darwin_arm64.tar.gz'
        binary = b'#!/bin/sh\necho 0.1.0\n'
        with tarfile.open(self.asset, 'w:gz') as archive:
            entry = tarfile.TarInfo('tidemux-0.1.0/tidemux')
            entry.size = len(binary)
            archive.addfile(entry, io.BytesIO(binary))
        digest = hashlib.sha256(self.asset.read_bytes()).hexdigest()
        (self.assets / 'SHA256SUMS').write_text(f'{digest}  {self.asset.name}\n')

    def command(self, name, body):
        p = self.mock / name
        p.write_text(body)
        p.chmod(0o755)

    def run_install(self):
        return subprocess.run(['sh', str(INSTALLER)], env=self.env,
                              text=True, capture_output=True)

    def test_install(self):
        result = self.run_install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(subprocess.check_output([str(self.existing), 'version'], text=True).strip(), '0.1.0')
        self.assertEqual(list(self.dest.glob('.tidemux-install.*')), [])

    def test_corrupt_archive_preserves_existing(self):
        self.asset.write_bytes(b'corrupt')
        self.assertNotEqual(self.run_install().returncode, 0)
        self.assertEqual(self.existing.read_text(), 'old installation')

    def test_missing_release_preserves_existing(self):
        (self.assets / 'SHA256SUMS').unlink()
        self.assertNotEqual(self.run_install().returncode, 0)
        self.assertEqual(self.existing.read_text(), 'old installation')

    def test_directory_target_preserves_contents(self):
        self.existing.unlink()
        self.existing.mkdir()
        marker = self.existing / 'keep'
        marker.write_text('preserve')
        self.assertNotEqual(self.run_install().returncode, 0)
        self.assertEqual(marker.read_text(), 'preserve')
        self.assertEqual(list(self.existing.iterdir()), [marker])

    def test_symlink_to_directory_preserves_contents(self):
        self.existing.unlink()
        target = self.root / 'other-directory'
        target.mkdir()
        self.existing.symlink_to(target, target_is_directory=True)
        self.assertNotEqual(self.run_install().returncode, 0)
        self.assertTrue(self.existing.is_symlink())
        self.assertEqual(list(target.iterdir()), [])

    def test_unsupported_platform_preserves_existing(self):
        self.command('uname', '#!/bin/sh\necho Linux\n')
        self.assertNotEqual(self.run_install().returncode, 0)
        self.assertEqual(self.existing.read_text(), 'old installation')


if __name__ == '__main__':
    unittest.main()
