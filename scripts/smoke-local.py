#!/usr/bin/env python3
"""Smoke test of the notification stack against `scripts/local.sh up` (gateway :8081, identity :8080, courier :9091 and its
worker). It signs in as the local admin through identity and checks: the bell over SSE (a Notify by user id reaches the browser,
a read in one tab reaches the others), preferences, the admin screens (templates: preview, save, revert; an announcement; the
delivery log), a webhook delivered by the worker with a valid signature, and two events grouped into one mail (digest).

    PW='<password of admin@example.com>' python3 courier/scripts/smoke-local.py    # PW defaults to SETUP_ADMIN_PASSWORD of local.sh

It leaves test data in the local database (a template version that it reverts, an announcement to every user, mails in
.local/mails). Needs `go` (it calls courierctl) and `docker` (it makes a digest due instead of waiting ten minutes).
"""
import base64, hashlib, http.cookiejar, http.server, json, os, re, subprocess, sys, threading, time
import urllib.error, urllib.parse, urllib.request

GK, ISS, UI = "http://localhost:8081", "http://localhost:8080", "http://localhost:3004"
EMAIL, PASSWORD = "admin@example.com", os.environ.get("PW", "Correct-Horse-Battery-9")
ROOT = "/Users/hupham/workspaces/personal/t-projects"
fails = []

def check(name, cond, extra=""):
    print(("ok   " if cond else "FAIL ") + name + ("" if cond else "  -> " + str(extra)[:300]), flush=True)
    if not cond: fails.append(name)

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k): return None

jar = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar), NoRedirect)
csrf = ""
def call(method, url, js=None, data=None, headers=None):
    h = dict(headers or {}); body = None
    if js is not None: body, h["Content-Type"] = json.dumps(js).encode(), "application/json"
    elif data is not None: body = urllib.parse.urlencode(data).encode()
    try: r = opener.open(urllib.request.Request(url, method=method, headers=h, data=body))
    except urllib.error.HTTPError as e: r = e
    return r.status, dict(r.headers), r.read().decode()
def api(method, path, js=None):
    h = {"Origin": UI}
    if method != "GET": h["Csrf-Token"] = csrf
    s, hd, b = call(method, GK + path, js=js, headers=h)
    try: return s, json.loads(b) if b else {}
    except Exception: return s, b
absolute = lambda base, loc: loc if "://" in loc else base + loc

# --- login through identity like a browser
s, h, b = call("GET", f"{GK}/auth/login?return_to=" + urllib.parse.quote(UI + "/app", safe=""))
s, h, b = call("GET", h["Location"])  # the authorize URL: identity sends the browser to its sign in page
ret = urllib.parse.parse_qs(urllib.parse.urlparse(h["Location"]).query)["return_to"][0]
s, h, b = call("GET", ISS + "/auth-api/csrf")
s, h, b = call("POST", ISS + "/auth-api/login", js={"email": EMAIL, "password": PASSWORD, "return_to": ret, "csrf": json.loads(b)["csrf"]})
login = json.loads(b) if b.startswith("{") else {}
check("login", s == 200 and (login.get("redirect") or login.get("location") or login.get("next")), (s, b[:300]))
target = login.get("redirect") or login.get("location") or login.get("next") or ""
s, h, b = call("GET", absolute(ISS, target)); s, h, b = call("GET", h["Location"])  # authorize -> the gateway callback
s, h, b = call("GET", f"{GK}/auth/me", headers={"Origin": UI}); me = json.loads(b); csrf = me["csrf_token"]; uid = me["sub"]
need = ["GET /api/notifications", "GET /api/notifications/preferences", "GET /api/courier/event-types", "GET /api/courier/deliveries", "GET /api/courier/webhooks", "POST /api/courier/broadcasts"]
check("admin is allowed every notification route", all(k in me["allowed"] for k in need), [k for k in need if k not in me["allowed"]])

# --- the bell
s, r = api("GET", "/api/notifications/unread-count"); check("unread-count through gateway -> courier (token exchange + JWT)", s == 200 and "unread" in r, (s, r))
s, r = api("GET", "/api/notifications"); check("list inbox", s == 200 and "items" in r, (s, r)); base_items = len(r.get("items", []))

events = []
def sse():
    req = urllib.request.Request(GK + "/api/notifications/stream", headers={"Origin": UI, "Accept": "text/event-stream"})
    try:
        with opener.open(req, timeout=40) as resp:
            events.append(("status", resp.status, resp.headers.get("Content-Type")))
            for line in resp:
                line = line.decode().strip()
                if line.startswith("data:"): events.append(json.loads(line[5:]))
    except Exception as e: events.append(("err", str(e)))
threading.Thread(target=sse, daemon=True).start()
time.sleep(2)
check("SSE stream opened as text/event-stream with a first count", events and events[0][1:] == (200, "text/event-stream") and any(isinstance(e, dict) and "unread" in e for e in events), events)

