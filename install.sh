#!/usr/bin/env bash
# One-shot installer for telegram-alerter.
#
# Usage:
#   ./install.sh                         # prompts for anything not set in env
#   IMAGE=ghcr.io/you/telegram-alerter:v1 \
#   TELEGRAM_BOT_TOKEN=... TELEGRAM_CHAT_ID=... ./install.sh
#
# Env vars (prompted for if unset):
#   IMAGE                image ref to build/push and deploy (registry your cluster can pull from)
#   TELEGRAM_BOT_TOKEN   bot token from @BotFather
#   TELEGRAM_CHAT_ID     your chat id (from getUpdates)
#   AUTH_TOKEN           API bearer token; auto-generated if left empty
#   SKIP_BUILD=1         skip docker build/push (image already pushed)
#   SKIP_TEST=1          skip the post-deploy test page

set -euo pipefail

NAMESPACE=telegram-alerter
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

log()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

command -v kubectl >/dev/null || fail "kubectl not found"
if [[ "${SKIP_BUILD:-}" != "1" ]]; then
  command -v docker >/dev/null || fail "docker not found (or set SKIP_BUILD=1 if the image is already pushed)"
fi

# --- gather config -----------------------------------------------------------
if [[ -z "${IMAGE:-}" ]]; then
  read -rp "Image ref to push/deploy (e.g. ghcr.io/you/telegram-alerter:v1): " IMAGE
fi
[[ -n "$IMAGE" ]] || fail "IMAGE is required"

if [[ -z "${TELEGRAM_BOT_TOKEN:-}" ]]; then
  read -rsp "Telegram bot token (from @BotFather): " TELEGRAM_BOT_TOKEN; echo
fi
[[ -n "$TELEGRAM_BOT_TOKEN" ]] || fail "TELEGRAM_BOT_TOKEN is required"

if [[ -z "${TELEGRAM_CHAT_ID:-}" ]]; then
  read -rp "Telegram chat id: " TELEGRAM_CHAT_ID
fi
[[ -n "$TELEGRAM_CHAT_ID" ]] || fail "TELEGRAM_CHAT_ID is required"

if [[ -z "${AUTH_TOKEN:-}" ]]; then
  AUTH_TOKEN="$(openssl rand -hex 32)"
  log "Generated AUTH_TOKEN: $AUTH_TOKEN"
  log "Save it — callers need it as: Authorization: Bearer <token>"
fi

# --- build & push ------------------------------------------------------------
if [[ "${SKIP_BUILD:-}" != "1" ]]; then
  log "Building image $IMAGE"
  docker build -t "$IMAGE" "$SCRIPT_DIR"
  log "Pushing image $IMAGE"
  docker push "$IMAGE"
else
  log "SKIP_BUILD=1, assuming $IMAGE is already pushed"
fi

# --- deploy ------------------------------------------------------------------
log "Creating namespace"
kubectl apply -f "$SCRIPT_DIR/k8s/namespace.yaml"

log "Creating/updating secret"
kubectl -n "$NAMESPACE" create secret generic telegram-alerter \
  --from-literal=TELEGRAM_BOT_TOKEN="$TELEGRAM_BOT_TOKEN" \
  --from-literal=TELEGRAM_CHAT_ID="$TELEGRAM_CHAT_ID" \
  --from-literal=AUTH_TOKEN="$AUTH_TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f -

log "Deploying"
sed "s|IMAGE_PLACEHOLDER|$IMAGE|" "$SCRIPT_DIR/k8s/deployment.yaml" | kubectl apply -f -
kubectl apply -f "$SCRIPT_DIR/k8s/service.yaml"

log "Waiting for rollout"
kubectl -n "$NAMESPACE" rollout status deployment/telegram-alerter --timeout=120s

# --- smoke test --------------------------------------------------------------
if [[ "${SKIP_TEST:-}" != "1" ]]; then
  log "Sending test page via port-forward"
  kubectl -n "$NAMESPACE" port-forward svc/telegram-alerter 18080:80 >/dev/null 2>&1 &
  PF_PID=$!
  trap 'kill "$PF_PID" 2>/dev/null || true' EXIT
  sleep 3
  HTTP_CODE=$(curl -s -o /tmp/telegram-alerter-test.out -w '%{http_code}' \
    -X POST http://localhost:18080/send \
    -H "Authorization: Bearer $AUTH_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"message":"✅ telegram-alerter installed and working"}')
  kill "$PF_PID" 2>/dev/null || true
  trap - EXIT
  if [[ "$HTTP_CODE" == "200" ]]; then
    log "Test page sent — check your phone!"
  else
    cat /tmp/telegram-alerter-test.out >&2 || true
    fail "test page failed (HTTP $HTTP_CODE)"
  fi
fi

log "Done. In-cluster endpoint: http://telegram-alerter.telegram-alerter.svc/send"
log 'Example: curl -X POST http://telegram-alerter.telegram-alerter.svc/send -H "Authorization: Bearer <AUTH_TOKEN>" -H "Content-Type: application/json" -d '"'"'{"message":"alert!"}'"'"
