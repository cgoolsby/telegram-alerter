# telegram-alerter

A tiny Go service that runs as a pod in your Kubernetes cluster and forwards
HTTP API calls to your personal Telegram — a self-hosted pager for alerts.

```
POST /send  →  Telegram sendMessage  →  your phone buzzes
```

## Quick install

Once you have a bot token and chat ID (steps 1–2 below):

```sh
./install.sh
```

It prompts for the image ref and credentials (or reads `IMAGE`,
`TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID`, `AUTH_TOKEN` from the environment),
builds and pushes the image, creates the namespace + secret, deploys, and
sends a test page. `SKIP_BUILD=1` skips the image build/push; `SKIP_TEST=1`
skips the test page. Steps 4–6 below are the manual equivalent.

## 1. Create the Telegram bot

1. In Telegram, message [@BotFather](https://t.me/BotFather) and send `/newbot`.
2. Give it a name and a username (must end in `bot`, e.g. `curtis_pager_bot`).
3. BotFather replies with a **bot token** like `123456789:AAExampleTokenString`. Save it.

## 2. Find your chat ID

Bots can't message you first, so:

1. Open a chat with your new bot and send it any message (e.g. "hi").
2. Run:
   ```sh
   curl -s "https://api.telegram.org/bot<BOT_TOKEN>/getUpdates" | python3 -m json.tool
   ```
3. Find `result[0].message.chat.id` — that number is your **chat ID**.

## 3. Generate an API auth token

This is the shared secret callers must present to the service:

```sh
openssl rand -hex 32
```

## 4. Build and push the image

```sh
make docker-build docker-push IMAGE=<your-registry>/telegram-alerter:v1
```

Then set the image in `k8s/deployment.yaml` (replace `IMAGE_PLACEHOLDER`).

## 5. Deploy

```sh
kubectl apply -f k8s/namespace.yaml

kubectl -n telegram-alerter create secret generic telegram-alerter \
  --from-literal=TELEGRAM_BOT_TOKEN='<bot token>' \
  --from-literal=TELEGRAM_CHAT_ID='<chat id>' \
  --from-literal=AUTH_TOKEN='<auth token>'

kubectl apply -f k8s/deployment.yaml -f k8s/service.yaml
```

## 6. Send alerts

From anywhere inside the cluster:

```sh
curl -X POST http://telegram-alerter.telegram-alerter.svc/send \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"message":"🚨 disk 90% full on node-1"}'
```

From your laptop:

```sh
kubectl -n telegram-alerter port-forward svc/telegram-alerter 8080:80
curl -X POST http://localhost:8080/send \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"message":"test page"}'
```

### Page yourself from localhost

`send.sh` is a small client that reads `.env` and texts you:

```sh
./send.sh "disk is full on node-1"
echo "build failed" | ./send.sh          # message from stdin
./send.sh -s "quiet note"                # silent (no notification sound)
./send.sh -m HTML "<b>bold</b> alert"    # parse_mode HTML or MarkdownV2
./send.sh --via-service "msg"            # route through the deployed pod (port-forward)
```

Direct mode (default) talks straight to the Telegram API, so it works even if
the cluster is down. `--via-service` exercises the deployed service instead.

### Request body

| Field        | Type   | Required | Notes                                              |
|--------------|--------|----------|----------------------------------------------------|
| `message`    | string | yes      | Truncated to Telegram's 4096-char limit            |
| `parse_mode` | string | no       | `MarkdownV2` or `HTML` for formatted messages      |
| `silent`     | bool   | no       | `true` delivers without a notification sound       |
| `chat_id`    | string | no       | Override the default chat (e.g. a group)           |

### Responses

- `200 {"ok":true}` — delivered to Telegram
- `400` — empty message or invalid JSON
- `401` — missing/wrong bearer token
- `502` — Telegram rejected the message (description included) or was unreachable

## Endpoints

- `POST /send` — send a free-form message (bearer auth required)
- `POST /webhook/alertmanager` — Alertmanager/Grafana webhook receiver (bearer auth required)
- `GET /healthz` — liveness/readiness probe (no auth)
- `GET /metrics` — Prometheus metrics (no auth)

## Install with Helm

A chart lives in [`chart/`](chart/). It templates the Deployment, Service,
optional Secret, ServiceMonitor, and NetworkPolicy.

```sh
# create the credentials Secret (recommended over putting tokens in values)
kubectl create namespace telegram-alerter
kubectl -n telegram-alerter create secret generic telegram-alerter \
  --from-literal=TELEGRAM_BOT_TOKEN='<bot token>' \
  --from-literal=TELEGRAM_CHAT_ID='<chat id>' \
  --from-literal=AUTH_TOKEN='<auth token>'

helm install telegram-alerter ./chart -n telegram-alerter \
  --set serviceMonitor.enabled=true \
  --set serviceMonitor.labels.release=kube-prometheus-stack
```

Alternatively let the chart create the Secret (keep tokens out of git — pass
via `--set` or an uncommitted values file):

```sh
helm install telegram-alerter ./chart -n telegram-alerter --create-namespace \
  --set secret.create=true \
  --set secret.botToken=... --set secret.chatId=... --set secret.authToken=...
```

Key values (`chart/values.yaml`): `image.tag` (defaults to the chart
appVersion), `secret.create` / `secret.existingSecret`, `serviceMonitor.*`,
`networkPolicy.enabled`, `resources`.

## Metrics

`/metrics` exposes a Prometheus counter:

```
telegram_alerter_messages_total{endpoint="send|alertmanager",result="sent|failed|throttled|rejected"}
```

`result` is `sent` (delivered), `failed` (Telegram rejected/unreachable),
`throttled` (deduped by the throttle window, or Telegram 429), or `rejected`
(bad auth or invalid input). Enable scraping with `serviceMonitor.enabled=true`
and set `serviceMonitor.labels` to match your Prometheus release selector.

## Throttling

Set `config.throttleWindowSeconds` (env `THROTTLE_WINDOW_SECONDS`) to suppress
duplicate messages — same chat + text — seen within the window. Throttled
requests return `200 {"ok":true,"throttled":true}` (no retry) and increment the
`throttled` metric. `0` disables it. This is in addition to Telegram's own rate
limiting (429s also count as `throttled`).

## Grafana dashboard & alerts

Enable the hot-loaded dashboard and alerting rules via the chart:

```sh
helm upgrade telegram-alerter ./chart -n telegram-alerter \
  --set serviceMonitor.enabled=true --set serviceMonitor.labels.release=kube-prometheus-stack \
  --set grafanaDashboard.enabled=true \
  --set prometheusRule.enabled=true
```

- **Dashboard** (`grafanaDashboard.*`) — a ConfigMap labeled for the Grafana
  sidecar, in `grafanaDashboard.namespace` (default `monitoring`). Panels: sent/
  failed/throttled/rejected stats, a failed-sends-&-throttles timeseries, and a
  **last N messages sent** logs panel (from the pod logs via Loki — requires
  `config.logMessageContent: true`). Set `grafanaDashboard.datasources.*` to
  match your metrics/logs datasource UIDs (defaults: `victoria-metrics`, `loki`).
- **Alerts** (`prometheusRule.*`) — `TelegramAlerterDown`,
  `TelegramAlerterSendsFailing` (failures with zero successes),
  `TelegramAlerterSendFailureRate`, and `TelegramAlerterThrottling`. Thresholds
  and severities are configurable.

> ⚠️ A pager can't page itself. Route the `TelegramAlerter*` alerts to a
> **different** receiver (email/PagerDuty/a second bot) so a telegram-alerter
> outage still reaches you.

## Grafana & Alertmanager integration

### Grafana (works as-is)

Grafana's webhook contact point already sends a `message` field, so point it
straight at `/send`:

- **URL**: `http://telegram-alerter.telegram-alerter.svc/send`
- **Authorization Header** → Scheme `Bearer`, Credentials `<AUTH_TOKEN>`

Customize the notification template to control what lands in `message`.

### Alertmanager

Alertmanager's webhook body has a fixed shape (no `message` field), so use the
dedicated receiver `/webhook/alertmanager`, which formats the alerts for you:

```yaml
receivers:
  - name: telegram-pager
    webhook_configs:
      - url: http://telegram-alerter.telegram-alerter.svc/webhook/alertmanager
        send_resolved: true
        http_config:
          authorization:
            type: Bearer
            credentials: <AUTH_TOKEN>
```

The receiver renders a concise page like:

```
🔥 FIRING: 2 alert(s)
[critical] HighCPU on node-1
  CPU above 90% for 5m
[warning] DiskFull on node-2
  disk at 95%
```

It reads `alertname`, `severity`, and `instance` labels and the `summary`
(or `description`) annotation, caps the list at 10 alerts, and switches the
emoji to ✅ for resolved notifications.

### Two things to check

1. **Reachability.** Prometheus/Grafana usually live in a `monitoring`
   namespace; the service DNS above works cross-namespace. If you run
   default-deny NetworkPolicies, apply `k8s/networkpolicy.example.yaml`
   (adjust the namespace selector) to allow ingress from monitoring.
2. **Throttling.** Tune grouping on the *sender* side — Alertmanager's
   `group_by` / `group_wait` / `repeat_interval`, or Grafana's notification
   policy — so a flapping target doesn't page you dozens of times.

## Configuration (env vars)

| Variable             | Required | Default                    |
|----------------------|----------|----------------------------|
| `TELEGRAM_BOT_TOKEN` | yes      |                            |
| `TELEGRAM_CHAT_ID`   | yes      |                            |
| `AUTH_TOKEN`         | yes      |                            |
| `PORT`               | no       | `8080`                     |
| `TELEGRAM_API_BASE`  | no       | `https://api.telegram.org` |

## Run locally

```sh
TELEGRAM_BOT_TOKEN=... TELEGRAM_CHAT_ID=... AUTH_TOKEN=test go run .
```

## Tests

```sh
make test
```

End-to-end test in a local [kind](https://kind.sigs.k8s.io/) cluster (builds
the image, deploys the full stack, verifies auth/validation, and sends a real
test page to your Telegram — needs `.env`):

```sh
./local.sh        # create cluster + deploy + test
./local.sh down   # tear down the kind cluster
```
