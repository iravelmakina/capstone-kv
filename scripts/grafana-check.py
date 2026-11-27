#!/usr/bin/env python3
"""Checks Grafana's data sources and the capstone-kv dashboard (ADR-0029).

    scripts/grafana-check.py [URL] [PASSWORD_FILE]
    (defaults: http://localhost:3000, secrets/grafana_password)

Locally after `make local-up`; in AWS with `make grafana` running (SSM port forwarding).

Each data source is checked the way its plugin supports:
  - prometheus, loki: Grafana's health check (/api/datasources/uid/<uid>/health), which
    runs in the plugin's server-side part, then one real query through Grafana's proxy;
  - alertmanager: Grafana's plugin for it has no server-side part ("backend" isn't set in
    its plugin.json), so the health check API answers 500 "Plugin unavailable" for every
    Alertmanager. Its "Save & test" button checks from the browser through the proxy; this
    does the same: GET /api/v2/status through the proxy.
Then the dashboard: loaded, as many panels as the file in the repo, every panel's data
source one that exists. Exits 1 on the first failed group. The password is read from the
file and never printed.
"""
import base64
import json
import sys
import urllib.error
import urllib.request

URL = (sys.argv[1] if len(sys.argv) > 1 else "http://localhost:3000").rstrip("/")
PW_FILE = sys.argv[2] if len(sys.argv) > 2 else "secrets/grafana_password"
DASHBOARD = "monitoring/grafana/dashboards/capstone-kv.json"

auth = "Basic " + base64.b64encode(("admin:" + open(PW_FILE).read().strip()).encode()).decode()
failed = False


def get(path):
    """GET path; returns (status, parsed JSON or text)."""
    req = urllib.request.Request(URL + path, headers={"Authorization": auth})
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            status, body = r.status, r.read()
    except urllib.error.HTTPError as e:
        status, body = e.code, e.read()
    except urllib.error.URLError as e:
        sys.exit(f"FAIL Grafana not reachable at {URL}: {e.reason}")
    try:
        return status, json.loads(body)
    except ValueError:
        return status, body.decode(errors="replace")[:200]


def check(ok, what, detail=""):
    global failed
    print(("ok   " if ok else "FAIL ") + what + (f" ({detail})" if detail else ""))
    failed |= not ok


def health(uid):
    status, body = get(f"/api/datasources/uid/{uid}/health")
    return status == 200 and isinstance(body, dict) and body.get("status") == "OK", f"{status} {body.get('message', '') if isinstance(body, dict) else body}"


status, sources = get("/api/datasources")
check(status == 200, "data sources listed", f"{status}")
if status != 200:
    sys.exit(1)
uids = {s["uid"] for s in sources}

for s in sources:
    uid, kind, proxy = s["uid"], s["type"], f"/api/datasources/proxy/uid/{s['uid']}"
    if kind == "prometheus":
        ok, detail = health(uid)
        check(ok, f"{s['name']}: health check", detail)
        st, body = get(proxy + "/api/v1/query?query=up")
        n = len(body["data"]["result"]) if st == 200 and isinstance(body, dict) else 0
        up = sum(r["value"][1] == "1" for r in body["data"]["result"]) if n else 0
        check(n > 0, f"{s['name']}: query up", f"{up} of {n} targets up")
    elif kind == "loki":
        ok, detail = health(uid)
        check(ok, f"{s['name']}: health check", detail)
        st, body = get(proxy + "/loki/api/v1/label/container/values")
        names = body.get("data", []) if st == 200 and isinstance(body, dict) else []
        check(bool(names), f"{s['name']}: containers with logs", f"{len(names)}: {' '.join(sorted(names))[:120]}")
    elif kind == "alertmanager":
        # No server-side health check for this plugin (see the docstring): through the proxy.
        st, body = get(proxy + "/api/v2/status")
        version = body.get("versionInfo", {}).get("version", "?") if isinstance(body, dict) else "?"
        check(st == 200, f"{s['name']}: status through Grafana's proxy", f"{st}, Alertmanager {version}")
    else:
        ok, detail = health(uid)
        check(ok, f"{s['name']} ({kind}): health check", detail)

want = json.load(open(DASHBOARD))
st, body = get(f"/api/dashboards/uid/{want['uid']}")
panels = body.get("dashboard", {}).get("panels", []) if st == 200 and isinstance(body, dict) else []
check(len(panels) == len(want["panels"]), f"dashboard {want['uid']}: panels", f"{len(panels)} of {len(want['panels'])}")
missing = sorted({p["datasource"]["uid"] for p in panels
                  if isinstance(p.get("datasource"), dict) and p["datasource"].get("uid") not in uids | {"-- Grafana --", "-- Mixed --", "-- Dashboard --"}})
check(not missing, "every panel's data source exists", "missing: " + ", ".join(missing) if missing else "")

print("grafana check " + ("FAILED" if failed else "passed") + f" against {URL}")
sys.exit(1 if failed else 0)
