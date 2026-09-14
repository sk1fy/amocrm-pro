#!/usr/bin/env python3
"""Minimal Alertmanager webhook sink for the observability test channel.

Logs every received POST body to stdout (visible via `docker logs
observability-webhook-sink-1`). Replace the URL in the secret file
/opt/amocrm-observability/secrets/alertmanager_webhook_url with the agreed
real channel once provided.
"""
import http.server
import json
import sys


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0) or 0)
        body = self.rfile.read(length).decode("utf-8", "replace") if length else ""
        record = {"path": self.path, "body": body}
        print(json.dumps(record), flush=True)
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8080
    http.server.HTTPServer(("0.0.0.0", port), Handler).serve_forever()
