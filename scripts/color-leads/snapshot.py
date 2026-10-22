#!/usr/bin/env python3
"""Offline ColorLeads export validation and reconciliation. Never activates owners."""
import argparse
import hashlib
import json
import pathlib
import re
import sys

PRODUCT = "rkrs_color_leads"
FIELDS = ("lead_id", "stage_id", "pipeline_id", "borderColor", "backgroundColor", "timestamp")


def canonical(value):
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False)


def digest(value):
    return hashlib.sha256(canonical(value).encode()).hexdigest()


def positive(value, label):
    if type(value) is not int or value <= 0:
        raise ValueError(label + " must be a positive integer")
    return value


def unique_object(pairs):
    out = {}
    for key, value in pairs:
        if key in out:
            raise ValueError("duplicate JSON key: " + key)
        out[key] = value
    return out


def read(path):
    def invalid(value):
        raise ValueError("non-finite JSON number: " + value)
    return json.loads(pathlib.Path(path).read_text(), object_pairs_hook=unique_object, parse_constant=invalid)


def normalize(export):
    if not isinstance(export, dict) or export.get("schema_version") != 1:
        raise ValueError("export schema_version must be 1")
    if export.get("product_code") != PRODUCT:
        raise ValueError("wrong product scope")
    account = positive(export.get("account_id"), "account_id")
    source = export.get("source")
    if not isinstance(source, dict) or source.get("platform") not in ("ssd", "core"):
        raise ValueError("source platform must be ssd or core")
    for field in ("revision", "boundary", "exported_at"):
        if not isinstance(source.get(field), str) or not source[field].strip():
            raise ValueError("source." + field + " is required")
    # A caller-supplied boundary is provenance, never a quiescence receipt.
    settings = export.get("settings")
    if not isinstance(settings, dict):
        raise ValueError("settings must be the effective SSD settings object; resolve defaults explicitly")
    if "main_settings" in settings and not isinstance(settings["main_settings"], list):
        raise ValueError("main_settings must be an ordered array")
    rows = export.get("colors")
    if not isinstance(rows, list):
        raise ValueError("colors must be an array")
    normalized, seen = [], set()
    for row in rows:
        if not isinstance(row, dict):
            raise ValueError("color row must be an object")
        if "amo_account_id" in row and row["amo_account_id"] != account:
            raise ValueError("cross-account source row")
        key = tuple(positive(row.get(k), k) for k in FIELDS[:3])
        if key in seen:
            raise ValueError("duplicate composite color key: " + str(key))
        seen.add(key)
        item = {k: row[k] for k in FIELDS[:3]}
        for field in FIELDS[3:]:
            value = row.get(field)
            if value is not None and not isinstance(value, str):
                raise ValueError(field + " must be string or null; no lossy coercion")
            item[field] = value
        normalized.append(item)
    normalized.sort(key=lambda row: tuple(row[k] for k in FIELDS[:3]))
    return {"schema_version": 1, "product_code": PRODUCT, "account_id": account,
            "settings": settings, "colors": normalized}


def plan(export):
    payload = normalize(export)
    blockers = []
    settings = payload["settings"]
    if len(canonical(settings).encode()) > 256 * 1024:
        blockers.append("settings exceeds current Core 256 KiB limit")
    rules = settings.get("main_settings", [])
    if len(rules) > 500 or any(not isinstance(rule, dict) for rule in rules):
        blockers.append("rules must be objects, maximum 500 in current Core")
    if len(payload["colors"]) > 1000:
        blockers.append("color rows exceed current Core snapshot limit 1000; do not truncate")
    for row in payload["colors"]:
        for field in ("borderColor", "backgroundColor"):
            if row[field] is not None and not re.fullmatch(r"#[0-9a-fA-F]{6}([0-9a-fA-F]{2})?", row[field]):
                blockers.append("unsupported Core color format at " + str((row["lead_id"], field)))
        if row["timestamp"] is not None and len(row["timestamp"].encode()) > 128:
            blockers.append("timestamp exceeds current Core 128 byte limit")
    return {"kind": "color-leads-import-candidate", "schema_version": 1,
            "source": export["source"], "payload": payload, "payload_sha256": digest(payload),
            "source_sha256": digest(export), "source_rows": export["colors"],
            "counts": {"color_rows": len(payload["colors"]),
                       "rules": len(payload["settings"].get("main_settings", []))},
            "activation_allowed": False,
            "target_compatible": not blockers, "blockers": blockers,
            "requirements": ["authenticated Core import command with matching frozen transfer",
                             "server-verified source quiescence and no uncertain requests",
                             "reconciliation at final source boundary before activation"]}


def reconcile(source, target):
    left, right = normalize(source), normalize(target)
    if left["account_id"] != right["account_id"]:
        raise ValueError("cannot reconcile different accounts")
    def keyed(rows):
        return {tuple(row[k] for k in FIELDS[:3]): row for row in rows}
    a, b = keyed(left["colors"]), keyed(right["colors"])
    missing = sorted(a.keys() - b.keys())
    extra = sorted(b.keys() - a.keys())
    changed = sorted(k for k in a.keys() & b.keys() if a[k] != b[k])
    equal_settings = canonical(left["settings"]) == canonical(right["settings"])
    return {"matches": left == right, "account_id": left["account_id"],
            "source_sha256": digest(left), "target_sha256": digest(right),
            "settings_match": equal_settings,
            "missing_keys": missing, "extra_keys": extra, "changed_keys": changed,
            "source_count": len(a), "target_count": len(b),
            "source_archive_sha256": digest(source), "target_archive_sha256": digest(target),
            "scope": "normalized operational projection only; source metadata is archived separately; does not prove owner fencing, billing or live behavior"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    prepare = sub.add_parser("plan")
    prepare.add_argument("source")
    compare = sub.add_parser("reconcile")
    compare.add_argument("source")
    compare.add_argument("target")
    args = parser.parse_args()
    try:
        source = read(args.source)
        result = plan(source) if args.command == "plan" else reconcile(source, read(args.target))
        print(json.dumps(result, ensure_ascii=False, indent=2, allow_nan=False))
        return 0 if result.get("matches", result.get("target_compatible", True)) else 1
    except (ValueError, OSError, TypeError) as exc:
        print("invalid snapshot: " + str(exc), file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
