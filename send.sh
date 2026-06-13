#!/usr/bin/env bash
# Page yourself on Telegram from localhost.
#
# Usage:
#   ./send.sh "disk is full on node-1"
#   echo "build failed" | ./send.sh
#   ./send.sh -s "quiet note"            # silent (no notification sound)
#   ./send.sh -m HTML "<b>bold</b> alert"  # parse_mode: HTML or MarkdownV2
#   ./send.sh --via-service "msg"        # route through the deployed pod (port-forward)
#
# Reads TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID (and AUTH_TOKEN for --via-service)
# from .env if present. Direct mode (default) talks straight to Telegram and
# works even if the cluster is down.

set -euo pipefail

NAMESPACE=telegram-alerter
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

fail() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# Load .env; shell env vars take precedence.
if [[ -f "$SCRIPT_DIR/.env" ]]; then
  while IFS='=' read -r key value; do
    [[ "$key" =~ ^[A-Z_]+$ ]] || continue
    [[ -z "${!key:-}" ]] && export "$key=$value"
  done < "$SCRIPT_DIR/.env"
fi

SILENT=false
VIA_SERVICE=false
PARSE_MODE=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -s|--silent)      SILENT=true; shift ;;
    -m|--parse-mode)  PARSE_MODE="${2:-}"; shift 2 ;;
    --via-service)    VIA_SERVICE=true; shift ;;
    -h|--help)        grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    --)               shift; break ;;
    *)                break ;;
  esac
done

# Message from remaining args, or stdin if none.
if [[ $# -gt 0 ]]; then
  MESSAGE="$*"
else
  MESSAGE="$(cat)"
fi
[[ -n "${MESSAGE//[[:space:]]/}" ]] || fail "empty message (pass as an argument or pipe via stdin)"

if [[ "$VIA_SERVICE" == "true" ]]; then
  # Route through the deployed service via an automatic port-forward.
  [[ -n "${AUTH_TOKEN:-}" ]] || fail "AUTH_TOKEN not set (needed for --via-service)"
  command -v kubectl >/dev/null || fail "kubectl not found"
  kubectl -n "$NAMESPACE" port-forward svc/telegram-alerter 18080:80 >/dev/null 2>&1 &
  PF_PID=$!
  trap 'kill "$PF_PID" 2>/dev/null || true' EXIT
  sleep 2
  PAYLOAD=$(python3 -c 'import json,sys; print(json.dumps({"message":sys.argv[1],"silent":sys.argv[2]=="true","parse_mode":sys.argv[3]}))' \
    "$MESSAGE" "$SILENT" "$PARSE_MODE")
  HTTP_CODE=$(curl -s -o /tmp/telegram-send.out -w '%{http_code}' \
    -X POST http://localhost:18080/send \
    -H "Authorization: Bearer $AUTH_TOKEN" \
    -H "Content-Type: application/json" \
    -d "$PAYLOAD")
  kill "$PF_PID" 2>/dev/null || true; trap - EXIT
  [[ "$HTTP_CODE" == "200" ]] || { cat /tmp/telegram-send.out >&2; fail "service returned HTTP $HTTP_CODE"; }
  echo "sent via service"
else
  # Direct to the Telegram API.
  [[ -n "${TELEGRAM_BOT_TOKEN:-}" ]] || fail "TELEGRAM_BOT_TOKEN not set (create .env from .env.example)"
  [[ -n "${TELEGRAM_CHAT_ID:-}" ]] || fail "TELEGRAM_CHAT_ID not set"
  ARGS=(--data-urlencode "chat_id=$TELEGRAM_CHAT_ID" --data-urlencode "text=$MESSAGE")
  [[ "$SILENT" == "true" ]] && ARGS+=(--data-urlencode "disable_notification=true")
  [[ -n "$PARSE_MODE" ]] && ARGS+=(--data-urlencode "parse_mode=$PARSE_MODE")
  RESP=$(curl -s "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage" "${ARGS[@]}")
  if ! printf '%s' "$RESP" | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin).get("ok") else 1)' 2>/dev/null; then
    DESC=$(printf '%s' "$RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("description","unknown error"))' 2>/dev/null || echo "unknown error")
    fail "telegram rejected message: $DESC"
  fi
  echo "sent"
fi
