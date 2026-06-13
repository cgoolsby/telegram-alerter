# Prompt: add telegram-alerter to k8s_home_platform

Paste the block below to an agent (e.g. Claude Code) running **inside the
`k8s_home_platform` repo**. It encodes that repo's conventions as discovered
from `CLAUDE.md`, `components/`, and the `home-lab` cluster overlays.

Prerequisite (run once from the `telegram-alerter` repo, not the platform repo):
publish the Helm chart as an OCI artifact so Flux can source it —

```sh
helm package chart/                    # -> telegram-alerter-0.2.0.tgz
helm push telegram-alerter-0.2.0.tgz oci://ghcr.io/cgoolsby/charts
# then make the ghcr package "telegram-alerter" (charts) public in GitHub UI
```

---

## PROMPT

You are working in the `k8s_home_platform` Flux GitOps repo. Add a new
first-party application component, **telegram-alerter**, following this repo's
existing conventions. Do not invent new patterns — mirror `cloudnative-pg` and
the rules in `CLAUDE.md`.

### What it is
`telegram-alerter` is a self-hosted Telegram pager (source:
`https://github.com/cgoolsby/telegram-alerter`). It exposes `POST /send` and
`POST /webhook/alertmanager` (bearer-auth), `GET /metrics`, `GET /healthz`. It
ships a Helm chart that can render a Deployment, Service, ServiceMonitor,
PrometheusRule, and a hot-loaded Grafana dashboard.

- Image: `ghcr.io/cgoolsby/telegram-alerter:v4` (public)
- Chart (OCI): `oci://ghcr.io/cgoolsby/charts` → chart `telegram-alerter` version `0.2.0` (public)
- Metric: `telegram_alerter_messages_total{endpoint,result}` where
  `result ∈ {sent,failed,throttled,rejected}`.

### Target
- **Cluster:** `home-lab-observability` (it runs kube-prometheus-stack, loki,
  promtail, victoria-metrics, and the Grafana sidecar — so ServiceMonitor,
  PrometheusRule, the dashboard ConfigMap, and the Loki "last messages" panel
  all resolve there). Alertmanager also lives here.
- **Namespace:** `telegram-alerter`
- **Flux layer:** `services` (this is an app, not an operator; the observability
  cluster has `system`/`global`/`services`, no `operators` layer).
- **Component version:** `v0.2.0-1.0` (`<chart-version>-<platform-revision>`).

### Conventions to honor (from CLAUDE.md)
1. Component path `components/helmreleases/telegram-alerter/v0.2.0-1.0/` with
   `base/`, `local-kind/`, `_cli-template/`, and `metadata.yaml`.
2. **No `namespace.yaml` in `base/`.** Declare the namespace in the cluster's
   `global/namespaces/`. No `createNamespace`/`crds: Skip`-style namespace
   creation in the HelmRelease.
3. Source the chart via an **OCI `HelmRepository`** (see `envoy-gateway` for the
   `type: oci` pattern) — never a remote URL at reconcile time.
4. HelmRelease `name` == `targetNamespace` (`telegram-alerter`), so set
   `fullnameOverride: telegram-alerter` in `values:` to avoid double-prefixing.
5. Secrets are **SOPS-encrypted `*.enc.yaml`** (the rule
   `clusters/home-lab/.*\.enc\.yaml$` already covers home-lab; encrypts
   `data|stringData`). The chart references an existing Secret
   (`secret.existingSecret`), so do **not** put tokens in `values:`.

### Files to create

**1. `components/helmreleases/telegram-alerter/v0.2.0-1.0/base/helmrepository.yaml`**
```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata:
  name: telegram-alerter-oci
  namespace: flux-system
spec:
  type: oci
  interval: 60m
  url: oci://ghcr.io/cgoolsby/charts
```

**2. `.../base/telegram-alerter.yaml`** (HelmRelease)
```yaml
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: telegram-alerter
  namespace: flux-system
spec:
  targetNamespace: telegram-alerter
  interval: 5m
  serviceAccountName: ${FLUX_TENANT}
  chart:
    spec:
      chart: telegram-alerter
      version: '0.2.0'
      sourceRef:
        kind: HelmRepository
        name: telegram-alerter-oci
  values:
    fullnameOverride: telegram-alerter
    image:
      tag: v4
    secret:
      create: false
      existingSecret: telegram-alerter
    config:
      throttleWindowSeconds: 300     # dedupe identical pages within 5m — confirm
      logMessageContent: true
    serviceMonitor:
      enabled: true
      labels:
        release: kube-prometheus-stack   # CONFIRM scrape selector (see notes)
    grafanaDashboard:
      enabled: true
      namespace: monitoring
      datasources:
        metrics: { uid: victoria-metrics, type: victoriametrics-metrics-datasource }
        logs:    { uid: loki, type: loki }
    prometheusRule:
      enabled: true
      namespace: monitoring
```

