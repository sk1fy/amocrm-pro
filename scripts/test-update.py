#!/usr/bin/env python3
"""Exercise the updater with real Compose merging and fake container operations."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
COMPOSE = shutil.which("docker-compose")
if not COMPOSE:
    COMPOSE = shutil.which("docker")
if not COMPOSE:
    raise RuntimeError("Docker Compose is required for real config merging")
FILES = ["docker-compose.yml", "docker-compose.v0.yml", "docker-compose.task-control.yml",
         "deploy/distribution-staging/core-distribution.local.yaml",
         "deploy/task-control-staging/docker-compose.loopback.yml"]

DOCKER = r'''#!/usr/bin/env python3
import json, os, subprocess, sys
from pathlib import Path
a = sys.argv[1:]
if a[0] == 'inspect':
    print(os.environ['MOCK_DEPLOYED_FILES']); sys.exit()
assert a.pop(0) == 'compose', a
files = []
while a and a[0] == '-f':
    files += a[:2]; a = a[2:]
command = a[0]
with open(os.environ['TRACE'], 'a') as f:
    f.write(json.dumps({'files': files, 'args': a}) + '\n')
if command == 'version':
    print('test compose'); sys.exit()
if command == 'ps' and '--quiet' in a:
    if os.environ.get('WORKER_EXISTS', '1') == '1': print('worker-id')
elif command == 'config':
    real = [os.environ['REAL_COMPOSE']]
    if Path(real[0]).name == 'docker': real += ['compose']
    sys.exit(subprocess.call(real + files + a))
elif command == 'build' and os.environ.get('FAIL_BUILD') == '1':
    sys.exit(19)
elif command == 'exec':
    if '--version' in a: print(os.environ['REVISION'])
    elif '--post-data=' in a:
        print('  HTTP/1.1 401 Unauthorized', file=sys.stderr); sys.exit(1)
'''


class UpdateTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        (self.repo / ".git").mkdir()
        for name in FILES + ["update.sh", "scripts/check-update-compose.py"]:
            dest = self.repo / name
            dest.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / name, dest)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.write_binary("docker", DOCKER)
        self.write_binary("sleep", "#!/bin/sh\nexit 0\n")
        self.write_binary("git", """#!/bin/sh
case "$*" in
  'rev-parse --git-dir') echo .git ;;
  'rev-parse --git-path update-last-state') echo .git/update-last-state ;;
  'rev-parse --abbrev-ref HEAD') echo main ;;
  'rev-parse HEAD') echo "$REVISION" ;;
esac
""")
        self.trace = self.root / "trace"
        self.env = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ["PATH"],
                        PROJECT_DIR=str(self.repo), TRACE=str(self.trace),
                        REAL_COMPOSE=COMPOSE, REVISION="a" * 40,
                        MOCK_DEPLOYED_FILES=",".join(str(self.repo / f) for f in FILES))
        for key in ("COMPOSE_FILE", "COMPOSE_PROFILES", "UPDATE_COMPOSE_FILES"):
            self.env.pop(key, None)
        # Synthetic fixtures only. No host .env or production credentials.
        self.env.update({
            "TASK_CONTROL_SERVICE_KEYS": "test-secret-do-not-log",
            "TASK_CONTROL_ASSERTION_KEY_ID": "test",
            "TASK_CONTROL_ASSERTION_PRIVATE_KEY": "test-secret-do-not-log",
            "DATABASE_URL": "postgres://test:test@postgres/core",
            "ENCRYPTION_KEYS": "test-secret-do-not-log", "ACTIVE_ENCRYPTION_KEY_VERSION": "1",
            "DISTRIBUTION_SERVICE_KEYS": "test-secret-do-not-log", "DISTRIBUTION_TEAMOS_KEY_ID": "test",
            "DISTRIBUTION_TEAMOS_URL": "https://teamos.example.test",
            "DISTRIBUTION_DOMAIN": "distribution.example.test", "CORE_BRIDGE_GATEWAY": "172.30.0.1",
            **{key: "test" for key in ("ACTIVITY_OWNER_PASSWORD", "EVENTS_OWNER_PASSWORD",
                                       "ACTIVITY_RUNTIME_PASSWORD", "EVENTS_RUNTIME_PASSWORD")},
        })

    def write_binary(self, name, source):
        path = self.bin / name
        path.write_text(source)
        path.chmod(0o755)

    def run_update(self, *args):
        result = subprocess.run(["bash", str(self.repo / "update.sh"), *args], env=self.env,
                                text=True, capture_output=True)
        self.assertNotIn("test-secret-do-not-log", result.stdout + result.stderr)
        calls = [json.loads(s) for s in self.trace.read_text().splitlines()]
        return result, calls

    def test_update_keeps_all_overlays_for_every_operation(self):
        result, calls = self.run_update("main")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        operations = [c for c in calls if c["args"][0] not in ("version",)
                      and "--quiet" not in c["args"]]
        expected = [arg for name in FILES for arg in ("-f", str(self.repo / name))]
        for call in operations:
            self.assertEqual(call["files"], expected)
        commands = [c["args"][0] for c in operations]
        self.assertLess(commands.index("config"), commands.index("build"))
        self.assertLess(commands.index("build"), commands.index("down"))
        self.assertLess(commands.index("down"), commands.index("up"))
        checks = str([c["args"] for c in operations if c["args"][0] == "exec"])
        self.assertIn("8084/components/distribution/ready", checks)
        self.assertIn("8087/api/v1/widget/task-control/auth", checks)

    def test_check_is_read_only(self):
        result, calls = self.run_update("--check")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([c["args"][0] for c in calls], ["version", "ps", "config"])
        self.assertFalse((self.repo / ".git/update-last-state").exists())

    def test_missing_overlay_aborts_before_mutation(self):
        (self.repo / FILES[-1]).unlink()
        result, calls = self.run_update("main")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(c["args"][0] in ("build", "down", "up") for c in calls))

    def test_reversed_overlays_cannot_drop_task_control_port(self):
        wrong = FILES[:3] + [FILES[4], FILES[3]]
        self.env["MOCK_DEPLOYED_FILES"] = ",".join(str(self.repo / f) for f in wrong)
        result, calls = self.run_update("main")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(c["args"][0] in ("build", "down", "up") for c in calls))

    def test_build_failure_does_not_stop_current_stack(self):
        self.env["FAIL_BUILD"] = "1"
        result, calls = self.run_update("main")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(c["args"][0] in ("down", "up") for c in calls))

    def test_empty_distribution_keys_aborts_before_stop(self):
        self.env["DISTRIBUTION_SERVICE_KEYS"] = ""
        result, calls = self.run_update("main")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(c["args"][0] in ("build", "down", "up") for c in calls))

    def test_public_task_control_port_rejected(self):
        path = self.repo / FILES[-1]
        path.write_text(path.read_text().replace("127.0.0.1", "0.0.0.0"))
        result, calls = self.run_update("--check")
        self.assertNotEqual(result.returncode, 0)

    def test_base_only_worker_does_not_enable_widgets(self):
        self.env["MOCK_DEPLOYED_FILES"] = str(self.repo / FILES[0])
        result, calls = self.run_update("main")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        build = next(c for c in calls if c["args"][0] == "build")
        self.assertEqual(build["args"], ["build", "api", "worker", "migrate"])


if __name__ == "__main__":
    unittest.main()
