#!/usr/bin/env bash
# Render the parity config, start dorang on a scratch state directory, run the
# differential harness against it and the live LiteLLM, then stop dorang.
#
#   ./run.sh                 # the default suite
#   ./run.sh --only errors   # arguments pass through to parity.py
#   ./run.sh --list          # spends nothing
#
# Reads LITELLM_OPENAI_BASE_URL and LITELLM_OPENAI_API_KEY from ~/env. Nothing
# it writes contains a credential: the rendered config still references the key
# by `key_env:`, and only the base URL is substituted (see REPORT.md gap G1).
#
# The state directory is a mktemp -d that is removed on exit, so repeated runs
# never inherit a previous run's ledger, budgets or generated key pepper.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
port="${DORANG_PARITY_PORT:-4199}"

# shellcheck disable=SC1090
set -a; . ~/env; set +a
: "${LITELLM_OPENAI_BASE_URL:?not set -- source ~/env}"
: "${LITELLM_OPENAI_API_KEY:?not set -- source ~/env}"

rundir="$(mktemp -d)"
trap 'kill "${dorang_pid:-}" 2>/dev/null || true; rm -rf "$rundir"' EXIT

export DORANG_STATE_DIR="$rundir/state"
export DORANG_MASTER_KEY="sk-parity-$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
export DORANG_KEY_PEPPER="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
mkdir -p "$DORANG_STATE_DIR"

envsubst '${LITELLM_OPENAI_BASE_URL}' < "$here/dorang.yaml.tmpl" > "$rundir/dorang.yaml"

# $DORANG_BIN reuses an already-built binary; otherwise build one per run so the
# harness is always exercising the working tree rather than a stale artifact.
bin="${DORANG_BIN:-$rundir/dorang}"
if [ ! -x "$bin" ]; then
  ( cd "$repo" && go build -o "$bin" ./cmd/dorang )
fi

"$bin" -config "$rundir/dorang.yaml" -check
"$bin" -config "$rundir/dorang.yaml" -listen "127.0.0.1:$port" \
  > "$rundir/dorang.log" 2>&1 &
dorang_pid=$!

for _ in $(seq 1 60); do
  if curl -sf -m 2 -o /dev/null "http://127.0.0.1:$port/health/liveliness"; then break; fi
  sleep 0.5
done
if ! curl -sf -m 2 -o /dev/null "http://127.0.0.1:$port/health/liveliness"; then
  echo "dorang did not come up:" >&2; cat "$rundir/dorang.log" >&2; exit 1
fi

export DORANG_BASE_URL="http://127.0.0.1:$port"
export DORANG_API_KEY="$DORANG_MASTER_KEY"
python3 "$here/parity.py" "$@"
