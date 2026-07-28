#!/usr/bin/env bash
# Render providers.yaml.tmpl, start dorang on a scratch state directory, run
# the bring-up probe against every real provider, then stop dorang.
#
#   ./run-providers.sh                # every case
#   ./run-providers.sh --only jina    # arguments pass through to probe.py
#   ./run-providers.sh --list         # spends nothing
#
# THIS SPENDS REAL MONEY AND REAL PLAN QUOTA. The probe sends one small request
# per endpoint family per provider, with max_tokens in the low tens, and no
# case is ever repeated. Read probe.py --list before running it.
#
# Nothing it writes contains a credential: the rendered config still references
# every key by `key_env:`, only base URLs are substituted, and the render lives
# in a mktemp -d that is removed on exit.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
port="${DORANG_PROVIDERS_PORT:-4300}"

if [ "${1:-}" = "--list" ]; then
  exec python3 "$here/probe.py" --list
fi

# shellcheck disable=SC1090
set -a; . ~/env; set +a
for v in OLLAMA_API_KEY OLLAMA_API_KEY_2 Z_AI_API_KEY Z_AI_ANTHROPIC_BASE_URL \
         ANTHROPIC_AUTH_TOKEN ANTHROPIC_BASE_URL QWEN_TOKEN_PLAN_KEY \
         QWEN_TOKEN_PLAN_ANTHROPIC_URL OPENROUTER_API_KEY JINA_API_KEY \
         HF_TOKEN LITELLM_OPENAI_API_KEY LITELLM_OPENAI_BASE_URL; do
  if [ -z "${!v:-}" ]; then echo "$v is not set -- source ~/env" >&2; exit 1; fi
done

rundir="$(mktemp -d)"
trap 'kill "${dorang_pid:-}" 2>/dev/null || true; rm -rf "$rundir"' EXIT

export DORANG_STATE_DIR="$rundir/state"
export DORANG_MASTER_KEY="sk-providers-$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
export DORANG_KEY_PEPPER="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
mkdir -p "$DORANG_STATE_DIR"

envsubst '${Z_AI_ANTHROPIC_BASE_URL} ${ANTHROPIC_BASE_URL} ${QWEN_TOKEN_PLAN_ANTHROPIC_URL} ${LITELLM_OPENAI_BASE_URL}' \
  < "$here/providers.yaml.tmpl" > "$rundir/providers.yaml"

bin="${DORANG_BIN:-$rundir/dorang}"
if [ ! -x "$bin" ]; then ( cd "$repo" && go build -o "$bin" ./cmd/dorang ); fi

"$bin" -config "$rundir/providers.yaml" -check
"$bin" -config "$rundir/providers.yaml" -listen "127.0.0.1:$port" \
  > "$rundir/dorang.log" 2>&1 &
dorang_pid=$!

for _ in $(seq 1 60); do
  if curl -sf -m 2 -o /dev/null "http://127.0.0.1:$port/health/liveliness"; then break; fi
  sleep 0.5
done
curl -sf -m 2 -o /dev/null "http://127.0.0.1:$port/health/liveliness" || {
  echo "dorang did not come up:" >&2; cat "$rundir/dorang.log" >&2; exit 1; }

export DORANG_BASE_URL="http://127.0.0.1:$port"
export DORANG_API_KEY="$DORANG_MASTER_KEY"
rc=0
python3 "$here/probe.py" "$@" || rc=$?

if [ -n "${DORANG_KEEP_LOGS:-}" ]; then
  cp "$rundir/dorang.log" "${DORANG_KEEP_LOGS}/providers-dorang.log" 2>/dev/null || true
fi
exit "$rc"