**3. `.../base/version.yaml`**
```yaml
apiVersion: tensorwave-infra.tensorwave.com/v1alpha1
kind: ComponentVersion
metadata:
  name: telegram-alerter
  namespace: telegram-alerter
spec:
  version: v0.2.0-1.0
```

**4. `.../base/kustomization.yaml`**
```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - telegram-alerter.yaml
  - helmrepository.yaml
  - version.yaml
```

**5. `.../local-kind/kustomization.yaml`** and
**`.../_cli-template/kustomization.yaml`** — copy the exact shape from
`cloudnative-pg`'s equivalents (the `_cli-template` uses the
`{{ .Version }}/{{ .Overlay }}` placeholders).

**6. `.../metadata.yaml`**
```yaml
defaultTenant: system
defaultNamespace: telegram-alerter
namespacePsa: restricted            # the app runs non-root, read-only rootfs
healthCheck:
  apiVersion: helm.toolkit.fluxcd.io/v2
  kind: HelmRelease
  name: telegram-alerter
  namespace: flux-system
changeDurationMin: 15
impact: None Expected
details: Deploy telegram-alerter pager (chart 0.2.0, image v4)
```

**7. `.../README.md`** — short component README matching the others.

### Wiring into `home-lab-observability`
8. **Namespace:** create
   `clusters/home-lab/observability/global/namespaces/telegram-alerter.yaml`
   (Namespace with `pod-security.kubernetes.io/enforce: restricted`) and add it
   to that directory's `kustomization.yaml`.
9. **Instance:** create
   `clusters/home-lab/observability/services/telegram-alerter/kustomization.yaml`:
   ```yaml
   apiVersion: kustomize.config.k8s.io/v1beta1
   kind: Kustomization
   resources:
     - ../../../../../components/helmreleases/telegram-alerter/v0.2.0-1.0/base/
     - telegram-alerter-secret.enc.yaml
   ```
   and add `- telegram-alerter/` to
   `clusters/home-lab/observability/services/kustomization.yaml`.
10. **Secret (SOPS):** create
    `clusters/home-lab/observability/services/telegram-alerter/telegram-alerter-secret.enc.yaml`:
    ```yaml
    apiVersion: v1
    kind: Secret
    metadata:
      name: telegram-alerter
      namespace: telegram-alerter
    type: Opaque
    stringData:
      TELEGRAM_BOT_TOKEN: "<from BotFather>"
      TELEGRAM_CHAT_ID: "<chat id>"
      AUTH_TOKEN: "<openssl rand -hex 32>"
    ```
    then encrypt in place: `sops --encrypt --in-place <that file>`. Confirm
    `data|stringData` is encrypted and metadata stays readable.

### Docs
11. Add a row to the HelmReleases table in `components/README.md` and create the
    matching `components/helmreleases/telegram-alerter.md` doc page if the repo
    keeps per-component docs.

### Decisions to confirm with the maintainer before finalizing
- **Scrape mechanism:** the cluster runs both kube-prometheus-stack and
  victoria-metrics. Verify whether scraping uses Prometheus-Operator
  `ServiceMonitor` (chart default) or VictoriaMetrics `VMServiceScrape`. If the
  latter, the chart's ServiceMonitor won't be picked up — add a `VMServiceScrape`
  in `base/` instead (and likewise confirm `PrometheusRule` vs `VMRule`).
- **`serviceMonitor.labels`:** match the running Prometheus/VMAgent
  `serviceMonitorSelector` (the `release: kube-prometheus-stack` label is a
  guess).
- **Datasource UIDs:** confirm `victoria-metrics` /
  `victoriametrics-metrics-datasource` and `loki` match this cluster's Grafana
  datasources.
- **Throttle window** (`config.throttleWindowSeconds`) default.
- **Self-paging:** route Alertmanager's `TelegramAlerter*` alerts to a
  **different** receiver than telegram-alerter — a pager can't page about its
  own outage. Update the Alertmanager config/route accordingly.

### Verify
- `kustomize build clusters/home-lab/observability/services/telegram-alerter/` renders cleanly.
- `flux build kustomization <name> --path ...` (or the repo's CI lint) passes.
- `sops -d <secret>.enc.yaml` round-trips for an authorized key.
- After reconcile: HelmRelease Ready; pod up; `/metrics` scraped; dashboard
  "Telegram Alerter" appears in Grafana; a test `POST /send` reaches the phone.
