# courier

Notification service: a gRPC API records each notification in PostgreSQL and queues the work on Redis Streams; a worker renders locale-aware Go templates and sends email (SMTP) or Telegram, retrying with backoff and keeping the outcome of every delivery.

**Setting it up (deploy order, secrets, identity client, checks, troubleshooting, in Vietnamese): [docs/setup-notification.md](docs/setup-notification.md).**

Module: `github.com/huponx/courier`. Contracts live in `protos` (`courier/v1`); generated stubs in `protogen`. Shared libraries come from `gocommon`.

## Architecture

```
client  --gRPC-->  courier  --commit-->  PostgreSQL (events, deliveries)   <-- source of truth
                      |                       ^   ^
                      +--XADD--> Redis Streams|   | outcome, retry time
                                   |          |   |
                                   +--XREADGROUP--> worker  -->  email / Telegram
                                                    worker relay: due rows --XADD--> Redis
```

PostgreSQL is the source of truth, Redis only carries work. `SendNotification` commits the event and one delivery per recipient, then pushes the delivery ids to the stream. If that push fails (or Redis loses the message) the **relay** inside the worker pushes the rows again: a delivery is `pending` with a `next_attempt_at`, and the relay selects the due ones that were never pushed (`enqueued_at` empty) or were pushed longer than `QUEUE_RELAY_STALE` ago. A stream message carries only the delivery id; the worker takes a short lease on the row (`locked_until`), so a duplicate message or a second worker does nothing.

Delivery lifecycle (`courier_deliveries.status`):

| Status | Meaning |
| --- | --- |
| `pending` | waiting for the first send or the next retry (`next_attempt_at`) |
| `sent` | done |
| `failed` | permanent error (SMTP 5xx, Telegram 400/403, template that cannot render): not retried |
| `dead` | `QUEUE_MAX_RETRY` attempts used: the dead-letter state, `RetryDelivery` sends it again |

`courier_deliveries` also keeps `user_id` (audit link to identity), `locale` and `vars` (the name) per recipient (`V42`).

The stream only holds work not done yet: a message is deleted when it is acknowledged (a retry is a new message, pushed by the relay), an entry nobody can decode is dropped, and a worker that finds the consumer group missing (Redis was flushed) creates it again. A send is bounded by 90 seconds (the SMTP conversation by 60), and the error stored in the delivery log never contains the URL of the call (it holds the bot token or the Slack secret). A channel that is not configured on a worker (for example no VAPID keys) is retried, not failed.

A transient error schedules the next attempt after 1 min, 5 min, 30 min, then 2 h. The tables are created by `db-migration` (`V41__courier_notifications.sql`), not by courier. Once every delivery of an event is sent its `template_data` is deleted from the database (reset links and codes must not stay there); events older than `RETENTION_DAYS` are purged by the relay.

| Process | Role |
| --- | --- |
| `cmd/courier` | gRPC only (`SendNotification`, `GetNotification`, `RetryDelivery`). Validates locale/template, writes the event and one delivery per recipient, then pushes them to Redis. |
| `cmd/worker` | Consumes the stream, renders templates, sends, records the outcome; runs the relay (outbox, retries) and the retention purge. HTTP is only Kubernetes probes (`/healthz`, `/readyz`). |
| `cmd/courierctl` | Optional local CLI to call the API (`grpcclient`): send, `-get <notification_id>`, `-retry <delivery_id>`. Not deployed. |

Courier does not send messages. The worker does not expose gRPC.

## API

`CourierService.SendNotification` (see `courier/v1` in the protos repo):

- `locale` — `vi`, `en`, …
- `template_id` — directory name under `internal/notify/templates/{locale}/`
- `template_data` — `map<string,string>` passed into templates as `{{.name}}`
- `recipients` — `channel` (`EMAIL` = 10, `TELEGRAM` = 20) + `address` (email or Telegram `chat_id`)
- `idempotency_key` — optional; unique in `courier_events` for as long as the event is kept (`RETENTION_DAYS`). A repeat returns the same `notification_id` with `queued` = 0 and sends nothing again.

The RPC returns after the commit (`notification_id`, `queued` = number of deliveries); the send itself is asynchronous.

`GetNotification(notification_id)` returns every delivery with `status`, `attempts`, `last_error`, `next_attempt_at`, `sent_at`. `RetryDelivery(delivery_id)` puts a `failed` or `dead` delivery back in the queue with a fresh attempt count (`FAILED_PRECONDITION` for any other status). Both are service-to-service calls under the same auth as `SendNotification`.

 Field numbers and enum values use multiples of 10 (`UNSPECIFIED` = 0).

## Notify (events and users)

`CourierService.Notify` is the call for the services of the system: it says **what happened and to whom**, courier decides channels, address, language and template.

- `event_type` — a type of the catalog [`internal/catalog/event_types.yaml`](internal/catalog/event_types.yaml), for example `security.password_reset`. Unknown types are rejected (`event_type_unknown`).
- `data` (`google.protobuf.Struct`) — validated against the type: every `required` key present, no key outside `required` + `optional` (+ `name`, `brand`, `logo_url`). A typo is `INVALID_ARGUMENT` (`data_invalid`) listing what is missing or unknown, instead of an empty hole in a mail. Numbers and booleans are formatted as text; lists and objects are refused.
- `user_ids` — users of identity. Courier resolves each one with `GetUser` (cached `USER_CACHE_TTL`): e-mail, name and language. The mail goes in the language of the user, with `{{.name}}` filled per recipient. A user that does not exist or has no e-mail is returned in `skipped_user_ids`; an identity outage is `UNAVAILABLE` (the caller retries, nothing was written).
- `recipients` — raw addresses, for someone identity does not know any more (a rejected applicant) or an operations chat; `locale_override` sets their language.
- One event, one `idempotency_key`, one delivery per (recipient, channel). Only e-mail is reachable by `user_ids` for now; a Telegram-only event (`bvchain.*`) takes raw `recipients` until the chat binding of a later phase.

