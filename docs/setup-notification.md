# Notification system setup guide

For whoever deploys the system: what to prepare, in which order, and how to check it. The design and the settings of each
feature are in the [courier README](../README.md); this page puts them together as one procedure.

## 1. Overview

```
service (identity, bvchain, ...) --gRPC Notify(user_ids | recipients, event_type, data)--> courier ──► PostgreSQL (courier_*)
                                                                                              │
web (bell, Settings, Admin) ──► gatekeeper /api/notifications/* (REST + SSE) ──► courier      └─► Redis Streams ─► courier-worker ─► email | Telegram | webhook | Web Push
```

| Component | Role |
| --- | --- |
| `courier` | gRPC API: `Notify`, inbox, preferences, admin. Writes the state to PostgreSQL and queues the work in Redis. It sends nothing itself. |
| `courier-worker` | Takes the work from Redis, renders the template, sends, records the result. It also runs the relay (outbox, retry), the digest, the Telegram bot listener and the cleanup of old data. |
| `db-migration` | Creates the `courier_*` tables (migrations V41–V50). Courier does **not** create tables itself. |
| `identity` | Lets courier look up users (email, language), issues the tokens of the bell and Settings, receives the `courier:*` permissions. |
| `gatekeeper` | HTTP gateway of the web apps: the `/api/notifications/*` and `/api/courier/*` routes, including the SSE stream. |
| web (`portal`, `admin`, `bill`, `dict`) | The bell in every app; Settings → Notifications in the portal; the admin pages in admin. |

Principle: PostgreSQL is the source of truth and Redis is only a queue. A calling service says "which event, for whom"; courier picks
the channels, addresses and language from the preferences of the user.

## 2. Rollout checklist

Do the steps in order; each one needs the one before.

1. **Release protos** with the new `courier/v1` (`Notify`, inbox, preferences, admin, `permissions.proto`), then raise `protogen` in the `go.mod` of courier, identity, bvchain and gatekeeper (and rebuild the gatekeeper `descriptors.binpb`: `make descriptors`).
2. **Secrets** (section 3): `courier-client`, `courier-vapid`, the Telegram bot.
3. **db-migration**: release and sync, which runs V41 → V50 (section 4).
4. **identity**: new chart and the `setup:` block that creates the `courier` client and the exchange policies (section 5). Wait for the `setup` Job to succeed.
5. **courier** and **courier-worker**: bump `targetRevision`, check the env (section 6). On start courier registers its permissions with identity (retries for up to 2 minutes).
6. **gatekeeper**: bump `targetRevision` (the new routes are in the chart). Check the ingress (section 7).
7. **web**: `portal`, `admin`, `bill`, `dict` (the bell and the new pages).
8. **Grant the `courier:*` permissions** to roles in the admin app (section 8).
9. **bvchain**: switch to `BVCHAIN_NOTIFY_USER_ID` (section 9). Optional, do it last.
10. **Monitoring** (section 11) and the checks of section 10.

Rollback: every step only adds (new tables, new RPCs) and the old `SendNotification` still works. Going back to the previous courier/worker
is safe because the old tables do not change; deliveries created by the new version (channels `webhook`, `push`) fail on the old one.

## 3. Secrets and keys

Secrets live in the `mm-ansible` vault (never in `mm-deploy`). See "Identity and gatekeeper secrets" in the mm-ansible README.

| Secret (namespace `application`) | Key | Value | Used by |
| --- | --- | --- | --- |
| `courier-client` | `secret` | random string (`make identity-secrets` makes one) | courier `IDENTITY_CLIENT_SECRET`; the identity setup Job (`COURIER_CLIENT_SECRET`) |
| `courier-vapid` | `private-key` | VAPID private key | courier-worker `VAPID_PRIVATE_KEY` |
| `postgres-data-rw-credentials` | `password` | already there | courier and worker `POSTGRES_PASSWORD` |
| `telegram-...-credentials` | `bot-token` | already there | worker `TELEGRAM_BOT_TOKEN` |

Two places in `mm-ansible` must know each Secret, or it is never created in the cluster:

