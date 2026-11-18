#!/usr/bin/env python3
"""Verify pinned local release artifacts without reading runtime secrets."""
import argparse
import hashlib
import json
import pathlib
import re
import subprocess
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
IMAGE_ID = re.compile(r"sha256:[0-9a-f]{64}\Z")
PINNED = re.compile(r"[^\s@]+@sha256:[0-9a-f]{64}\Z")
IDENTIFIER = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}\Z")


def command(*args):
    result = subprocess.run(args, cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if result.returncode:
        # Docker and git diagnostics may contain caller input; never relay them.
        raise ValueError("artifact command failed")
    return result.stdout


def source_migrations(revision):
    names = command("git", "ls-tree", "-r", "--name-only", revision, "--", "migrations-platform").decode().splitlines()
    files = {str(pathlib.PurePosixPath(name).relative_to("migrations-platform")): command("git", "show", f"{revision}:{name}") for name in names if name.endswith(".sql")}
    if not files:
        raise ValueError("source migrations missing")
    return files


def migration_hash(files):
    digest = hashlib.sha256()
    for name, content in sorted(files.items()):
        digest.update(name.encode() + b"\0" + content + b"\0")
    return digest.hexdigest()


def verify(images, revision, config_id, identity_id):
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise ValueError("full source revision required")
    if not all(IDENTIFIER.fullmatch(value) for value in (config_id, identity_id)):
        raise ValueError("configuration and identity IDs must be nonsecret identifiers")
    if not all(IMAGE_ID.fullmatch(ref) or PINNED.fullmatch(ref) for ref in images.values()):
        raise ValueError("immutable image ID or repository digest required")
    # Resolve every input before running any image. All later uses use these IDs.
    resolved = {}
    for role, ref in images.items():
        info = json.loads(command("docker", "image", "inspect", ref))[0]
        if not IMAGE_ID.fullmatch(info["Id"]):
            raise ValueError("invalid resolved image ID")
        resolved[role] = info
    architectures = set()
    result = {}
    for role, info in resolved.items():
        cfg = info["Config"]
        if (cfg.get("Labels") or {}).get("org.opencontainers.image.revision") != revision:
            raise ValueError("image revision mismatch")
        user = cfg.get("User", "").split(":")[0]
        if not user or user in ("0", "root"):
            raise ValueError("nonroot runtime required")
        if info.get("Os") != "linux" or info.get("Architecture") not in ("amd64", "arm64"):
            raise ValueError("unsupported image architecture")
        architectures.add(info["Architecture"])
        binary = {"platform": "/usr/local/bin/amocrm-platform",
                  "migrate": "/usr/local/bin/amocrm-migrate",
                  "lifecycle": "/usr/local/bin/platform-lifecycle"}[role]
        version = command("docker", "run", "--rm", "--network=none", "--entrypoint", binary, info["Id"], "--version").decode().strip()
        if version != revision:
            raise ValueError("binary revision mismatch")
        result[role] = {"image_id": info["Id"], "architecture": info["Architecture"], "revision": version}
    if len(architectures) != 1:
        raise ValueError("image architectures differ")
    expected = source_migrations(revision)
    container = command("docker", "create", resolved["migrate"]["Id"]).decode().strip()
    try:
        with tempfile.TemporaryDirectory(prefix="platform-migrations-") as temp:
            command("docker", "cp", container + ":/migrations/.", temp)
            actual = {p.relative_to(temp).as_posix(): p.read_bytes() for p in pathlib.Path(temp).rglob("*") if p.is_file()}
            if actual != expected:
                raise ValueError("migration image differs from source commit")
    finally:
        command("docker", "rm", container)
    return {"schema_version": 1, "source_revision": revision, "configuration_id": config_id,
            "identity_id": identity_id, "images": result, "migrations_sha256": migration_hash(expected)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("platform-image", "migrate-image", "lifecycle-image", "revision", "out", "config-id", "identity-id"):
        parser.add_argument("--" + name, required=True)
    args = parser.parse_args()
    try:
        manifest = verify({"platform": args.platform_image, "migrate": args.migrate_image, "lifecycle": args.lifecycle_image}, args.revision, args.config_id, args.identity_id)
        pathlib.Path(args.out).write_text(json.dumps(manifest, indent=2) + "\n")
    except (ValueError, KeyError, TypeError, OSError, json.JSONDecodeError):
        parser.exit(1, "release verification failed; no manifest written\n")


if __name__ == "__main__":
    main()
