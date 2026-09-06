#!/usr/bin/env python3
"""Development Compose smoke. Restarts API and briefly stops Activity; no DB reset."""
import datetime
import json
from pathlib import Path
import subprocess
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
COMPOSE = ["docker-compose", "-f", "docker-compose.activity.yml"]
BASE = "http://127.0.0.1:18082"


def command(*args):
    print("$", " ".join(args), flush=True)
    result = subprocess.run(args, cwd=ROOT, text=True, capture_output=True)
    print(result.stdout, end="", flush=True)
    print(result.stderr, end="", flush=True)
    result.check_returncode()
    return result.stdout


def get(path):
    try:
        with urllib.request.urlopen(BASE + path, timeout=5) as response:
            return response.status, response.read().decode()
    except urllib.error.HTTPError as error:
        return error.code, error.read().decode()


def expect(path, status):
    deadline = time.monotonic() + 30
    last = None
    while time.monotonic() < deadline:
        try:
            last = get(path)
            if last[0] == status:
                print(f"GET {BASE}{path} -> {last[0]} {last[1].strip()}", flush=True)
                return last[1]
        except (OSError, urllib.error.URLError) as error:
            last = str(error)
        time.sleep(0.5)
    raise AssertionError(f"{path}: expected {status}, got {last}")


def catalog(entries, owner=None):
    seen = set()
    for entry in entries:
        descriptor, placement = entry["descriptor"], entry["placement"]
        code = descriptor["service_code"]
        seen.add(code)
        assert placement["mode"] == "grpc", entry
        assert placement["owns_database"] == (code == owner), entry
        assert (placement["max_connections"] > 0) == (code == owner), entry
        assert descriptor["contract_version"] == "v1", entry
        if code in ("activity", "crm-events"):
            assert descriptor["product_version"] == "v0", entry
    assert owner is None or owner in seen, seen
    return seen


print(datetime.datetime.now(datetime.timezone.utc).isoformat(), flush=True)
expect("/ready", 200)
expect("/components/activity/ready", 200)
assert catalog(json.loads(expect("/components", 200))) == {"activity", "crm-events", "gateway"}
for service, port, owner in [("worker", 8081, "gateway"), ("activity", 8091, "activity"), ("crm-events", 8092, "crm-events")]:
    output = command(*COMPOSE, "exec", "-T", service, "wget", "-qO-", f"http://127.0.0.1:{port}/components")
    catalog(json.loads(output), owner)
try:
    command(*COMPOSE, "stop", "activity")
    command(*COMPOSE, "restart", "api")
    expect("/ready", 200)
    expect("/components/activity/ready", 503)
finally:
    command(*COMPOSE, "start", "activity")
expect("/ready", 200)
expect("/components/activity/ready", 200)
for service in ("api", "worker", "activity", "crm-events", "postgres"):
    container = command(*COMPOSE, "ps", "-q", service).strip()
    command("docker", "inspect", "--format", "{{.Name}} image={{.Image}} state={{.State.Status}} health={{.State.Health.Status}}", container)
print("PASS: current grpc runtime registry, local ownership, Core restart while Activity down, recovery; no live amoCRM requests.", flush=True)
