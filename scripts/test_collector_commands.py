#!/usr/bin/env python3
"""Regression checks for retained collector command output and cleanup."""
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from test_elasticsearch_package import cleanup_container, run_logged


class CollectorCommandsTest(unittest.TestCase):
    def test_success_and_nonzero_exit_retain_combined_output(self):
        with tempfile.TemporaryDirectory() as temporary:
            log=Path(temporary)/'command.log'
            run_logged([sys.executable,'-c','print("ready")'],log)
            self.assertEqual(log.read_text(),'ready\n')
            with self.assertRaisesRegex(RuntimeError,'command.log failed'):
                run_logged([sys.executable,'-c','import sys; print("invalid config",file=sys.stderr); sys.exit(7)'],log)
            self.assertEqual(log.read_text(),'invalid config\n')

    def test_timeout_retains_flushed_partial_output(self):
        with tempfile.TemporaryDirectory() as temporary:
            log=Path(temporary)/'timeout.log'
            with self.assertRaises(subprocess.TimeoutExpired):
                run_logged([sys.executable,'-c','import time; print("starting JVM",flush=True); time.sleep(30)'],log,timeout=.5)
            self.assertEqual(log.read_text(),'starting JVM\n')

    def test_cleanup_attempts_stop_and_remove_after_failed_log_collection(self):
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary)
            docker=root/'docker'
            docker.write_text('#!'+sys.executable+'\nimport sys\nprint(" ".join(sys.argv[1:]),flush=True)\nsys.exit(1 if sys.argv[1]=="logs" else 0)\n')
            docker.chmod(0o700)
            log=root/'cleanup.log'
            cleanup_container(str(docker),'fixture-container',log)
            self.assertEqual(log.read_text().splitlines(),[
                'logs fixture-container','stop -t 15 fixture-container','rm -f fixture-container'])


if __name__=='__main__':
    unittest.main()
