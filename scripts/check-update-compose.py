#!/usr/bin/env python3
"""Validate the resolved update config without printing interpolated secrets."""
import json
import sys


def validate(config, task_control, distribution):
    worker = config["services"]["worker"]
    env = worker.get("environment", {})
    ports = worker.get("ports", [])

    def require(condition, message):
        if not condition:
            raise ValueError(message)

    for enabled, prefix, target, required in (
        (task_control, "TASK_CONTROL", 8087,
         ("SERVICE_KEYS", "ASSERTION_KEY_ID", "ASSERTION_PRIVATE_KEY")),
        (distribution, "DISTRIBUTION", 8084,
         ("SERVICE_KEYS", "TEAMOS_KEY_ID", "TEAMOS_URL")),
    ):
        if not enabled and not env.get(prefix + "_HTTP_ADDRESS"):
            continue
        require(env.get(prefix + "_HTTP_ADDRESS") == ":" + str(target),
                prefix + ": missing worker listener")
        require(str(env.get(prefix + "_TLS_PROXY")).lower() == "true",
                prefix + ": TLS proxy must remain enabled")
        for suffix in required:
            require(bool(env.get(prefix + "_" + suffix)),
                    prefix + "_" + suffix + ": required setting is empty")
        bindings = [p for p in ports if p.get("target") == target]
        require(bool(bindings), prefix + ": missing loopback port (check overlay order)")
        require(all(p.get("host_ip") == "127.0.0.1" and p.get("published")
                    and p.get("protocol", "tcp") == "tcp" for p in bindings),
                prefix + ": listener must only be published on 127.0.0.1")


if __name__ == "__main__":
    try:
        validate(json.load(sys.stdin), sys.argv[1] == "1", sys.argv[2] == "1")
    except (ValueError, KeyError, TypeError, IndexError):
        # Do not echo config, JSON parser context, or any values from .env.
        print("Invalid update Compose config: required widget listener, settings or loopback ports missing. "
              "Use Distribution overlay BEFORE Task Control loopback overlay.", file=sys.stderr)
        sys.exit(1)
    print("Update Compose config OK (widget listeners and loopback ports checked)")
