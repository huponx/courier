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
| `SERVICE_NAME` / `ENV` | `app` / `dev` | Only `ENV=local` has optional gRPC auth. Any other value (`dev`, `staging`, `production`, empty, a typo) requires ServiceAccount tokens, see [Service-to-service auth](#service-to-service-auth) |
| `DEFAULT_LOCALE` | `en` | Template fallback |
| `REDIS_HOST` / `PORT` / `PASSWORD` / `DB` | `localhost` / `6379` / empty / `0` | |
| `COURIER_AUTH_AUDIENCE` | `courier` | Audience the callers' ServiceAccount tokens must have (any `ENV` except `local`); the process panics at start if it is blank |
| `COURIER_AUTH_ALLOWED` | empty | Accepted callers, comma separated, for example `system:serviceaccount:billing:billing-api`. Empty is valid and rejects every caller. Not a secret: it goes in the Helm values |
| `COURIER_S2S_TOKEN` | empty | **Local only** (`ENV=local`): optional static Bearer token; empty leaves the server open. Ignored (with a warning) in every other environment, do not deploy it |

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
| `COURIER_TOKEN_FILE` | empty | ServiceAccount token file, sent as `authorization: Bearer` (re-read when it changes). Wins over `COURIER_S2S_TOKEN` |
| `COURIER_S2S_TOKEN` | empty | Static Bearer token for a courier started with `ENV=local` and the same `COURIER_S2S_TOKEN` |

## Service-to-service auth

Outside `ENV=local` a caller proves who it is with a Kubernetes ServiceAccount token. Courier asks the API server to review the token (`TokenReview`), accepts it only when the audience is `COURIER_AUTH_AUDIENCE` and the caller is listed in `COURIER_AUTH_ALLOWED`, and logs the caller. There is no shared secret to create or rotate. Details and the `gocommon` API: `grpcserver.WithKubeAuth`, `grpcclient.WithBearerTokenFile` in the [`gocommon` README](https://github.com/huponx/gocommon#service-to-service-auth).

| `ENV` | gRPC auth |
| --- | --- |
| `local` | optional: open, or a static Bearer token when `COURIER_S2S_TOKEN` is set |
| `dev`, `staging`, `production`, anything else | mandatory ServiceAccount tokens; `COURIER_S2S_TOKEN` is ignored |

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