1. the **value** in the vault (`k8s_secrets_vault.yml`), as `application-courier-client:` / `application-courier-vapid:` under `vault_k8s_secrets`;
2. the **entry** in the list `k8s_secrets.yml` (a Secret that is not listed is not touched):

   ```yaml
   - name: courier-client
     namespace: application
   - name: courier-vapid
     namespace: application
   ```

Put the vault value in first (a listed Secret without a vault key makes the `secrets` run fail), then `make deploy TAGS=secrets`; the pods must restart (Reloader does it).

### VAPID key (Web Push)

VAPID is the key pair the worker uses to prove to the browser push services (FCM, Mozilla, Apple) that it is the server the subscription was made for.

For a cluster: run `make vapid-keys` in `mm-ansible` (on macOS it copies the YAML block to the clipboard; the first comment line holds `VAPID_PUBLIC_KEY`), then paste the block into the vault with `make vault-edit F=k8s_secrets_vault.yml`. Do it once.

Without mm-ansible:

```bash
cd courier && go run ./cmd/courierctl vapid
# VAPID_PUBLIC_KEY=...    -> public value: set it in the courier AND courier-worker Applications (not a secret)
# VAPID_PRIVATE_KEY=...   -> secret courier-vapid / private-key
```

Changing the pair makes every existing subscription useless (browsers must subscribe again). `VAPID_SUBJECT` (worker) is the `mailto:` contact of the push service.

### Telegram bot

1. Create the bot with @BotFather and take the **token** (the worker already uses it) and the **username** (without `@`).
2. Set `TELEGRAM_BOT_USERNAME` on **courier** (the "Link Telegram" button in the portal builds `https://t.me/<bot>?start=<code>` with it).
3. The worker listens to the bot with `getUpdates`, so **the bot must not have a webhook**. If it ever had one: `https://api.telegram.org/bot<TOKEN>/deleteWebhook`.
4. Only one worker listens to the bot at a time (a Postgres advisory lock); turn it off with `TELEGRAM_POLL=false`.

## 4. Migrations (repo `db-migration`)

| Version | Content |
| --- | --- |
| V41 | `courier_events`, `courier_deliveries` (status, retry, dead letter, outbox) |
| V42 | recipient columns on the delivery (`user_id`, `locale`, `vars`) |
| V43 | `courier_inbox` (the bell) |
| V44 | user preferences, quiet hours, Telegram bindings, link codes; status `skipped` |
| V45 | templates edited in admin (+ version history), broadcasts |
| V46 | webhooks, channel `webhook` |
| V47 | Web Push subscriptions, channel `push` |
| V48 | digest (merging notifications) |
| V49 | identity role `user_directory` (permission `identity:user:read`) |
| V50 | channels used by each broadcast (`courier_broadcasts.channels`) |

Try it locally: `docker compose up --build` in `db-migration`. Tables follow the `courier_*` plural naming.

## 5. identity: the `courier` client

Courier needs a client in identity to (a) look users up with `GetUser`/`ListUsers`, (b) register the `courier:*` permissions on start. The `setup:` block of
`applications/<env>/identity.yaml` (already updated in mm-deploy) adds to `SETUP_CONFIG`:

```json
{"id":"courier","type":"confidential","resource":"courier","secret_env":"COURIER_CLIENT_SECRET",
 "roles":["service_registrar","user_directory"],"grants":["client_credentials"]}
```
and two exchange policies:

| Policy | What it is for |
| --- | --- |
| `gatekeeper → courier` (depth 1) | gatekeeper exchanges the user's token for one with audience `courier` (bell, Settings, admin) |
| `courier → identity` (depth 1) | courier gets a service token to call identity |

The `setup` Job only **adds** (idempotent). After it ran and courier registered its permissions, remove the `setup:` block from the Application as identity
always requires. The `courier-client` secret must exist before the Job runs.

Check: in the admin app → Roles & Permissions you must see 7 `courier:*` permissions (after courier started).

## 6. courier and courier-worker settings

`mm-deploy/applications/{dev,staging,production}/courier.yaml` and `courier-worker.yaml` already hold the whole config block; what needs a real value is left empty.

**courier**

