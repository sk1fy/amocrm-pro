"""Exercise secret-file permissions and cleanup without starting Docker."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class TransferTemporaryFilesTest(unittest.TestCase):
    def test_early_exit_and_config_failure_remove_private_files(self):
        script = Path(__file__).with_name("verify-transfer.sh").resolve()
        for failure in ("0", "1"):
            with self.subTest(config_failure=failure), tempfile.TemporaryDirectory() as root:
                root = Path(root)
                binaries = root / "bin"
                binaries.mkdir()
                scratch = root / "scratch"
                scratch.mkdir()
                stub = binaries / "docker-compose"
                stub.write_text("""#!/bin/sh
set -eu
for file in "$TMPDIR"/*/config.yml; do
  [ -f "$file" ] || continue
  python3 -c 'import os,sys; p=sys.argv[1]; assert os.stat(p).st_mode & 0o777 == 0o600; assert os.stat(os.path.dirname(p)).st_mode & 0o777 == 0o700' "$file"
  touch "$CHECKED"
  [ "$FAIL_CONFIG" = 0 ] || exit 23
done
printf '%s\n' 'name: amocrm-activity' core-plane activity-plane events-plane rpc-plane core-postgres activity-postgres events-postgres activity:9091 crm-events:9092 worker:9090 events-postgres-target activity-postgres-target
""")
                stub.chmod(0o700)
                docker = binaries / "docker"
                docker.write_text("#!/bin/sh\nexit 0\n")
                docker.chmod(0o700)
                checked = root / "checked"
                env = dict(os.environ, PATH=str(binaries) + os.pathsep + os.environ["PATH"],
                           TMPDIR=str(scratch), TRANSFER_LIVE="1",
                           TRANSFER_PROJECT="audit-private-test",
                           CHECKED=str(checked), FAIL_CONFIG=failure)
                result = subprocess.run(["sh", str(script)], env=env, capture_output=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertTrue(checked.exists(), result.stderr.decode())
                self.assertEqual(list(scratch.iterdir()), [])
                if failure == "0":
                    self.assertIn(b"TRANSFER_LIVE gRPC cutover", result.stderr)


if __name__ == "__main__":
    unittest.main()
