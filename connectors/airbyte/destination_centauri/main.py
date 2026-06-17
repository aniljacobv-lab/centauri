#!/usr/bin/env python3
"""Airbyte destination for Centauri.

Lands records from any of Airbyte's sources into Centauri as bi-temporal
facts, via the HTTP API (POST /v1/append). One connector replaces N bespoke
importers: point any Airbyte source (Postgres, Mongo, Stripe, Salesforce, …)
at this destination and every row becomes an immutable, time-stamped fact.

Pure Python standard library only — no Airbyte CDK, no third-party packages,
matching Centauri's zero-dependency ethos. Implements the Airbyte Protocol
commands a destination needs: spec, check, write.
"""
import argparse
import json
import sys
import urllib.error
import urllib.parse
import urllib.request

DOC_URL = "https://github.com/proxima360/centauri/tree/main/connectors/airbyte"

SPEC = {
    "documentationUrl": DOC_URL,
    "supported_destination_sync_modes": ["append"],
    "supportsIncremental": True,
    "connectionSpecification": {
        "$schema": "http://json-schema.org/draft-07/schema#",
        "title": "Centauri Destination Spec",
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
            "facet": {
                "type": "string", "title": "Facet", "order": 3, "default": "source",
                "description": "Facet each fact is written on.",
            },
            "primary_key": {
                "type": "string", "title": "Primary key field", "order": 4,
                "description": "Record field used as the subject key "
                               "(subject = '<stream>:<value>'). If blank, a per-stream "
                               "counter is used.",
            },
            "provenance": {
                "type": "string", "title": "Provenance", "order": 5,
                "default": "SYSTEM_FEED",
                "enum": ["SYSTEM_FEED", "HUMAN_ENTRY", "SCAN_VERIFIED", "AI_INFERRED"],
            },
            "batch_size": {
                "type": "integer", "title": "Batch size", "order": 6, "default": 500,
                "description": "Facts per /v1/append request.",
            },
        },
    },
}


def emit(msg):
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()


def log(level, message):
    emit({"type": "LOG", "log": {"level": level, "message": message}})


def load(path):
    with open(path) as f:
        return json.load(f)


def http(config, method, path, body=None):
    url = config["centauri_url"].rstrip("/") + path
    if config.get("database"):
        url += "?" + urllib.parse.urlencode({"db": config["database"]})
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
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
        http(config, "GET", "/v1/stats")
        emit({"type": "CONNECTION_STATUS", "connectionStatus": {"status": "SUCCEEDED"}})
    except Exception as e:  # noqa: BLE001 — report any failure to Airbyte
        emit({"type": "CONNECTION_STATUS",
              "connectionStatus": {"status": "FAILED", "message": str(e)}})


def cmd_write(args):
    config = load(args.config)
    facet = config.get("facet", "source")
    pk = config.get("primary_key") or ""
    provenance = config.get("provenance", "SYSTEM_FEED")
    batch_size = int(config.get("batch_size", 500))
    counters = {}
    buf = []

    def flush():
        if buf:
            http(config, "POST", "/v1/append", {"events": buf})
            buf.clear()

    written = 0
    for raw in sys.stdin:
        raw = raw.strip()
        if not raw:
            continue
        try:
            msg = json.loads(raw)
        except json.JSONDecodeError:
            continue
        kind = msg.get("type")
        if kind == "RECORD":
            rec = msg.get("record", {})
            stream = rec.get("stream", "stream")
            data = rec.get("data", {})
            if pk and pk in data:
                key = data[pk]
            else:
                counters[stream] = counters.get(stream, 0) + 1
                key = counters[stream]
            buf.append({
                "subject": "%s:%s" % (stream, key),
                "facet": facet,
                "type": "OBSERVED",
                "value": data,
                "provenance": provenance,
                "confidence": 1.0,
            })
            written += 1
            if len(buf) >= batch_size:
                flush()
        elif kind == "STATE":
            # Durably persist everything up to this state, THEN acknowledge it
            # back to Airbyte — the contract that makes resumable syncs safe.
            flush()
            emit(msg)
    flush()
    log("INFO", "centauri destination: wrote %d fact(s)" % written)


def main():
    p = argparse.ArgumentParser(prog="destination-centauri")
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("spec")
    c = sub.add_parser("check")
    c.add_argument("--config", required=True)
    w = sub.add_parser("write")
    w.add_argument("--config", required=True)
    w.add_argument("--catalog", required=True)
    args = p.parse_args()
    {"spec": cmd_spec, "check": cmd_check, "write": cmd_write}[args.cmd](args)


if __name__ == "__main__":
    main()
