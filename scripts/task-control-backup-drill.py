#!/usr/bin/env python3
"""Disposable Core migration/backup/restore drill. Never accepts an external DB."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import uuid

root = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--output", type=Path, required=True)
args = parser.parse_args()
out = args.output.resolve()
out.mkdir(parents=True, exist_ok=False)
os.chmod(out, 0o700)
name = "task-control-backup-" + uuid.uuid4().hex[:10]


def docker(*command, input=None, check=True):
    return subprocess.run(["docker", *command], input=input, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=check)


def sql(database, query, check=True):
    return docker("exec", "-i", name, "psql", "-X", "-qAt", "-U", "r10", "-d", database, "--set", "ON_ERROR_STOP=on", input=query.encode(), check=check)


def fingerprints(database):
    tables = sql(database, "SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename").stdout.decode().splitlines()
    assert len(tables) >= 30
    hashes = {}
    for table in tables:
        identifier = '"' + table.replace('"', '""') + '"'
        payload = sql(database, f"SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY to_jsonb(r)::text),'[]'::jsonb)::text FROM {identifier} r").stdout
        hashes[table] = hashlib.sha256(payload).hexdigest()
    return hashes


try:
    docker("run", "-d", "--name", name, "-p", "127.0.0.1::5432", "-e", "POSTGRES_DB=r10_core_test", "-e", "POSTGRES_USER=r10", "-e", "POSTGRES_PASSWORD=r10-synthetic-only", "postgres:17-alpine")
    for attempt in range(30):
        # The initdb temporary server accepts Unix sockets before its restart.
        # Wait for the final TCP listener, which is the one reached by migrate.
        if docker("exec", name, "pg_isready", "-h", "127.0.0.1", "-U", "r10", "-d", "r10_core_test", check=False).returncode == 0:
            break
        time.sleep(1)
    else:
        raise RuntimeError("PostgreSQL readiness timeout")
    port = docker("port", name, "5432/tcp").stdout.decode().strip().rsplit(":", 1)[1]

    def migrate(database, directory):
        env = dict(os.environ, GOWORK="off", DATABASE_URL=f"postgres://r10:r10-synthetic-only@127.0.0.1:{port}/{database}?sslmode=disable", MIGRATIONS_DIR=str(directory))
        with (out / "migrations.log").open("ab") as log:
            subprocess.run(["go", "run", "./cmd/migrate", "up"], cwd=root, env=env, stdout=log, stderr=subprocess.STDOUT, check=True)

    with tempfile.TemporaryDirectory(prefix="task-control-old-migrations-") as temporary:
        for file in (root / "migrations").glob("*.sql"):
            if int(file.name.split("_", 1)[0]) <= 28:
                shutil.copy2(file, temporary)
        migrate("r10_core_test", temporary)
    integration, installation, company, client = [str(uuid.uuid4()) for _ in range(4)]
    sql("r10_core_test", f"""
      BEGIN;
      INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri)
        VALUES('{integration}','r10-synthetic','{client}','synthetic-only','https://core.example.invalid/oauth/amocrm/callback');
      INSERT INTO integration_services(integration_id,service_code,enabled)VALUES('{integration}','task-control',true);
      INSERT INTO installations(id,integration_id,account_id,account_domain,status)VALUES('{installation}','{integration}',123,'synthetic.amocrm.ru','active');
      INSERT INTO task_control_service_grants(key_id,company_id,account_id,integration_id,enabled)VALUES('r10-synthetic','{company}',123,'{integration}',true);
      INSERT INTO task_control_runtime_descriptors(installation_id,generation,runtime_version,confirmed_by)VALUES('{installation}',1,'1.2.1','r10-local-drill');
      COMMIT;
    """)
    identity = sql("r10_core_test", f"SELECT generation FROM task_control_installation_versions WHERE installation_id='{installation}'").stdout
    migrate("r10_core_test", root / "migrations")
    assert identity == sql("r10_core_test", f"SELECT generation FROM task_control_installation_versions WHERE installation_id='{installation}'").stdout
    operator_env = dict(os.environ, GOWORK="off", APP_ENV="development", DATABASE_URL=f"postgres://r10:r10-synthetic-only@127.0.0.1:{port}/r10_core_test?sslmode=disable",
                        ENCRYPTION_KEYS="1:" + base64.b64encode(os.urandom(32)).decode(), ACTIVE_ENCRYPTION_KEY_VERSION="1")
    operator = ["go", "run", "./cmd/task-control-runtime-descriptor", "--installation-id", installation, "--generation", "1", "--runtime-version", "1.2.3", "--actor", "r10-local-drill"]
    with (out / "descriptor-cli.log").open("wb") as log:
        subprocess.run(operator, cwd=root, env=operator_env, stdout=log, stderr=subprocess.STDOUT, check=True)
    down = sql("r10_core_test", "BEGIN;\n" + (root / "migrations/000030_task_control_runtime_1_2_3.down.sql").read_text() + "\nCOMMIT;", check=False)
    assert down.returncode != 0 and b"1.2.3" in down.stderr, "downgrade must reject retained 1.2.3 evidence"
    before = fingerprints("r10_core_test")
    backup = docker("exec", name, "pg_dump", "-U", "r10", "-d", "r10_core_test", "--format=custom", "--no-owner", "--no-privileges").stdout
    archive = out / "core-synthetic.dump"
    archive.write_bytes(backup)
    os.chmod(archive, 0o600)
    docker("exec", name, "createdb", "-U", "r10", "r10_restored_test")
    docker("exec", "-i", name, "pg_restore", "-U", "r10", "-d", "r10_restored_test", "--exit-on-error", "--no-owner", "--no-privileges", input=backup)
    assert fingerprints("r10_restored_test") == before, "restore changed Core data"
    migrate("r10_restored_test", root / "migrations")
    assert fingerprints("r10_restored_test") == before, "migration replay changed restored data"
    report = {"kind": "synthetic-local-core-backup-restore", "result": "passed", "coreSHA": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True).strip(), "migrationUpgrade": "28 -> 30", "installationGenerationPreserved": True, "descriptorCLI": "passed", "downgradeWith123Rejected": True, "backupSHA256": hashlib.sha256(backup).hexdigest(), "tableFingerprints": before, "migrationReplayAfterRestore": "passed"}
    (out / "evidence.json").write_text(json.dumps(report, indent=2) + "\n")
    print(f"Core backup/restore passed: {len(before)} tables; evidence: {out / 'evidence.json'}")
finally:
    docker("rm", "-fv", name, check=False)
