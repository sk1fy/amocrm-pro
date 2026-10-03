#!/usr/bin/env python3
"""Offline, synthetic two-owner backup coordinator. Never serves HTTP.

Only the newly created Core test container and a PostgreSQL testcontainers
container supplied by this fixture can be addressed. No host DB URL or secret
is accepted. pg_dump/restore are executed inside those disposable containers.
"""
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

bridge = Path(sys.argv[1])
core_id = sys.argv[2]
project = sys.argv[3]
if not re.fullmatch(r"amocrm-pro-distribution-bridge-[0-9]+", project):
    raise ValueError("Invalid disposable bridge run")
sequence = 1

def inspect(container):
    if not re.fullmatch(r"[a-f0-9]{12,64}", container):
        raise ValueError("Invalid fixture container identity")
    return json.loads(subprocess.check_output(["docker", "inspect", container]))[0]

core = inspect(core_id)
if core.get("Config", {}).get("Labels", {}).get("com.docker.compose.project") != project or core.get("Config", {}).get("Labels", {}).get("com.docker.compose.service") != "postgres":
    raise ValueError("Core is not this disposable bridge project")

def run(container, executable, user, database, data=None):
    return subprocess.run(["docker", "exec", "-i", container, executable, "-U", user,
                           "-d", database, *(["--format=custom"] if executable == "pg_dump"
                                            else ["--clean", "--if-exists", "--exit-on-error"])],
                          input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=True).stdout

while not (bridge / "backup-stop").exists():
    request = bridge / f"backup-request-{sequence}.json"
    if not request.exists():
        time.sleep(0.1)
        continue
    try:
        body = json.loads(request.read_text())
        team_id = body["teamContainerID"]
        team = inspect(team_id)
        if team.get("Config", {}).get("Labels", {}).get("org.testcontainers") != "true":
            raise ValueError("Team owner is not a disposable testcontainers fixture")
        if team.get("Config", {}).get("Labels", {}).get("rkrs.distribution.bridge_run") != project:
            raise ValueError("Team owner belongs to another fixture run")
        if not str(team.get("Config", {}).get("Image", "")).startswith("postgres:"):
            raise ValueError("Team owner is not PostgreSQL")
        action = body["action"]
        if action not in ("backup", "restore"):
            raise ValueError("Unsupported offline fixture operation")
        targets = [(core_id, "amocrm_test", "amocrm_test", "core"),
                   (team_id, "company", "company", "team")]
        for container, user, database, owner in targets:
            dump = bridge / f"{owner}-owner-fixture.dump"
            if action == "backup":
                raw = run(container, "pg_dump", user, database)
                fd = os.open(dump, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
                os.fchmod(fd, 0o600)
                with os.fdopen(fd, "wb") as stream:
                    stream.write(raw)
            else:
                run(container, "pg_restore", user, database, dump.read_bytes())
        result = {"ok": True, "action": action, "owners": 2, "synthetic": True}
    except Exception as error:
        # Never expose connection configuration or dump contents in evidence.
        result = {"ok": False, "error": type(error).__name__}
    temporary = bridge / f"backup-reply-{sequence}.tmp"
    temporary.write_text(json.dumps(result))
    temporary.rename(bridge / f"backup-reply-{sequence}.json")
    sequence += 1
