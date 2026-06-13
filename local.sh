#!/usr/bin/env bash
# Local end-to-end test: kind cluster + telegram-alerter + real test page.
#
# Usage:
#   ./local.sh        # create kind cluster, deploy, run tests
#   ./local.sh down   # delete the kind cluster
#
# Needs .env (or env vars) with TELEGRAM_BOT_TOKEN, TELEGRAM_CHAT_ID, AUTH_TOKEN.
# The test sends a real message to your Telegram.

set -euo pipefail

CLUSTER=telegram-alerter-test
CONTEXT="kind-$CLUSTER"
NAMESPACE=telegram-alerter
LOCAL_IMAGE=telegram-alerter:local
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

log()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# All kubectl calls pin --context so this can never touch a real cluster.
kc() { kubectl --context "$CONTEXT" "$@"; }

if [[ "${1:-}" == "down" ]]; then
  kind delete cluster --name "$CLUSTER"
  exit 0
fi

command -v kind >/dev/null || fail "kind not found"
command -v kubectl >/dev/null || fail "kubectl not found"
command -v docker >/dev/null || fail "docker not found"

# Load credentials from .env if present; shell env vars take precedence.
if [[ -f "$SCRIPT_DIR/.env" ]]; then
  while IFS='=' read -r key value; do
    [[ "$key" =~ ^[A-Z_]+$ ]] || continue
    [[ -z "${!key:-}" ]] && export "$key=$value"
  done < "$SCRIPT_DIR/.env"
fi
[[ -n "${TELEGRAM_BOT_TOKEN:-}" ]] || fail "TELEGRAM_BOT_TOKEN not set (create .env from .env.example)"
[[ -n "${TELEGRAM_CHAT_ID:-}" ]] || fail "TELEGRAM_CHAT_ID not set"
[[ -n "${AUTH_TOKEN:-}" ]] || fail "AUTH_TOKEN not set"

# --- cluster -----------------------------------------------------------------
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log "Reusing existing kind cluster $CLUSTER"
else
  log "Creating kind cluster $CLUSTER"
  kind create cluster --name "$CLUSTER" --wait 120s
fi

# --- build & load ------------------------------------------------------------
log "Building $LOCAL_IMAGE"
docker build -t "$LOCAL_IMAGE" "$SCRIPT_DIR"
log "Loading image into kind"
kind load docker-image "$LOCAL_IMAGE" --name "$CLUSTER"

# --- deploy ------------------------------------------------------------------
log "Deploying"
kc apply -f "$SCRIPT_DIR/k8s/namespace.yaml"
kc -n "$NAMESPACE" create secret generic telegram-alerter \
  --from-literal=TELEGRAM_BOT_TOKEN="$TELEGRAM_BOT_TOKEN" \
  --from-literal=TELEGRAM_CHAT_ID="$TELEGRAM_CHAT_ID" \
  --from-literal=AUTH_TOKEN="$AUTH_TOKEN" \
  --dry-run=client -o yaml | kc apply -f -
sed "s|IMAGE_PLACEHOLDER|$LOCAL_IMAGE|" "$SCRIPT_DIR/k8s/deployment.yaml" | kc apply -f -
kc apply -f "$SCRIPT_DIR/k8s/service.yaml"
kc -n "$NAMESPACE" rollout restart deployment/telegram-alerter >/dev/null
kc -n "$NAMESPACE" rollout status deployment/telegram-alerter --timeout=120s

# --- tests -------------------------------------------------------------------
log "Starting port-forward"
kc -n "$NAMESPACE" port-forward svc/telegram-alerter 18080:80 >/dev/null 2>&1 &
PF_PID=$!
trap 'kill "$PF_PID" 2>/dev/null || true' EXIT
sleep 3

PASS=0; FAIL=0
check() { # check <name> <expected_code> <actual_code>
  if [[ "$2" == "$3" ]]; then
    log "PASS: $1 ($3)"; PASS=$((PASS+1))
  else
    printf '\033[1;31mFAIL:\033[0m %s (expected %s, got %s)\n' "$1" "$2" "$3"; FAIL=$((FAIL+1))
  fi
}

code=$(curl -s -o /dev/null -w '%{http_code}' http://localhost:18080/healthz)
check "healthz" 200 "$code"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST http://localhost:18080/send \
  -H "Content-Type: application/json" -d '{"message":"should be rejected"}')
check "missing auth rejected" 401 "$code"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST http://localhost:18080/send \
  -H "Authorization: Bearer wrong-token" \
  -H "Content-Type: application/json" -d '{"message":"should be rejected"}')
check "wrong auth rejected" 401 "$code"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST http://localhost:18080/send \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" -d '{"message":""}')
check "empty message rejected" 400 "$code"

code=$(curl -s -o /tmp/telegram-alerter-local-test.out -w '%{http_code}' \
  -X POST http://localhost:18080/send \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"message":"✅ telegram-alerter kind e2e test passed"}')
check "real send to telegram" 200 "$code"
[[ "$code" == "200" ]] || cat /tmp/telegram-alerter-local-test.out >&2 || true

code=$(curl -s -o /tmp/telegram-alerter-local-test.out -w '%{http_code}' \
  -X POST http://localhost:18080/webhook/alertmanager \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"E2ETest","severity":"info","instance":"kind"},"annotations":{"summary":"alertmanager webhook e2e test"}}]}')
check "alertmanager webhook" 200 "$code"
[[ "$code" == "200" ]] || cat /tmp/telegram-alerter-local-test.out >&2 || true

kill "$PF_PID" 2>/dev/null || true
trap - EXIT

log "$PASS passed, $FAIL failed"
if [[ "$FAIL" -eq 0 ]]; then
  log "All tests passed — check your phone for the test page."
  log "Cluster $CLUSTER left running; remove with: ./local.sh down"
else
  fail "some tests failed (cluster $CLUSTER left running for debugging)"
fi
