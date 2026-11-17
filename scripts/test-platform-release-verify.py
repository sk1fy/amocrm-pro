#!/usr/bin/env python3
import importlib.util
import json
import pathlib
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("release", pathlib.Path(__file__).with_name("platform-release-verify.py"))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)
REV = "a" * 40
IMAGE = "sha256:" + "b" * 64


class ArtifactTests(unittest.TestCase):
    def run_verify(self, revision=REV, migration=b"CREATE TABLE example();"):
        calls = []
        def fake(*args):
            calls.append(args)
            if args[:3] == ("docker", "image", "inspect"):
                return json.dumps([{"Id": IMAGE, "Os": "linux", "Architecture": "amd64", "Config": {"User": "10001", "Env": ["DATABASE_URL=postgres://secret@host/db"], "Labels": {"org.opencontainers.image.revision": revision}}}]).encode()
            if args[:2] == ("docker", "run"):
                self.assertEqual(sum(c[:3] == ("docker", "image", "inspect") for c in calls), 3)
                self.assertEqual(args[-2], IMAGE)
                return REV.encode()
            if args[:2] == ("docker", "create"):
                self.assertEqual(args[2], IMAGE)
                return b"container-id"
            if args[:2] == ("docker", "cp"):
                pathlib.Path(args[3], "000001.up.sql").write_bytes(migration)
            return b""
        with patch.object(release, "command", side_effect=fake), patch.object(release, "source_migrations", return_value={"000001.up.sql": b"CREATE TABLE example();"}):
            return release.verify(dict.fromkeys(("platform", "migrate", "lifecycle"), IMAGE), REV, "config-1", "identity-1")

    def test_verified_manifest_has_only_safe_metadata(self):
        manifest = self.run_verify()
        self.assertEqual(manifest["images"]["platform"]["image_id"], IMAGE)
        self.assertNotIn("secret", json.dumps(manifest))
        self.assertNotIn("DATABASE_URL", json.dumps(manifest))
        self.assertEqual(set(manifest), {"schema_version", "source_revision", "configuration_id", "identity_id", "images", "migrations_sha256"})

    def test_wrong_revision_rejected(self):
        with self.assertRaisesRegex(ValueError, "revision mismatch"):
            self.run_verify(revision="c" * 40)

    def test_wrong_migrations_rejected(self):
        with self.assertRaisesRegex(ValueError, "migration image"):
            self.run_verify(migration=b"different SQL")

    def test_secrets_and_mutable_tags_rejected_before_docker(self):
        for value in ("postgres://user:secret@host/db", "key=secret", "token secret"):
            with patch.object(release, "command") as command:
                with self.assertRaises(ValueError):
                    release.verify(dict.fromkeys(("platform", "migrate", "lifecycle"), IMAGE), REV, value, "identity")
                command.assert_not_called()
        with self.assertRaisesRegex(ValueError, "immutable"):
            release.verify(dict.fromkeys(("platform", "migrate", "lifecycle"), "repo:latest"), REV, "config", "identity")


if __name__ == "__main__":
    unittest.main()