Every template key must be declared in the catalog: `catalog_test.go` reads the templates and fails when a template uses a key the catalog does not list. A new event = a template directory + an entry in the YAML.

Courier needs `IDENTITY_TARGET` to resolve users, and identity must let it call `GetUser` (permission `identity:user:read`): create a confidential client `courier` in identity, give it a role with that permission and an exchange policy `courier -> identity`, then set `IDENTITY_TOKEN_URL`, `IDENTITY_CLIENT_ID`, `IDENTITY_CLIENT_SECRET` (see identity `docs/service-to-service-auth.md`, method 3). Locally `scripts/local.sh` does the same with the client `courier-dev` (roles `service_registrar` and `user_directory`, policy `courier-dev -> identity`), so a local courier also registers its permissions. Without `IDENTITY_TARGET`, `user_ids` are refused (`user_resolution_disabled`) and raw `recipients` still work.

`SendNotification` (raw template id and address) stays for the callers that have not moved yet (bvchain).

## Inbox (the bell)

`InboxService` is the in-app inbox of the signed-in user, called by the web apps through the gateway (`/api/notifications`). An event type with the `in_app` channel in the catalog writes one row per user in `courier_inbox` **in the same transaction as the event**: the title and body are rendered then, in the language of the user, from `in_app.title.tmpl` and `in_app.body.tmpl`, and the link of the item is `data.link`. It is not a delivery (no worker, no retry); give `in_app` only to events whose `link` is safe to keep (never a reset link).

| RPC | Gateway route | |
| --- | --- | --- |
| `ListInbox` | `GET /api/notifications?pageSize&pageToken&unreadOnly` | newest first, keyset pages, archived items left out |
| `GetUnreadCount` | `GET /api/notifications/unread-count` | |
| `MarkRead` | `POST /api/notifications/read` `{ids}` or `{all:true}` | ids of other users match nothing |
| `Archive` | `POST /api/notifications/archive` `{ids}` | an archived item also leaves the count |
| `StreamInbox` | `GET /api/notifications/stream` (SSE) | first message = unread count, then one per change, heartbeat every 25 s |

The user is the subject of the token, never a field of the request; a service token (no user) is refused (`inbox_user_required`). The rule of every RPC is `public` (any signed-in user), so the permission check lets users through, and the service API (`Notify`, `SendNotification`, `GetNotification`, `RetryDelivery`) needs `courier:notification:send` / `courier:notification:manage`, which a plain user never holds (`TestUsersReachOnlyTheInbox`). Kubernetes ServiceAccount callers stay trusted for the service API by their allow list.

Realtime: courier publishes `{unread, item?}` on the Redis channel `courier:inbox:<user_id>` after the commit (and `{unread}` after a read or archive, so other tabs agree); `StreamInbox` subscribes to it. Best effort: a missed message is repaired by the next list or the polling of the client. Items older than `INBOX_RETENTION_DAYS` (365) are purged by the worker.