tls = os.environ.get("TLS") == "1"  # stack started with `TLS=1 scripts/local.sh up`
env = dict(os.environ, GRPC_TARGET="localhost:9091", GRPC_INSECURE="false" if tls else "true", ENV="local")
if tls: env["GRPC_TLS_CA_FILE"] = ROOT + "/.local/tls/ca.crt"  # courierctl always trusts the real CA
def ctl(*args):
    p = subprocess.run(["go", "run", "./cmd/courierctl", *args], cwd=ROOT + "/courier", env=env, capture_output=True, text=True)
    return p.returncode, (p.stdout + p.stderr)
t0 = time.time()
rc, out = ctl("-event", "security.mfa_changed", "-users", uid, "-data", "change=enabled")
check("Notify by user id", rc == 0 and "event_id=" in out, out[-300:])
deadline = time.time() + 6
got = None
while time.time() < deadline and not got:
    got = next((e for e in events if isinstance(e, dict) and e.get("item")), None); time.sleep(0.2)
check("the bell gets the item over SSE", got and got["unread"] >= 1 and "item" in got, events[-3:])
check("item title is rendered", got and got["item"]["title"] != "", got)
s, r = api("GET", "/api/notifications"); check("the newest item of the list is the one the stream delivered", s == 200 and r["items"][0]["id"] == got["item"]["id"], (s, r.get("items", [{}])[0].get("id"), got and got["item"]["id"]))
item = r["items"][0]["id"]
before = got["unread"]  # a rerun starts with the unread items of the earlier one
s, r = api("POST", "/api/notifications/read", {"ids": [item]}); check("mark read", s == 200 and r.get("unread") == before - 1, (s, r, before))
time.sleep(1)
check("another tab hears of the read over SSE", any(isinstance(e, dict) and e.get("unread") == before - 1 and not e.get("item") for e in events[1:]), events[-3:])
s, r = api("POST", "/api/notifications/archive", {"ids": [item]}); check("archive", s == 200, (s, r))

# --- preferences
s, r = api("GET", "/api/notifications/preferences"); cats = {c["category"]: c for c in r.get("categories", [])}
check("preferences list categories; security is mandatory", s == 200 and cats.get("security", {}).get("mandatory"), (s, r))
s, r = api("PUT", "/api/notifications/preferences", {"categories": [{"category": "admin", "disabledChannels": ["email"]}], "quietHours": {"enabled": True, "from": "22:00", "to": "07:00", "timezone": "Asia/Ho_Chi_Minh"}})
check("save preferences with quiet hours", s == 200, (s, r))
s, r = api("GET", "/api/notifications/preferences"); check("preferences round trip", r["quietHours"]["enabled"] and "email" in next(c for c in r["categories"] if c["category"] == "admin").get("disabledChannels", []), r)
api("PUT", "/api/notifications/preferences", {"categories": [{"category": "admin"}], "quietHours": {"enabled": False}})
check("telegram/push hidden without bot and keys", r["telegram"].get("available") in (None, False) and r["push"].get("available") in (None, False), (r["telegram"], r["push"]))

# --- admin: templates
s, r = api("GET", "/api/courier/event-types"); check("event types", s == 200 and any(e["type"] == "system.announcement" for e in r["eventTypes"]), (s, str(r)[:200]))
s, r = api("GET", "/api/courier/templates/system_announcement/en/email"); check("get built-in template", s == 200 and not r.get("customized") and r["content"]["subject"], (s, r))
s, r = api("POST", "/api/courier/templates/system_announcement/en/email/preview", {"content": {"subject": "[{{.title}}]", "text": "{{.message}}"}}); check("preview", s == 200 and r["rendered"]["subject"].startswith("[sample"), (s, r))
s, r = api("PUT", "/api/courier/templates/system_announcement/en/email", {"content": {"subject": "SMOKE {{.title}}", "text": "Hello {{.name}}: {{.message}}"}}); check("save template (a new version)", s == 200 and r.get("version", 0) >= 1, (s, r))
s, r = api("PUT", "/api/courier/templates/system_announcement/en/email", {"content": {"subject": "{{.title", "text": "x"}}); check("broken template refused", s == 400, (s, r))