| Variable | Example / meaning |
| --- | --- |
| `POSTGRES_HOST/DB/USER` + secret `POSTGRES_PASSWORD` | the shared `data` database |
| `REDIS_HOST/PORT` + secret `REDIS_PASSWORD` | already there |
| `COURIER_AUTH_ALLOWED` | ServiceAccounts allowed to call gRPC (identity, bvchain, bvreport), already there |
| `COURIER_ISSUER` | issuer of identity (`https://auth.<env>…`) |
| `COURIER_JWKS_URL` | `http://identity.application.svc.cluster.local:8080/.well-known/jwks.json` |
| `IDENTITY_TARGET` | `identity.application.svc.cluster.local:9090` |
| `IDENTITY_INSECURE` | `true` by default (plaintext gRPC). Set `false` once identity serves TLS (see section 14); the CA to trust is `GRPC_TLS_CA_FILE` |
| `IDENTITY_TOKEN_URL` | `http://identity.application.svc.cluster.local:8080/oauth2/token` |
| `IDENTITY_CLIENT_ID` + secret `IDENTITY_CLIENT_SECRET` | `courier` + `courier-client` |
| `TELEGRAM_BOT_USERNAME` | bot username; empty hides the Telegram section |
| `VAPID_PUBLIC_KEY` | public key; empty hides the Web Push section |
| optional | `USER_CACHE_TTL` (60s), `TEMPLATE_RELOAD` (30s), `METRICS_ADDR` (`:9100`), `PERMISSIONS_SYNC_TIMEOUT` (2m) |

**courier-worker**

| Variable | Meaning |
| --- | --- |
| `POSTGRES_*`, `REDIS_*` | as above |
| `SMTP_*`, `MAIL_BRAND`, `MAIL_LOGO_URL`, `TELEGRAM_BOT_TOKEN` | already there |
| `VAPID_SUBJECT`, `VAPID_PUBLIC_KEY`, secret `VAPID_PRIVATE_KEY` | Web Push; if missing the worker only warns and sends no push |
| optional | `QUEUE_MAX_RETRY` (5), `QUEUE_RELAY_INTERVAL` (5s), `QUEUE_RELAY_STALE` (10m), `RETENTION_DAYS` (90), `INBOX_RETENTION_DAYS` (365), `TELEGRAM_POLL` (true) |

The worker scales out (production has autoscaling): the relay uses `FOR UPDATE SKIP LOCKED` and only one worker listens to the Telegram bot. Do **not** set
`WEBHOOK_ALLOW_PRIVATE=true` outside local: it lets webhooks and push endpoints point at internal addresses.

## 7. gatekeeper, ingress and web

- The routes are in the gatekeeper chart (`routes.yaml`); the `courier` upstream defaults to `courier.application.svc.cluster.local:9090` (see `charts/gatekeeper/values.yaml`). No Application change is needed if the courier service keeps that name.
- **SSE** (`/api/notifications/stream`): the default nginx ingress `proxy-read-timeout` of 60s is longer than the 25s heartbeat, so it works; the gateway already sends `X-Accel-Buffering: no`. If the ingress timeout was lowered below 30s, raise it. The stream closes by itself after 10 minutes and the browser reconnects (which checks the session again).
- **portal**: serves `/sw.js` (the Web Push service worker) without long caching. The CSP `script-src 'self'` needs no change.
- Web Push only works over **https** (or localhost).

## 8. Permissions for administrators

In the admin app → Roles & Permissions, grant these to the right roles (admin has `*`, so it is already covered):

| Permission | Opens |
| --- | --- |
| `courier:template:read` / `:write` | the *Templates* page (view, edit, preview, revert) |
| `courier:broadcast:send` | the *Announcements* page |
| `courier:delivery:read` | the *Deliveries* page |
| `courier:notification:manage` | the Retry button in the delivery log |
| `courier:webhook:manage` | the *Webhooks* page (a Slack URL is a secret: administrators only) |

Regular users need **no** permission to use the bell and Settings → Notifications.

## 9. bvchain

bvchain calls `Notify` with `bvchain.*` events. Two ways to pick the recipient (one of them is required):

- `TELEGRAM_CHAT_ID`: a fixed chat (as before).
- `BVCHAIN_NOTIFY_USER_ID`: an identity user id; the message goes to the Telegram chat that user linked, following their preferences. It wins over `TELEGRAM_CHAT_ID`.