Auth: set `COURIER_ISSUER` and `COURIER_JWKS_URL` (same values as the dictionary's `DICTIONARY_ISSUER` / `_JWKS_URL`) and let identity issue tokens for the audience `courier`: an exchange policy `gatekeeper -> courier` (`scripts/local.sh` creates it, with a client `courier-dev` whose resource is `courier`). Without them courier refuses user tokens and the bell stays empty.

## Preferences, quiet hours and Telegram

`PreferencesService` (gateway `/api/notifications/preferences` and `/telegram-binding`) is what the signed-in user chooses; `Notify` applies it for every `user_ids` recipient:

- **Categories x channels.** The categories are those of the catalog (`security`, `account`, `admin`, `bvchain`, ...). A user turns a channel off per category; the default is everything on, and only the exceptions are stored (`courier_user_prefs`). A category whose events are all `mandatory` (security, account) cannot be turned off: `SetPreferences` ignores it and `Notify` sends regardless. A switched-off e-mail or Telegram message is kept as a `skipped` delivery (`last_error: turned off by the user`) for the audit, a switched-off `in_app` writes no item; the user is then in `skipped_user_ids`.
- **Quiet hours.** One window per user (`from`, `to` as `HH:MM` in an IANA zone, may cross midnight). A non-mandatory e-mail or Telegram message that arrives inside it is created with `next_attempt_at` at the end of the window: it is not pushed to Redis, the relay sends it when it is due. The bell and mandatory events are never held back. An unknown zone means no quiet hours.
- **Telegram.** `StartTelegramBinding` returns a one-time code (10 characters, valid 15 minutes, one live code per user) and the link `https://t.me/<TELEGRAM_BOT_USERNAME>?start=<code>`. The worker listens to the bot with `getUpdates` (`TELEGRAM_BOT_TOKEN`, switch off with `TELEGRAM_POLL=false`; one worker polls at a time, the one holding a PostgreSQL advisory lock, the others try again every 15 s) and, on `/start <code>` in a private chat, stores `courier_channel_bindings` and answers in the language of the Telegram client. A chat belongs to one user: linking it again moves it. When Telegram says the chat blocked the bot or is gone (403, or 400 "chat not found"), the delivery fails for good and the binding is removed. Events whose channel is `telegram` reach a user only through that binding; a user without one is in `skipped_user_ids`.

Without `TELEGRAM_BOT_USERNAME` the section is hidden (`telegram.available` is false) and categories that only use Telegram are not listed.

## Admin: templates, announcements, delivery log

`AdminNotificationService` (gateway `/api/courier/*`, the admin app) is for the administrators. Each method has its own permission, declared in `courier/v1/permissions.proto` and **registered with identity at start** (`RegisterPermissions`, like the dictionary): give the client of courier in identity the role `service_registrar` and set `IDENTITY_TOKEN_URL`, `IDENTITY_CLIENT_ID`, `IDENTITY_CLIENT_SECRET`; courier is not ready until the registration worked (`PERMISSIONS_SYNC_TIMEOUT`, 2 min, then it exits). Without `IDENTITY_CLIENT_ID` it only warns, and an administrator that holds `*` can still use the screens.

| Permission | Allows |
| --- | --- |
| `courier:webhook:manage` | configure the webhooks |
| `courier:template:read` | event types, templates, previews, versions |
| `courier:template:write` | save and revert a template |
| `courier:broadcast:send` | send and list announcements |
| `courier:delivery:read` | the delivery log |
| `courier:notification:manage` | retry a failed or dead delivery (`RetryDelivery`) |
| `courier:notification:send` | `Notify` and `SendNotification` (services) |

- **Templates.** A template is the directory of an event type (`system_announcement`) for one locale and channel. The built-in files stay the fallback; a save writes `courier_templates` (and an entry of `courier_template_versions`, newest last) and wins over the built-in ones, a revert deletes the row (the history keeps it as a "reverted" entry). The renderer reads through an overlay file system, so nothing else changes; both processes reload the edited templates every `TEMPLATE_RELOAD` (30 s), and courier at once after a save. A save is checked first: the content must render (subject and body for e-mail, text for Telegram, title and body for the inbox) with sample data, else `INVALID_ARGUMENT` / `template_invalid`. `PreviewTemplate` renders a draft the same way without saving. The e-mail page layout is not editable, only the "content" of the html part.
- **Announcements.** `CreateBroadcast` sends a catalog type marked `broadcast: true` (`system.announcement`: `title`, `message`, optional `link`) to everyone (active users), the holders of a role, or listed users. Courier lists the audience through identity `ListUsers` (so it needs `IDENTITY_TARGET`) and sends it in pages of 200 as ordinary `Notify` events: the preferences, quiet hours and languages of the users apply. At most 20,000 users, sent while the request waits; the result (users addressed, users not reached) is kept in `courier_broadcasts`. An announcement can be sent on a subset of the channels of its type (`channels`: `email`, `in_app`, `push`; for example the bell only); a channel the type does not have is refused (`channel_unknown`), and the choices of the users still apply. `Notify` has the same `channels` field. The channels used are kept in `courier_broadcasts.channels` (`V50`; empty for an older announcement, which used every channel) and shown in the history. An announcement cannot be recalled.
- **Delivery log.** `ListDeliveries` pages the deliveries newest first, filtered by status, channel, notification, user and a part of the address. A failed or dead delivery is sent again with `RetryDelivery`.

## Digest (grouping events)

Events of the same kind that come close together can reach a person as **one** message ("3 registrations are waiting") instead of one each. A type opts in in the catalog: `digest: {window: 10m, group_by: [step]}` (`account.registration_pending` and `bvchain.failed` do). It applies to the messages that interrupt (e-mail, Telegram, browser notifications, webhooks); **the inbox is not grouped**, every item shows in the bell. A mandatory type is never grouped (the catalog refuses it).

- **How.** The first event of a group (same type, same values of `group_by`, same recipient, channel and address) becomes the *leader*: a delivery created `pending` with its first attempt at the end of the window (`courier_deliveries.digest_key`, `V48`). A later event of the group creates no delivery: it is attached to the leader (`courier_digest_items`) and goes out with it. The window starts at the first event and is not extended. When the worker claims the leader it closes the group (clears the key), so the next event starts a new one, and sends the leader and its items as one message; a failed send is retried with the same items.
- **Template.** The message is rendered from `<template>_digest` (for example `identity_registration_pending_digest`) in each channel of the type; browser notifications and webhooks use its `in_app.title` and `in_app.body`. The data is the values of the first event plus `count` and `items`, **one line per event**, the first event first; a line is the event's inbox body, else its Telegram text, else its e-mail subject. In an HTML part `items` is one string: use `{{range lines .items}}…{{.}}…{{end}}` (`lines` splits it and each line is escaped on its own), as `identity_registration_pending_digest/email.html.tmpl` does. Edit the digest template in the admin app like any other (the preview fills `count` and `items` with samples). `catalog_test.go` fails when a grouped type lacks a digest template or one reads an unknown key.
- **Races.** A unique index allows one open group per key and address, so concurrent events have one leader. The leader is locked while an event attaches: a worker that claims it meanwhile waits and then sees the item. The data of an event is deleted only when every delivery and every digest that carries it has been sent.
- **Quiet hours** push the leader later when their end is later than the window. The delivery log shows how many events went in a message (`merged`).

## Browser notifications (Web Push)

The `push` channel shows a notification on the devices a user subscribed, even when the portal is closed. It renders the **in-app template** (title and body; the admin edits one template for both) and opens `data.link` when clicked. The catalog gives it to the events that have an inbox item (`security.mfa_changed`, `security.password_changed`, `account.registration_pending`, `system.announcement`).

- **Keys.** Make a VAPID key pair with `courierctl vapid` and set `VAPID_PUBLIC_KEY` (courier and worker), `VAPID_PRIVATE_KEY` and `VAPID_SUBJECT` (`mailto:ops@example.com`, the contact of the push services) on the **worker**, which sends; the private key is a Secret. Without them the section is hidden in the portal and the worker logs a warning. Changing the key pair invalidates every subscription (the browsers must subscribe again).
- **Subscription.** The portal registers its service worker (`/sw.js`), calls `PushManager.subscribe` with the public key and sends the result to `SubscribePush`: one row per browser in `courier_push_subscriptions` (`V47`, at most 10 per user, the oldest goes; the same endpoint again moves to the signed-in user). A delivery goes to **each device** of the user, with the usual preferences (the "browser notifications" switch of a category), quiet hours and `skipped` rows.
- **Answers.** `404` and `410` mean the subscription is gone: the delivery fails for good and the subscription is deleted. `429` and `5xx` are retried, any other `4xx` is final. The endpoint is chosen by the browser, so courier connects to **public addresses only** (like webhooks) and `SubscribePush` refuses anything but `https` (`WEBHOOK_ALLOW_PRIVATE` relaxes it for local runs).
- **Scope.** A push subscription belongs to the origin that registered the service worker: the portal. The notification appears wherever the browser runs, whichever app the user has open, and a click opens the link (an absolute URL for another app).

## Webhooks (Slack or a signed JSON POST)

A webhook is an endpoint of the administrators that receives the events it subscribes to (admin app, *Webhooks*; permission `courier:webhook:manage`; `courier_webhooks`, `V46`). It is a delivery channel like e-mail: the delivery of an event to a webhook is a row of `courier_deliveries` with channel `webhook` and the webhook id as address, so it has the retries, the dead letters, the log and the metrics of the other channels.

- **Subscription.** `events` holds exact types (`bvchain.failed`), prefixes (`bvchain.*`) or `*`. A webhook gets an event **once** (not per user; an announcement reaches it once, with its first page), only when it is enabled. **Mandatory events (security, account) never go to a webhook**: they carry reset links and codes; the admin screen refuses to subscribe to them by name.
- **Message.** The title and body of the event's inbox template in the default locale of the event, else the event type and its values. `slack`: `{"text": "*title*\nbody"}` for a Slack incoming webhook (no signature, the URL is the secret). `generic`: `{"id","event_type","created_at","title","text","data":{...}}`, with the headers `X-Courier-Event`, `X-Courier-Timestamp` (Unix seconds) and `X-Courier-Signature: sha256=<hex>`, the HMAC-SHA256 of `"<timestamp>.<body>"` with the secret of the webhook. A receiver recomputes it and refuses an old timestamp. The secret is made by courier and shown once (rotate it from the edit form).
- **Retries.** A network error, `429` and `5xx` are retried with the usual backoff; any other answer (`4xx`, a redirect, a deleted or disabled webhook) is final: `failed`.
- **Safety.** The address must be `https` without credentials, and courier only connects to **public** addresses (the check is on the connected IP, so a name that resolves to the cluster is refused, redirects are not followed). `WEBHOOK_ALLOW_PRIVATE=true` (a local run) allows `http` and private addresses. The URL of a Slack webhook is a secret shown to anyone with `courier:webhook:manage`: give it to administrators only.

## Metrics and alerts

Both processes serve Prometheus metrics on `METRICS_ADDR` (`:9100`, path `/metrics`; the charts annotate the pods for scraping). The queue state is read from PostgreSQL and Redis at the scrape (cached 10 s), so any replica answers the same; the outcome of each send is counted in the worker that made it.

| Metric | |
| --- | --- |
| `courier_delivery_attempts_total{channel,outcome}` | attempts by outcome: `sent`, `retry`, `failed` (permanent error), `dead` (out of attempts) |
| `courier_send_duration_seconds{channel}` | histogram of the time a channel took |
| `courier_notify_total{result}` | `Notify` and `SendNotification` calls: `ok`, `invalid` (caller mistake), `error` |
| `courier_deliveries_pending{channel}` | waiting for a send or a retry |
| `courier_deliveries_recent{status,channel}` | the deliveries of the last 24 hours |
| `courier_oldest_due_delivery_age_seconds` | age of the oldest pending delivery that is due now: the sign of stuck workers |
| `courier_stream_length`, `courier_stream_pending` | the Redis stream and the messages not acknowledged |
| `courier_dependency_up{dependency}` | PostgreSQL and Redis answered the scrape |

[`docs/prometheus-rules.yaml`](docs/prometheus-rules.yaml) has the alerts (a delivery went dead, a channel fails, the queue is stuck, a dependency is down, `Notify` errors) and [`docs/grafana-dashboard.json`](docs/grafana-dashboard.json) a dashboard to import. Both are starting points: tune the thresholds to the volume of the system.

## Templates

Embedded under `internal/notify/templates/{locale}/{template_id}/`:

| File | Channel |
| --- | --- |
| `email.subject.tmpl` | Email subject (`text/template`) |
| `email.txt.tmpl` | Email text/plain |
| `email.html.tmpl` | Email text/html (`html/template`) |
| `telegram.txt.tmpl` | Telegram body |

Lookup: requested locale, then `DEFAULT_LOCALE` (default `en`). Courier rejects the RPC if no matching parts exist. The worker renders at send time. The HTML part of an email is set into `templates/<locale>/layout.html.tmpl` (logo, card, footer; the template file is its `content` block, `brand` in the data overrides the default name "MM"); a locale without a layout sends the part as it is.

Email uses `multipart/alternative` when both text and HTML are present. Sample template: `hello` (`en` and `vi`).

Templates in use (all `en` and `vi`; `telegram.txt.tmpl` is plain text, no parse mode):

| `template_id` | Channel | Data keys | Caller |
|---|---|---|---|
| `identity_password_reset`, `identity_welcome`, `identity_password_changed`, `identity_mfa_changed` | email | `name`, `link`, `expires_in` (minutes for the reset, days for the welcome), `change` (for `identity_mfa_changed`: `enabled`, `disabled`, `enforced`, `reset`, `passkey_added`, `passkey_removed`, `recovery_regenerated`) | identity |
| `bvchain_checkin`, `bvchain_checkout` | telegram | `shift` (`1` morning, `2` afternoon), `time` (`HH:MM`) | bvchain |
| `bvchain_failed` | telegram | `step`, `error` | bvchain, bvreport |
| `bvreport_working_report` | telegram | `from_date`, `to_date`, `total_in_period`, `today`, `total_to_today`, `days_with_cico`, `missing_days` | bvreport |

Try a template without the caller: `courierctl --template bvchain_checkin --channel telegram --address <chat_id> --data shift=1 --data time=08:30`.

## Adding a channel

1. Add a proto enum value (`CHANNEL_SLACK = 30`) and regenerate stubs.
2. Add `templates/{locale}/{id}/slack.txt.tmpl` (and any other parts).
3. Implement `notify.Channel` in `internal/notify/slack`.
4. `registry.Register(...)` in `cmd/worker`.

## Local development

`go.mod` pins released versions of `gocommon` and `protogen`. To work against the sibling checkouts instead, use a Go workspace (`go.work` is git-ignored and not sent to Docker builds):

```bash
go work init . ../gocommon ../protogen/go   # once, in courier/; delete go.work to go back to the pinned versions
```

```bash
# from the monorepo root — regenerate Go stubs after proto changes
# (the stubs of ../protogen/go are used through go.work)
PROTO_REF=v0.1.6 PROTO_DIR=protos PROTOGEN_DIR=protogen bash protos/scripts/generate.sh go

cd courier
docker compose up -d          # Redis :6379
cp .env.example .env          # optional; config is env-based. It moves courier to :9091 and the worker probes to :8082
                              # (defaults 9090/8081 collide with identity and gatekeeper when all run locally)

go run ./cmd/courier
go run ./cmd/worker

go run ./cmd/courierctl \
  --locale vi \
  --template hello \
  --channel telegram \
  --address '<chat_id>' \
  --data name=Hu
```

Courier listens on `HOST:PORT` (default `0.0.0.0:9090`). Worker probes listen on `HEALTH_ADDR` (default `:8081`).

## Configuration

All settings are environment variables (see `.env.example`).

**Shared / courier**

| Variable | Default | Notes |
| --- | --- | --- |
| `HOST` / `PORT` | `0.0.0.0` / `9090` | gRPC listen (courier) |
| `SHUTDOWN_TIMEOUT` | `15s` | Graceful drain |
| `LOG_LEVEL` / `LOG_ENCODING` | `info` / `console` | |
| `SERVICE_NAME` / `ENV` | `app` / `dev` | Only `ENV=local` has optional gRPC auth. Any other value (`dev`, `staging`, `production`, empty, a typo) requires ServiceAccount tokens, see [Service-to-service auth](#service-to-service-auth) |
| `DEFAULT_LOCALE` | `en` | Template fallback |
| `REDIS_HOST` / `PORT` / `PASSWORD` / `DB` | `localhost` / `6379` / empty / `0` | |
| `POSTGRES_HOST` / `PORT` / `USER` / `PASSWORD` / `DB` / `SSLMODE` | `localhost` / `5432` / `postgres` / empty / `app` / `disable` | Courier and the worker both connect; the schema comes from `db-migration` (`courier_*` tables). `POSTGRES_PASSWORD` is a Secret in the Helm values |
| `TELEGRAM_BOT_USERNAME` | empty | The bot of the "Link Telegram" button (with or without `@`). Empty hides the section |
| `PERMISSIONS_SYNC_TIMEOUT` | `2m` | How long the start retries to register the permissions with identity (needs `IDENTITY_CLIENT_ID`) |
| `TEMPLATE_RELOAD` | `30s` | How often (courier and worker) the templates edited in the admin app are loaded |
| `VAPID_PUBLIC_KEY` / `VAPID_PRIVATE_KEY` / `VAPID_SUBJECT` | empty | Web Push (`courierctl vapid` makes the pair). Courier needs the public key, the worker all three; empty: no Web Push |
| `WEBHOOK_ALLOW_PRIVATE` | `false` | Webhooks may use `http` and loopback or private addresses (local runs only; courier and worker) |
| `METRICS_ADDR` | `:9100` | Where `/metrics` is served (courier and worker); empty turns it off |
| `COURIER_ISSUER` / `COURIER_JWKS_URL` | empty | `iss` of the user tokens and where identity publishes its keys, for `InboxService`. Empty: user tokens are refused |
| `IDENTITY_TARGET` | empty | identity gRPC address to resolve `user_ids`; empty refuses them |
| `IDENTITY_INSECURE` | `true` | plaintext gRPC to identity (default). Set `false` once identity serves TLS (CA: `GRPC_TLS_CA_FILE`; courier serves TLS itself with `GRPC_TLS_CERT_FILE`/`GRPC_TLS_KEY_FILE`); see `docs/setup-notification.md` section 14 |
| `IDENTITY_TOKEN_URL` / `IDENTITY_CLIENT_ID` / `IDENTITY_CLIENT_SECRET` / `IDENTITY_AUDIENCE` | empty / empty / empty / `identity` | Client credentials of courier in identity (secret in a Secret); the same client, `courier-dev`, is used by `scripts/local.sh` |
| `USER_CACHE_TTL` | `60s` | How long a resolved user is cached |
| `COURIER_AUTH_AUDIENCE` | `courier` | Audience the callers' ServiceAccount tokens must have (any `ENV` except `local`); the process panics at start if it is blank |
| `COURIER_AUTH_ALLOWED` | empty | Accepted callers, comma separated, for example `system:serviceaccount:billing:billing-api`, or the short form `billing:billing-api` (`namespace:name`, needs `gocommon` v0.3.2 or later). In the Helm values it can be a YAML list (needs `go-application` 1.3.0 or later), which the chart joins with commas. Empty is valid and rejects every caller. Not a secret: it goes in the Helm values |

**Worker**

| Variable | Default | Notes |
| --- | --- | --- |
| `HEALTH_ADDR` | `:8081` | `/healthz`, `/readyz` (readiness fails while draining or if Redis or PostgreSQL is down) |
| `QUEUE_MAX_RETRY` | `5` | Attempts before a delivery becomes `dead` |
| `QUEUE_RELAY_INTERVAL` | `5s` | How often the relay pushes due deliveries (retries, rows Redis never got) |
| `QUEUE_RELAY_STALE` | `10m` | A pushed delivery still `pending` after this long is pushed again (message lost with Redis) |
| `INBOX_RETENTION_DAYS` | `365` | Inbox items older than this are deleted (`0` keeps all) |
| `RETENTION_DAYS` | `90` | Events and deliveries older than this are deleted (`0` keeps all); events with a pending delivery stay |
| `QUEUE_CLAIM_MIN_IDLE` | `30s` | `XAUTOCLAIM` idle time |
| `TELEGRAM_BOT_TOKEN` | | Required to send Telegram and to listen to the bot |
| `TELEGRAM_POLL` | `true` | Listen to the bot for `/start <code>` (one worker at a time) |
| `SMTP_HOST` / `PORT` / `USER` / `PASSWORD` / `FROM` | port `587` | Required to send email |
| `MAIL_BRAND` / `MAIL_LOGO_URL` | empty (name `MM`, logo drawn as a letter tile) | Name and logo (a PNG on a public https address; mail clients do not draw SVG) at the top of every HTML email. A caller that sends `brand` or `logo_url` in the template data wins over them |
| `MAIL_DIR` | empty | Local runs: write every email as `<UTC time>_<to>_<subject>.html` and `.txt` into this directory instead of sending it (replaces SMTP; `scripts/local.sh` sets it to `.local/mails`) |

**courierctl**

| Variable | Default |
| --- | --- |
| `GRPC_TARGET` | `localhost:9090` |
| `GRPC_TIMEOUT` | `5s` |
| `GRPC_INSECURE` | `true` |
| `COURIER_TOKEN_FILE` | empty | ServiceAccount token file, sent as `authorization: Bearer` (re-read when it changes). |

## Service-to-service auth

Outside `ENV=local` a caller proves who it is with a Kubernetes ServiceAccount token. Courier asks the API server to review the token (`TokenReview`), accepts it only when the audience is `COURIER_AUTH_AUDIENCE` and the caller is listed in `COURIER_AUTH_ALLOWED`, and logs the caller. There is no shared secret to create or rotate. Details and the `gocommon` API: `grpcserver.WithKubeAuth`, `grpcclient.WithBearerTokenFile` in the [`gocommon` README](https://github.com/huponx/gocommon#service-to-service-auth).

| `ENV` | gRPC auth |
| --- | --- |
| `local` | services are not authenticated (a laptop has no Kubernetes API server); users still need their token for the inbox and the admin screens |
| `dev`, `staging`, `production`, anything else | mandatory ServiceAccount tokens |

Health checks (`/grpc.health.v1.Health/*`) skip auth. Courier starts with an empty `COURIER_AUTH_ALLOWED` (and logs a warning), it then rejects every call.

**Courier side.** The chart (`go-application` 1.1.0 or later) creates courier's ServiceAccount and binds it to the ClusterRole `system:auth-delegator`, which is what lets it create `TokenReview`s (`serviceAccount.create` and `rbac.authDelegator`, both on in `charts/courier/values.yaml`; the binding is cluster scoped). Set the audience and the callers in the Application values of `mm-deploy`:

```yaml
application:                     # alias of the go-application dependency
  config:
    data:
      COURIER_AUTH_AUDIENCE: courier
      COURIER_AUTH_ALLOWED: "system:serviceaccount:billing:billing-api"
```

**Caller side.** Give the caller its own ServiceAccount (not `default`), mount a projected token for the audience `courier` and send it with `grpcclient.WithBearerTokenFile`; then add the caller's `system:serviceaccount:<namespace>:<name>` to `COURIER_AUTH_ALLOWED`:

```yaml
deployment:
  serviceAccountName: billing-api        # or serviceAccount.create: true
  volumes:
    - name: courier-token
      projected:
        sources:
          - serviceAccountToken:
              audience: courier
              expirationSeconds: 3600
              path: token
  container:
    volumeMounts:
      - name: courier-token
        mountPath: /var/run/secrets/courier
        readOnly: true
```

```go
conn, err := grpcclient.Dial(ctx, target, grpcclient.WithBearerTokenFile("/var/run/secrets/courier/token"))
```

**From a laptop** against a real environment: `kubectl create token <sa> -n <namespace> --audience courier --duration 10m > /tmp/courier-token`, then `COURIER_TOKEN_FILE=/tmp/courier-token courierctl ...` (the service account must be in `COURIER_AUTH_ALLOWED`). To work against a local courier use `ENV=local`.

A revoked token can still be accepted for up to a minute (courier caches accepted reviews for 60 s, capped by the token expiry).

## Kubernetes

- Courier: gRPC health on the API port (`grpcserver` health is on by default; no token required for `/grpc.health.v1.Health`).
- Worker: liveness `GET /healthz`, readiness `GET /readyz`. On SIGTERM the worker sets ready false, stops consuming, drains in-flight jobs, then shuts down the probe server. The worker does not call courier gRPC and does not need any gRPC auth setting.

## Queue

- Stream: `courier:notifications`
- Consumer group: `courier-workers`
- At-least-once: `XACK` after a successful send; unacked messages are reclaimed with `XAUTOCLAIM`

## Images and charts

Two images are built from the repository root (distroless, non-root):

```bash
make docker IMAGE_TAG=dev     # ghcr.io/huponx/courier:dev and ghcr.io/huponx/courier-worker:dev
```

`build/courier/Dockerfile` and `build/courier-worker/Dockerfile`; `courierctl` is not shipped.

Helm charts `charts/courier` and `charts/courier-worker` wrap the `go-application` chart (dependency alias `application`, so all values sit under `application:`). `version`, `appVersion` and `image.tag` are written at release time by the CI; the committed values only keep `helm template` working.

```bash
cd charts/courier && helm dependency build && helm lint . -f ci/full-values.yaml
helm template courier . -f ci/full-values.yaml
```

Secret values never go in git: set `application.secrets` with `secretName`/`key` in the deployment repository (`mm-deploy`).

## CI/CD

`.github/config/ci.yaml` is the contract and `.github/workflows/{pr,main,staging,production}.yml` call the reusable workflows of `huponx/actions` (`service-*.yml`):

| Flow | Trigger | Result |
|---|---|---|
| pr | pull request to `main` | lint, test, build, Trivy scan, `helm lint`; nothing pushed |
| main | push to `main` | image and chart pushed (`1.YYYYMMDD.<run>-main-<sha7>`), deployment PR for `dev` in `mm-deploy` |
| staging | push to `release*` | pre-release `v<YYYY>.<MM>.N-release-<sha7>`, image and chart pushed, deployment PR for `staging` |
| production | release published | no build; verifies the image and chart of the release tag exist, deployment PR for `production` |

`N` only increases after the previous pre-release of the month was published; until then a new push changes only the short sha. Required secret: `BOT_TOKEN` (write access to `mm-deploy`).

## Setup (one time)

Commands use the [GitHub CLI](https://cli.github.com) (`gh auth login` first, with admin rights on the repositories). Package visibility and access are set in the GitHub UI (package settings).

1. **Publish `huponx/actions`**: merge it and make sure a release exists and the floating `v1` tag points to it (the workflows use `huponx/actions/...@v1`). Other repositories must be allowed to use its workflows:

   ```bash
   gh release list -R huponx/actions --limit 3                     # a release must exist
   gh api repos/huponx/actions/git/ref/tags/v1 --jq .object.sha    # v1 must exist
   # private repo only: share the actions with the other repositories of the owner
   gh api -X PUT repos/huponx/actions/actions/permissions/access -f access_level=user
   ```

2. **Token for `mm-deploy`**: `GITHUB_TOKEN` cannot open PRs in another repository. Pick one:

   ```bash
   # a) PAT (fine-grained, repository huponx/mm-deploy: Contents and Pull requests write)
   gh secret set BOT_TOKEN -R huponx/courier                       # paste the token

   # b) GitHub App installed on mm-deploy (Contents and Pull requests write)
   gh variable set DEPLOY_APP_ID -R huponx/courier --body "<app id>"
   gh secret set DEPLOY_APP_PRIVATE_KEY -R huponx/courier < app-private-key.pem
   ```

   The App takes precedence when `DEPLOY_APP_ID` is set; without it `BOT_TOKEN` is used.

3. **Optional**: Slack notification of deployment PRs (set `slack-channel` in `ci.yaml`):

   ```bash
   gh secret set SLACK_WEBHOOK_URL -R huponx/courier
   ```

4. **Workflow permissions** (packages and the staging pre-release need write):

   ```bash
   gh api -X PUT repos/huponx/courier/actions/permissions/workflow \
     -f default_workflow_permissions=write -F can_approve_pull_request_reviews=false
   ```

5. **GHCR packages**: after the first `main` run the packages `courier`, `courier-worker`, `charts/courier` and `charts/courier-worker` exist. Link each to the repository (package settings, "Manage Actions access", role Write) and give the ArgoCD clusters pull access (public, or an image pull secret and an OCI repository credential in each ArgoCD). `go-application` 1.0.0 must be readable from `oci://ghcr.io/huponx/charts`.

   ```bash
   gh api "/user/packages?package_type=container" --jq '.[].name'   # list the packages (use /orgs/<org>/packages for an org)
   ```

6. **Branch ruleset** for `main` (PR required, squash only, required checks of the `pr` flow, no force push or deletion). Merge the workflows to `main` first, while `main` is unprotected: status checks only show up in the ruleset picker after they have run once. Save as `main-ruleset.json` and apply (the file is only an input, do not commit it):

   ```json
   {
     "name": "protect main",
     "target": "branch",
     "enforcement": "active",
     "conditions": { "ref_name": { "include": ["~DEFAULT_BRANCH"], "exclude": [] } },
     "rules": [
       { "type": "deletion" },
       { "type": "non_fast_forward" },
       { "type": "pull_request", "parameters": {
           "required_approving_review_count": 0, "dismiss_stale_reviews_on_push": false,
           "require_code_owner_review": false, "require_last_push_approval": false,
           "required_review_thread_resolution": false,
           "allowed_merge_methods": ["squash"] } },
       { "type": "required_status_checks", "parameters": {
           "strict_required_status_checks_policy": true,
           "required_status_checks": [
             { "context": "pr / contract" },
             { "context": "pr / lint-test" },
             { "context": "pr / build / build (courier)" },
             { "context": "pr / build / build (courier-worker)" } ] } }
     ],
     "bypass_actors": []
   }
   ```

   ```bash
   REPO=huponx/courier
   gh api repos/$REPO/rulesets --method POST --input main-ruleset.json
   gh api repos/$REPO/rulesets --jq '.[] | {id, name, enforcement}'   # verify
   ```

   Or in the UI: Settings > Rules > Rulesets > New ruleset > Import a ruleset. The check names are `<caller job> / <reusable job>`; confirm them with `gh pr checks <pr>` after the first PR run. To change it later: `gh api repos/$REPO/rulesets/<id> --method PUT --input main-ruleset.json`. Rulesets on a private repo need a paid GitHub plan.

   Second ruleset for `release*`: a merge into it creates a staging pre-release, so it needs a PR with the same checks as `main` (the `pr` flow also runs on PRs into `release*`), and no force push or deletion. Save as `release-ruleset.json` and apply the same way (`gh api repos/$REPO/rulesets --method POST --input release-ruleset.json`):

   ```json
   {
     "name": "protect release branches",
     "target": "branch",
     "enforcement": "active",
     "conditions": { "ref_name": { "include": ["refs/heads/release*"], "exclude": [] } },
     "rules": [
       { "type": "deletion" },
       { "type": "non_fast_forward" },
       { "type": "pull_request", "parameters": {
           "required_approving_review_count": 0, "dismiss_stale_reviews_on_push": false,
           "require_code_owner_review": false, "require_last_push_approval": false,
           "required_review_thread_resolution": false,
           "allowed_merge_methods": ["merge", "squash"] } },
       { "type": "required_status_checks", "parameters": {
           "strict_required_status_checks_policy": false,
           "required_status_checks": [
             { "context": "pr / contract" },
             { "context": "pr / lint-test" },
             { "context": "pr / build / build (courier)" },
             { "context": "pr / build / build (courier-worker)" } ] } }
     ],
     "bypass_actors": []
   }
   ```

   To update the existing ruleset: `gh api repos/$REPO/rulesets --jq '.[] | {id, name}'`, then `gh api repos/$REPO/rulesets/<id> --method PUT --input release-ruleset.json`. The ruleset applies to every `release*` branch, so create the first one (for example `release`) from `main` before applying it, or push it while the ruleset is not yet active.

7. **`mm-deploy`**:

   ```bash
   gh repo edit huponx/mm-deploy --enable-auto-merge --delete-branch-on-merge
   gh label create automerge -R huponx/mm-deploy --color 0E8A16 --description "Merge automatically when checks pass" || true
   ```

   - required checks on `main`: `ci`, `diff`, `cve`, `pr-title`;
   - each ArgoCD (one per environment) has a root app with `path: applications/<env>`;
   - after the first publish, add `applications/dev/courier.yaml` and `courier-worker.yaml` (same shape as `x-noti`) with `targetRevision` set to the published version; later versions come from the deployment PRs.

8. **Environments**: staging and production are `enabled: false` in `.github/config/ci.yaml`; remove that line once their ArgoCD and `applications/<env>` exist. For production add `reviewers` and keep `automerge` off.

9. **Verify**:

   ```bash
   gh secret list -R huponx/courier && gh variable list -R huponx/courier
   # merge a PR into main, then follow the run
   gh run list -R huponx/courier --workflow main.yml --limit 1
   gh run watch -R huponx/courier
   gh pr list -R huponx/mm-deploy --label automerge                 # the dev deployment PR
   ```
