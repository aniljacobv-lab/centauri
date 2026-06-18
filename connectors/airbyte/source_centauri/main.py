#!/usr/bin/env python3
"""Airbyte source for Centauri.

Streams Centauri facts out to any Airbyte destination (a warehouse, lake, or
another database) using the CDC endpoint GET /v1/changes. Incremental sync is
free: Centauri's resumable byte-offset cursor maps directly onto Airbyte's
STATE — save it, reconnect, and you get only new facts, never duplicates.

Pure Python standard library only. Implements the Airbyte Protocol commands a
source needs: spec, check, discover, read.
"""
import argparse
import json
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

DOC_URL = "https://github.com/aniljacobv-lab/centauri/tree/main/connectors/airbyte"

SPEC = {
    "documentationUrl": DOC_URL,
    "connectionSpecification": {
        "$schema": "http://json-schema.org/draft-07/schema#",
        "title": "Centauri Source Spec",
        "type": "object",
        "required": ["centauri_url"],
        "additionalProperties": True,
        "properties": {
            "centauri_url": {
                "type": "string", "title": "Centauri URL", "order": 0,
                "default": "http://localhost:7771",
                "description": "Base URL of the Centauri HTTP API.",
            },
            "token": {
                "type": "string", "title": "API token", "order": 1,
                "airbyte_secret": True,
                "description": "Bearer token, if the server requires one.",
            },
            "database": {
                "type": "string", "title": "Database", "order": 2,
                "description": "Named environment (optional; default DB if blank).",
            },
        },
    },
}

# One stream: the fact log. The cursor is Centauri's byte offset into the log.
STREAM = {
    "name": "facts",
    "json_schema": {
        "$schema": "http://json-schema.org/draft-07/schema#",
        "type": "object",
        "properties": {
            "event_id": {"type": "string"},
            "subject": {"type": "string"},
            "facet": {"type": "string"},
            "type": {"type": "string"},
            "value": {"type": "object"},
            "effective_time": {"type": "integer"},
            "recorded_time": {"type": "integer"},
            "provenance": {"type": "string"},
            "confidence": {"type": "number"},
        },
    },
    "supported_sync_modes": ["full_refresh", "incremental"],
    "source_defined_cursor": True,
    "default_cursor_field": ["cursor"],
}


def emit(msg):
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()


def log(level, message):
    emit({"type": "LOG", "log": {"level": level, "message": message}})


def load(path):
    with open(path) as f:
        return json.load(f)


def http_get(config, path):
    url = config["centauri_url"].rstrip("/") + path
    if config.get("database"):
        sep = "&" if "?" in url else "?"
        url += sep + urllib.parse.urlencode({"db": config["database"]})
    req = urllib.request.Request(url, method="GET")
    if config.get("token"):
        req.add_header("Authorization", "Bearer " + config["token"])
    with urllib.request.urlopen(req, timeout=60) as resp:
        raw = resp.read()
        return json.loads(raw) if raw else {}


def cmd_spec(_args):
    emit({"type": "SPEC", "spec": SPEC})


def cmd_check(args):
    config = load(args.config)
    try:
        http_get(config, "/v1/stats")
        emit({"type": "CONNECTION_STATUS", "connectionStatus": {"status": "SUCCEEDED"}})
    except Exception as e:  # noqa: BLE001
        emit({"type": "CONNECTION_STATUS",
              "connectionStatus": {"status": "FAILED", "message": str(e)}})


def cmd_discover(args):
    load(args.config)  # validate config is readable
    emit({"type": "CATALOG", "catalog": {"streams": [STREAM]}})


def state_msg(cursor):
    return {
        "type": "STATE",
        "state": {
            "type": "STREAM",
            "stream": {
                "stream_descriptor": {"name": "facts"},
                "stream_state": {"cursor": cursor},
            },
            # legacy field for older platforms
            "data": {"facts": {"cursor": cursor}},
        },
    }


def extract_cursor(state):
    if isinstance(state, list):
        for s in state:
            ss = s.get("stream", {}).get("stream_state", {})
            if "cursor" in ss:
                return int(ss["cursor"])
    if isinstance(state, dict):
        st = state.get("stream", {}).get("stream_state", {})
        if "cursor" in st:
            return int(st["cursor"])
        d = state.get("data", {})
        if isinstance(d.get("facts"), dict) and "cursor" in d["facts"]:
            return int(d["facts"]["cursor"])
        if "cursor" in d:
            return int(d["cursor"])
    return 0


def cmd_read(args):
    config = load(args.config)
    catalog = load(args.catalog)
    incremental = any(
        cs.get("stream", {}).get("name") == "facts" and cs.get("sync_mode") == "incremental"
        for cs in catalog.get("streams", [])
    )
    cursor = 0
    if incremental and args.state:
        try:
            cursor = extract_cursor(load(args.state))
        except Exception:  # noqa: BLE001 — a bad/empty state just means start over
            cursor = 0

    emitted = 0
    while True:
        resp = http_get(config, "/v1/changes?from=%d" % cursor)
        for ev in resp.get("events", []):
            emit({"type": "RECORD",
                  "record": {"stream": "facts", "data": ev,
                             "emitted_at": int(time.time() * 1000)}})
            emitted += 1
        cursor = int(resp.get("cursor", cursor))
        emit(state_msg(cursor))
        if resp.get("caught_up", True):
            break
    log("INFO", "centauri source: emitted %d fact(s), cursor=%d" % (emitted, cursor))


def main():
    p = argparse.ArgumentParser(prog="source-centauri")
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("spec")
    for name in ("check", "discover"):
        sp = sub.add_parser(name)
        sp.add_argument("--config", required=True)
    r = sub.add_parser("read")
    r.add_argument("--config", required=True)
    r.add_argument("--catalog", required=True)
    r.add_argument("--state", required=False)
    args = p.parse_args()
    {"spec": cmd_spec, "check": cmd_check,
     "discover": cmd_discover, "read": cmd_read}[args.cmd](args)


if __name__ == "__main__":
    main()
