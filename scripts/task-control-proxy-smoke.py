#!/usr/bin/env python3
"""Check the real staging nginx include with isolated loopback stub upstreams."""
import json
from pathlib import Path
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid

root = Path(__file__).resolve().parents[1]
name = "task-control-proxy-" + uuid.uuid4().hex[:10]
with tempfile.TemporaryDirectory(prefix="task-control-nginx-") as temporary:
    config = Path(temporary) / "nginx.conf"
    config.write_text('''events {}
http {
  access_log off;
  server { listen 18087; return 418; }
  server { listen 8180; return 419; }
  server { listen 8080;
    include /etc/nginx/task-control-public.conf;
    location / { return 404; }
  }
}
''')
    try:
        subprocess.run(["docker", "create", "--name", name, "-p", "127.0.0.1::8080",
                        "nginx:1.28-alpine"], check=True, stdout=subprocess.DEVNULL)
        subprocess.run(["docker", "cp", str(config), name + ":/etc/nginx/nginx.conf"], check=True)
        subprocess.run(["docker", "cp", str(root / "deploy/task-control-staging/public.locations.conf"), name + ":/etc/nginx/task-control-public.conf"], check=True)
        subprocess.run(["docker", "start", name], check=True, stdout=subprocess.DEVNULL)
        subprocess.run(["docker", "exec", name, "nginx", "-t"], check=True)
        port = subprocess.check_output(["docker", "port", name, "8080/tcp"], text=True).strip().rsplit(":", 1)[1]
        routes = {
            "/api/v1/widget/task-control/auth": 418,
            "/api/v1/widget/task-control/disconnect": 418,
            "/api/v1/widget/task-control/connection": 419,
            "/api/v1/widget/task-control/sessions": 419,
            "/api/v1/widget/task-control/bootstrap": 419,
            "/api/v1/widget/task-control/source-requests": 419,
            "/api/v1/widget/task-control/source-requests/12345678-1234-1234-1234-123456789abc/result": 419,
            "/api/v1/task-control/runtime/ack": 419,
            "/api/v1/task-control/runtime/telemetry": 419,
            "/api/v2/task-control/runtime/bootstrap": 419,
            "/api/v2/task-control/runtime/ack": 419,
            "/api/v2/task-control/connections": 404,
            "/api/v1/widget/task-control/source-requests/not-a-uuid/result": 404,
            "/api/v1/widget/task-control/source-requests/12345678-1234-1234-1234-123456789abc/result/extra": 404,
            "/internal/v1/task-control/accounts/123/installation": 404,
            "/admin/v1/installations/123/task-control/diagnostics": 404,
        }
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        for route, expected in routes.items():
            for attempt in range(20):
                try:
                    response = opener.open(f"http://127.0.0.1:{port}{route}", timeout=3)
                    actual = response.status
                    response.close()
                except urllib.error.HTTPError as error:
                    actual = error.code
                    error.close()
                except urllib.error.URLError:
                    if attempt == 19:
                        raise
                    time.sleep(0.1)
                    continue
                break
            assert actual == expected, (route, actual, expected)
        print(json.dumps({"kind": "isolated-proxy-routing", "routes": len(routes), "result": "passed"}))
    finally:
        subprocess.run(["docker", "rm", "-fv", name], stdout=subprocess.DEVNULL, check=False)