Switching: (1) the user signs in to the portal → Settings → Notifications → "Link Telegram" → presses Start in the bot; (2) set `BVCHAIN_NOTIFY_USER_ID` (a commented example is in `bvchain.yaml`); (3) check the message reaches the right chat;
(4) only then remove `TELEGRAM_CHAT_ID`. A user who has not linked Telegram does not make the job fail; it only logs "courier skipped the user".

## 10. Checks after the rollout

| # | Check | How | Expected |
| --- | --- | --- | --- |
| 1 | Courier is up and registered its permissions | courier log: `permissions registered`; the admin app shows `courier:*` | no `permission sync` error |
| 2 | Email through a user | change the password of a test user | the email arrives, a `sent` row in *Deliveries* |
| 3 | The bell | send `security.mfa_changed` (turn MFA on/off) | the bell count rises within ~2 seconds in portal/admin/bill/dict |
| 4 | Read in one app, the other updates | click "Mark all read" | the bell in the other tab/app goes to 0 |
| 5 | Preferences | Settings → turn off the email of the "Administration" group, then create a new sign-up | delivery `skipped`, still in the bell |
| 6 | Telegram | link with the code | status "Linked"; `bvchain` reaches the chat |
| 7 | Web Push | Settings → "Turn on for this device", then broadcast | an OS notification shows even with the portal closed |
| 8 | Webhook | admin → Webhooks → Add → Test | status `HTTP 200`; the Slack message shows up |
| 9 | Template | edit a template, Preview, Save | the next email/message uses the new version within 30 seconds |
| 10 | Digest | create 3 sign-ups in a row | the admin gets **1** "3 sign-ups…" email after ~10 minutes; the bell has 3 items |
| 11 | Retry/dead | temporarily set a wrong `SMTP_HOST` and send | the delivery goes pending → retry → dead; fix it and press Retry |
| 12 | Metrics | `curl <pod>:9100/metrics \| grep courier_` | `courier_delivery_attempts_total`, `courier_deliveries_pending` are there |

Command line tool (in the `courier` directory; needs `GRPC_TARGET` (default `localhost:9090` is identity, not courier), `GRPC_INSECURE=true`, a token if any):

```bash
export GRPC_TARGET=localhost:9091                                                   # local courier
go run ./cmd/courierctl -event security.password_changed -users <user-id>          # Notify by user
go run ./cmd/courierctl -event bvchain.failed -channel telegram -address <chat> -data step=login -data error=boom
go run ./cmd/courierctl -get <notification_id>                                      # see the deliveries
go run ./cmd/courierctl -retry <delivery_id>                                        # send a failed/dead delivery again
go run ./cmd/courierctl vapid                                                       # make a VAPID key pair
```

## 11. Monitoring

- Courier and the worker serve `/metrics` on `METRICS_ADDR` (`:9100`); the pods already carry the `prometheus.io/scrape` annotation.
- Sample alerts: `courier/docs/prometheus-rules.yaml` (`dead` deliveries, failure ratio, stuck queue, stream backlog, dependency down, `Notify` errors). The `PrometheusRule` kind needs the Prometheus operator; with a plain Prometheus copy `spec.groups` into a rule file.
- Grafana dashboard: import `courier/docs/grafana-dashboard.json`.
- The two most important signals: `courier_oldest_due_delivery_age_seconds` (is the worker sending?) and `increase(courier_delivery_attempts_total{outcome="dead"}[15m])` (is someone not getting their messages?).

## 12. Running locally

`scripts/local.sh up` (see `LOCAL.md`) starts Postgres, Redis, identity, gatekeeper, courier, the worker and the web apps; emails are written to `.local/mails`.
The script sets up the `courier-dev` client (roles `service_registrar`, `user_directory`; policies `courier-dev → identity` and `gatekeeper → courier`), so courier signs in to identity with client credentials like in production and registers the `courier:*` permissions. It also sets `COURIER_ISSUER/JWKS`, the metrics ports 9101/9102 and `WEBHOOK_ALLOW_PRIVATE=true`.
The local admin sign-in (`admin@example.com`) has `*`, so every admin page works.

Web Push works out of the box locally: `scripts/local.sh` carries a fixed VAPID key pair (for dev machines only; set `VAPID_*` in the environment to use your own). To try Telegram, set `TELEGRAM_BOT_TOKEN` and `TELEGRAM_BOT_USERNAME` before running the script.