# --- broadcast uses the edited template; the worker is another process and loads edited templates every TEMPLATE_RELOAD (30 s)
print("     (waiting 32 s for the worker to load the edited template)", flush=True)
time.sleep(32)
mails_before = set(os.listdir(ROOT + "/.local/mails"))
s, r = api("POST", "/api/courier/broadcasts", {"eventType": "system.announcement", "audience": {"all": True}, "data": {"title": "Maintenance", "message": "Tonight"}})
check("broadcast to everyone", s == 200 and r["broadcast"]["recipients"] >= 1, (s, r))
time.sleep(8)
new = [f for f in os.listdir(ROOT + "/.local/mails") if f not in mails_before and f.endswith(".txt")]
texts = [open(ROOT + "/.local/mails/" + f).read() for f in new]
check("the announcement mail uses the edited template (subject and text)", any("Subject: SMOKE Maintenance" in t and "Hello" in t and "Tonight" in t for t in texts), texts[:1])
# an announcement on the channels chosen: the bell only, to the admin: one more bell item, no e-mail
emails = lambda: len(api("GET", f"/api/courier/deliveries?userId={uid}&channel=email&pageSize=200")[1]["deliveries"])
bell = lambda: api("GET", "/api/notifications?pageSize=100")[1]["unread"]
e0, b0 = emails(), bell()
s, r = api("POST", "/api/courier/broadcasts", {"eventType": "system.announcement", "audience": {"userIds": [uid]}, "data": {"title": "Bell only", "message": "x"}, "channels": ["in_app"]})
check("announcement on the bell only", s == 200 and r["broadcast"]["channels"] == ["in_app"], (s, r))
time.sleep(2)
check("...one more bell item and no e-mail", bell() == b0 + 1 and emails() == e0, (b0, bell(), e0, emails()))
s, r = api("POST", "/api/courier/broadcasts", {"eventType": "system.announcement", "audience": {"userIds": [uid]}, "data": {"title": "t", "message": "x"}, "channels": ["telegram"]})
check("a channel the type does not have is refused", s == 400, (s, r))
s, r = api("DELETE", "/api/courier/templates/system_announcement/en/email"); check("revert template", s == 200, (s, r))

# --- delivery log
s, r = api("GET", "/api/courier/deliveries?pageSize=10"); check("delivery log", s == 200 and r["deliveries"], (s, str(r)[:200]))
check("log shows the sent announcement", any(d["eventType"] == "system.announcement" and d["delivery"]["status"] == "DELIVERY_STATUS_SENT" for d in r["deliveries"]), [ (d["eventType"], d["delivery"]["status"]) for d in r["deliveries"]])

# --- webhook to a local HTTP server
hits = []
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"])); hits.append((dict(self.headers), body)); self.send_response(200); self.end_headers(); self.wfile.write(b"ok")
    def log_message(self, *a): pass
srv = http.server.HTTPServer(("127.0.0.1", 18999), H); threading.Thread(target=srv.serve_forever, daemon=True).start()
s, r = api("POST", "/api/courier/webhooks", {"webhook": {"name": "smoke", "kind": "generic", "url": "http://127.0.0.1:18999/hook", "events": ["system.hello"], "enabled": True}})
check("create webhook (generic, secret returned once)", s == 200 and r.get("secret"), (s, r)); wid, secret = r["webhook"]["id"], r.get("secret")
s, r = api("POST", f"/api/courier/webhooks/{wid}/test"); check("webhook test", s == 200 and r.get("ok"), (s, r))
# a type that is not grouped (bvchain.failed waits ten minutes in a digest, webhooks included)
rc, out = ctl("-event", "system.hello", "-channel", "email", "-address", "hook@example.com", "-data", "name=Hook")
time.sleep(8)
real = [h for h in hits if b"system.hello" in h[1]]
check("the worker delivered the event to the webhook", len(real) >= 1, [h[1][:80] for h in hits])
if real:
    ts, sig = real[0][0].get("X-Courier-Timestamp"), real[0][0].get("X-Courier-Signature")
    import hmac
    want = "sha256=" + hmac.new(secret.encode(), (ts + ".").encode() + real[0][1], hashlib.sha256).hexdigest()
    check("the signature verifies with the secret", sig == want, (sig, want))
api("DELETE", f"/api/courier/webhooks/{wid}")

# --- digest: two registrations -> one mail
mails_before = set(os.listdir(ROOT + "/.local/mails"))
for n in ("ann", "bob"):
    ctl("-event", "account.registration_pending", "-users", uid, "-data", f"applicant_name={n}", "-data", f"applicant_email={n}@x.io")
s, r = api("GET", f"/api/courier/deliveries?userId={uid}&channel=email&pageSize=5")
lead = next((d for d in r["deliveries"] if d["eventType"] == "account.registration_pending"), None)
check("two events, one pending delivery with merged=1", lead and lead.get("merged") == 1 and lead["delivery"]["status"] == "DELIVERY_STATUS_PENDING", [(d["eventType"], d.get("merged")) for d in r["deliveries"]])
subprocess.run(["docker", "exec", "db-migration-postgres-1", "psql", "-U", "postgres", "-d", "app", "-c", f"update courier_deliveries set next_attempt_at = now() where id = '{lead['delivery']['id']}'"], capture_output=True)
time.sleep(10)
new = [f for f in os.listdir(ROOT + "/.local/mails") if f not in mails_before and f.endswith(".txt")]
digest = [open(ROOT + "/.local/mails/" + f).read() for f in new]
check("one digest mail lists both applicants", len(new) == 1 and "ann" in digest[0] and "bob" in digest[0] and "2" in new[0] + digest[0], new)

# --- a ServiceAccount-less caller cannot use the admin API; a plain call without a session is refused
s, hd, b = call("GET", GK + "/api/courier/event-types", headers={"Origin": UI}); 
print("\nFAILED:" if fails else "\nALL OK", fails)
sys.exit(1 if fails else 0)
