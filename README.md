# courier

Notification service: a gRPC API enqueues work on Redis Streams; a worker renders locale-aware Go templates and sends email (SMTP) or Telegram.

Module: `github.com/huponx/courier`. Contracts live in `protos` (`courier/v1`); generated stubs in `protogen`. Shared libraries come from `gocommon`.

## Architecture

```
client  --gRPC-->  courier  --XADD-->  Redis Streams  --XREADGROUP-->  worker  -->  email / Telegram
```

| Process | Role |
| --- | --- |
| `cmd/courier` | gRPC only (`SendNotification`). Validates locale/template, then enqueues one Redis job per recipient. |
| `cmd/worker` | Consumes the stream, renders templates, sends. HTTP is only Kubernetes probes (`/healthz`, `/readyz`). |
| `cmd/courierctl` | Optional local CLI to call the API (`grpcclient`). Not deployed. |

Courier does not send messages. The worker does not expose gRPC.

## API

`CourierService.SendNotification` (see `courier/v1` in the protos repo):

- `locale` — `vi`, `en`, …
- `template_id` — directory name under `internal/notify/templates/{locale}/`
- `template_data` — `map<string,string>` passed into templates as `{{.name}}`
- `recipients` — `channel` (`EMAIL` = 10, `TELEGRAM` = 20) + `address` (email or Telegram `chat_id`)
- `idempotency_key` — optional; Redis `SET NX` (~24h). A repeat returns the same `notification_id` and does not enqueue again.

The RPC returns after enqueue (`notification_id`, `queued`). Field numbers and enum values use multiples of 10 (`UNSPECIFIED` = 0).

## Templates

Embedded under `internal/notify/templates/{locale}/{template_id}/`:

| File | Channel |
| --- | --- |
| `email.subject.tmpl` | Email subject (`text/template`) |
| `email.txt.tmpl` | Email text/plain |
| `email.html.tmpl` | Email text/html (`html/template`) |
| `telegram.txt.tmpl` | Telegram body |

Lookup: requested locale, then `DEFAULT_LOCALE` (default `en`). Courier rejects the RPC if no matching parts exist. The worker renders at send time.

Email uses `multipart/alternative` when both text and HTML are present. Sample template: `hello` (`en` and `vi`).

## Adding a channel

1. Add a proto enum value (`CHANNEL_SLACK = 30`) and regenerate stubs.
2. Add `templates/{locale}/{id}/slack.txt.tmpl` (and any other parts).
3. Implement `notify.Channel` in `internal/notify/slack`.
4. `registry.Register(...)` in `cmd/worker`.

## Local development

This repo expects sibling checkouts of `gocommon` and `protogen` (`replace` in `go.mod`).

```bash
# from the monorepo root — regenerate Go stubs after proto changes
# (also uncomment the local `replace` lines in courier/go.mod)
PROTO_REF=v0.1.6 PROTO_DIR=protos PROTOGEN_DIR=protogen bash protos/scripts/generate.sh go

cd courier
docker compose up -d          # Redis :6379
cp .env.example .env          # optional; config is env-based

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
| `SERVICE_NAME` / `ENV` | `app` / `dev` | `ENV=dev` → optional S2S; any other `ENV` requires `COURIER_S2S_TOKEN` or the process panics |
| `DEFAULT_LOCALE` | `en` | Template fallback |
| `REDIS_HOST` / `PORT` / `PASSWORD` / `DB` | `localhost` / `6379` / empty / `0` | |
| `COURIER_S2S_TOKEN` | empty | Receiver S2S secret. Inject from a Kubernetes Secret (`secretKeyRef`), never a ConfigMap. Leave empty only for local `ENV=dev`. |

**Worker**

| Variable | Default | Notes |
| --- | --- | --- |
| `HEALTH_ADDR` | `:8081` | `/healthz`, `/readyz` (readiness fails while draining or if Redis is down) |
| `QUEUE_MAX_RETRY` | `5` | Then ack + log; no DLQ |
| `QUEUE_CLAIM_MIN_IDLE` | `30s` | `XAUTOCLAIM` idle time |
| `TELEGRAM_BOT_TOKEN` | | Required to send Telegram |
| `SMTP_HOST` / `PORT` / `USER` / `PASSWORD` / `FROM` | port `587` | Required to send email |

**courierctl**

| Variable | Default |
| --- | --- |
| `GRPC_TARGET` | `localhost:9090` |
| `GRPC_TIMEOUT` | `5s` |
| `GRPC_INSECURE` | `true` |
| `COURIER_S2S_TOKEN` | empty | Same secret as courier; sent as `authorization: Bearer` |

## Service-to-service auth

Courier uses generic Bearer helpers from `gocommon`. The token is **courier’s** secret; every caller must present it.

```yaml
env:
  - name: COURIER_S2S_TOKEN
    valueFrom:
      secretKeyRef:
        name: courier-s2s-token
        key: token
```

Mount the same Secret on courier and on callers. `ENV=dev` uses optional auth (empty token = open). Any other `ENV` calls `WithBearerAuth` and panics if the token is missing. gRPC health checks skip auth. See `gocommon` README for the `{RECEIVER}_S2S_TOKEN` convention.

## Kubernetes

- Courier: gRPC health on the API port (`grpcserver` health is on by default; no S2S token required for `/grpc.health.v1.Health`).
- Worker: liveness `GET /healthz`, readiness `GET /readyz`. On SIGTERM the worker sets ready false, stops consuming, drains in-flight jobs, then shuts down the probe server. The worker does not call courier gRPC and does not need `COURIER_S2S_TOKEN`.

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

   Optional second ruleset for `release*` (a push there creates a staging pre-release, so no force push and no deletion). Save as `release-ruleset.json` and apply the same way:

   ```json
   {
     "name": "protect release branches",
     "target": "branch",
     "enforcement": "active",
     "conditions": { "ref_name": { "include": ["refs/heads/release*"], "exclude": [] } },
     "rules": [ { "type": "deletion" }, { "type": "non_fast_forward" } ],
     "bypass_actors": []
   }
   ```

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
Test