Running the tests:

```bash
cd courier && COURIER_TEST_POSTGRES_URL='postgres://postgres:postgres@localhost:5432/app?sslmode=disable' \
  COURIER_TEST_REDIS_URL='redis://localhost:6379/9' go test ./...      # without the two variables the integration tests are skipped
cd gatekeeper && go test ./...      cd identity && go test ./...      cd bvchain && go test ./...
cd web && pnpm run ci
```

## 13. Troubleshooting

| Symptom | Usual cause | Fix |
| --- | --- | --- |
| `permission registration failed ... tls: first record does not look like a TLS handshake` | courier dials identity with TLS (`IDENTITY_INSECURE=false`) but identity speaks plaintext gRPC | set `IDENTITY_INSECURE` back to `true`, or enable TLS on identity (section 14). The inverse (plaintext client, TLS server) shows the same text in the **server** log |
| `x509: certificate signed by unknown authority` | the client does not trust the CA of the server certificate | mount the CA and set `GRPC_TLS_CA_FILE` (section 14); the same CA must have signed the server |
| `x509: certificate is valid for a, b, not c` | the name the client dials is not in the certificate | dial `<svc>.application.svc(.cluster.local)`, add the name to `dnsNames` of the `Certificate`, or set `GRPC_TLS_SERVER_NAME` |
| courier exits after ~2 minutes with `permission sync` | the `courier` client is missing, lacks the `service_registrar` role, or has a wrong secret | run the identity setup Job; check `courier-client` |
| every identity mail fails with `user_resolution_disabled` / `identity_unavailable` | no `IDENTITY_TARGET`/client credentials, or the `user_directory` role / `courier → identity` policy is missing | see sections 5–6 |
| the bell is empty, `401/403` from `/api/notifications` | the `gatekeeper → courier` policy is missing, or `COURIER_ISSUER/JWKS_URL` is empty | see sections 5–6 |
| the bell does not jump at once (updates only after 60s) | SSE is cut or buffered by the ingress; Redis is down | check the ingress timeout and `courier_dependency_up{dependency="redis"}` |
| the Telegram button does not show | `TELEGRAM_BOT_USERNAME` is empty | set it on courier |
| pressing Start in the bot does not link | the bot has a webhook; the worker does not hold the lock; the code expired (15 minutes) | `deleteWebhook`; see the worker log `listening to the Telegram bot` |
| the Web Push section does not show | `VAPID_PUBLIC_KEY` is empty, not https, or the browser does not support it | set the key; use https |
| the button spins when turning push on | the browser is waiting for the notification permission prompt | click Allow in the address bar (or set Notifications to Allow for the site) |
| push is `sent` but nothing appears | the OS blocks the browser's notifications (macOS: System Settings → Notifications → Chrome; Focus mode) | allow them; `registration.getNotifications()` in the console shows whether the browser displayed it |
| push does not arrive and there is no error | the worker lacks `VAPID_PRIVATE_KEY`; the browser blocks notifications | the worker log warns about VAPID; allow notifications for the site |
| webhook `failed: ... is not a public address` | the URL points at an internal IP | use a public URL (only local may set `WEBHOOK_ALLOW_PRIVATE`) |
| Slack webhook `failed: webhook answered 404/410` | the Slack URL was revoked | create the webhook again in Slack and edit the URL |
| a delivery stays `pending`, `courier_oldest_due_delivery_age_seconds` grows | the worker is not running / Redis is broken | check the worker pod; the relay re-queues after `QUEUE_RELAY_STALE` |
| the merged email does not arrive after the window | the worker does not run the relay; `next_attempt_at` is far away (quiet hours) | check the worker log; look at the delivery in the log |
| a template was edited but messages still use the old one | `TEMPLATE_RELOAD` (30s) has not passed yet | wait; check the log `templates: reload failed` |
| delivery `retry` with `channel push is not available on this worker` | the worker has no `VAPID_*` (or runs an old version during a rolling deploy) | set the key; the delivery is retried by backoff |
| `link_invalid` when calling `Notify` | `data.link` is not a `/...` path or an http(s) URL | fix the caller |

## 14. TLS for in-cluster gRPC

