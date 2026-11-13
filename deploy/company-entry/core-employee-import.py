from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
from urllib.request import Request, urlopen

CORE_USERS = "http://127.0.0.1:18085/admin/v1/accounts/{}/employee-import"


def fetch_employees(token, account):
    request = Request(CORE_USERS.format(account), headers={
        "Authorization": f"Bearer {token}",
        "X-Admin-Actor": "service:teamos-employee-import",
    })
    with urlopen(request, timeout=9) as response:
        snapshot = json.loads(response.read(1024 * 1024 + 1))
    connection = snapshot.get("connection") or {}
    if (snapshot.get("state") != "ok" or snapshot.get("account_id") != int(account)
            or not connection.get("installation_id")
            or not connection.get("integration_code")
            or datetime.fromisoformat(snapshot["fresh_until"]) <= datetime.now(timezone.utc)):
        raise ValueError("unverified Core snapshot")
    rows = snapshot["items"]
    if not isinstance(rows, list) or len(rows) != snapshot["total"] or not rows:
        raise ValueError("incomplete Core snapshot")
    seen = set()
    employees = []
    for row in rows:
        crm_id, name, active = row["id"], row["name"], row["is_active"]
        if (type(crm_id) is not int or crm_id <= 0 or crm_id in seen
                or not isinstance(name, str) or type(active) is not bool):
            raise ValueError("invalid Core employee")
        seen.add(crm_id)
        if active:
            employee = {"id": str(crm_id), "name": name}
            email = row.get("email")
            if isinstance(email, str):
                email = email.strip().lower()
                if email and not email.endswith("@users.invalid"):
                    employee["email"] = email
            employees.append(employee)
    if not employees:
        raise ValueError("Core snapshot has no active employees")
    return employees


class Handler(BaseHTTPRequestHandler):
    def reply(self, status, document):
        body = json.dumps(document, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        if self.path != "/internal/v1/teamos/employee-import":
            return self.reply(404, {"result": False})
        try:
            size = int(self.headers.get("Content-Length", "0"))
            if not 0 < size <= 1024:
                raise ValueError
            self.connection.settimeout(10)
            body = json.loads(self.rfile.read(size))
        except (ValueError, OSError):
            return self.reply(400, {"result": False})
        if not isinstance(body, dict) or body.get("app_name") != "rkrs_activity":
            return self.reply(403, {"result": False})
        account = body.get("amo_account_id")
        if (not isinstance(account, str) or not account.isascii() or not account.isdecimal()
                or account.startswith("0") or len(account) > 19):
            return self.reply(400, {"result": False})
        try:
            employees = fetch_employees(self.server.core_token, account)
        except (KeyError, TypeError, ValueError, OSError):
            return self.reply(503, {"result": False})
        return self.reply(200, {"result": True, "data": employees})

    def log_message(self, _format, *_args):
        pass


if __name__ == "__main__":
    with ThreadingHTTPServer(("127.0.0.1", 18275), Handler) as server:
        directory = Path(os.environ["CREDENTIALS_DIRECTORY"])
        server.core_token = (directory / "core-admin-token").read_text().strip()
        server.serve_forever()