gRPC between the services is plaintext by default. TLS is **server-side only** (the server presents a certificate; no client certificate): callers are still authenticated by their ServiceAccount token, JWT or client credentials. It is decided per hop, so it can be switched on one hop at a time. The code is `gocommon/tlsconfig` (see the gocommon README).

| Hop | Server (certificate) | Client switch |
| --- | --- | --- |
| courier → identity | identity | courier `IDENTITY_INSECURE=false` |
| dictionary → identity | identity | dictionary `DICTIONARY_IDENTITY_INSECURE=false` |
| bvchain, bvreport → courier | courier | `COURIER_INSECURE=false` |
| identity → courier (notify) | courier | identity `COURIER_INSECURE=false` |
| gatekeeper → identity / courier / dictionary | each | `GATEKEEPER_UPSTREAM_INSECURE=false` (or `insecure: false` of one upstream in `routes.yaml`) |

**Prerequisite (mm-ansible).** cert-manager (add-on of the `microk8s` role) and the internal CA of that role (`internal_issuer_name`, default `internal-ca`: a `selfSigned` issuer, the CA `Certificate` in `cert-manager`, a `ClusterIssuer` of type `ca`). The ACME issuer cannot sign `*.svc` names. Run `make deploy TAGS=microk8s` once per cluster, then `kubectl get clusterissuer internal-ca` must be `Ready`.

**Certificates (mm-deploy).** `infra-services/<env>/certificates/{identity,courier,dictionary}.yaml`: one `Certificate` each, `issuerRef: internal-ca`, `dnsNames: <svc>.application.svc[.cluster.local]`, Secret `<svc>-grpc-tls` with `tls.crt`, `tls.key`, `ca.crt`. The root app `infra-<env>` of mm-ansible syncs `infra-services/<env>` recursively (`recurse: true`); the manifests name `namespace: application` themselves, so they land there although that root's default namespace is `infrastructure`. cert-manager renews the Secret in place; the servers reload the files without a restart.

**Mounts (charts).** The charts mount the Secret read-only at `/var/run/tls` (`optional: true`, so a pod starts before the certificate exists) and set `GRPC_TLS_CA_FILE=/var/run/tls/ca.crt`: identity, courier and dictionary mount their own Secret; gatekeeper mounts `ca.crt` of `identity-grpc-tls` and bvchain/bvreport `ca.crt` of `courier-grpc-tls`. With one internal CA every `ca.crt` is the same. The lists live in the charts, not in the Application values, because Helm replaces a list instead of merging it: `deployment.volumes` in mm-deploy would drop the chart's own volumes (identity signing key, gatekeeper routes). The gRPC probes are `tcpSocket`/HTTP, which work unchanged with TLS.

**Switch one hop on** (start with courier → identity, the least traffic), in the *same* sync of mm-deploy:

1. server Application: `GRPC_TLS_CERT_FILE: /var/run/tls/tls.crt` and `GRPC_TLS_KEY_FILE: /var/run/tls/tls.key` (identity here);
2. client Application: its `*_INSECURE` variable `"false"`.

A gRPC port cannot serve both modes, so expect a short burst of `Unavailable` that the retries of the clients absorb. Because a server serves TLS to *all* its callers, switch every client of that server in the same sync (identity serves courier, dictionary and gatekeeper). Verify: the courier log shows `permissions registered` and no `Unavailable`; then the next hop. Rollback of a hop: the client variable back to `true` and remove the two server variables. Renew test: `cmctl renew identity-grpc-tls -n application`; the next handshake serves the new certificate with no restart.

**Not covered** (stay HTTP in the cluster): the JWKS and token URLs (`COURIER_JWKS_URL`, `DICTIONARY_JWKS_URL`, `IDENTITY_TOKEN_URL`, `DICTIONARY_TOKEN_URL`) and the back-channel logout of identity to gatekeeper. They are a separate change.

**Locally.** `scripts/local.sh` stays plaintext. `TLS=1 scripts/local.sh up backend` generates a throwaway CA and a certificate for `localhost` in `.local/tls`, serves every gRPC port with it and turns every `*_INSECURE` switch off; `TLS=1 python3 courier/scripts/smoke-local.py` then runs the whole stack over TLS. `TLS=1 TLS_WRONG_CA=1` gives the services another CA, so every hop fails with `unknown authority`.
