#!/usr/bin/env bash
# Start the fake upstream, start dorang against it, run the dual-account proof.
#
#   ./run-dualkey.sh
#
# It spends nothing: the only upstream is deploy/providers/fake_upstream.py.
# The two "credentials" are 32 random bytes generated here, exported by name,
# and never written to a file. Nothing in this directory contains a secret.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
port="${DORANG_DUALKEY_PORT:-4200}"
fake_port="${FAKE_UPSTREAM_PORT:-4290}"

rundir="$(mktemp -d)"
cleanup() {
  kill "${dorang_pid:-}" 2>/dev/null || true
  kill "${fake_pid:-}"  2>/dev/null || true
  rm -rf "$rundir"
}
trap cleanup EXIT

rand() { head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'; }

export FAKE_KEY_A="fake-a-$(rand)"
export FAKE_KEY_B="fake-b-$(rand)"
export FAKE_KEY_C="fake-c-$(rand)"
export FAKE_UPSTREAM_ACCOUNTS="acct-1=FAKE_KEY_A,acct-2=FAKE_KEY_B,plan-acct=FAKE_KEY_C"
export FAKE_UPSTREAM_PORT="$fake_port"
export FAKE_UPSTREAM_URL="http://127.0.0.1:$fake_port/v1"

export DORANG_STATE_DIR="$rundir/state"
export DORANG_MASTER_KEY="sk-dualkey-$(rand)"
export DORANG_KEY_PEPPER="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
mkdir -p "$DORANG_STATE_DIR"

python3 "$here/fake_upstream.py" > "$rundir/fake.log" 2>&1 &
fake_pid=$!
for _ in $(seq 1 40); do
  if curl -sf -m 1 -o /dev/null "http://127.0.0.1:$fake_port/__ctl/state"; then break; fi
  sleep 0.25
done
curl -sf -m 2 -o /dev/null "http://127.0.0.1:$fake_port/__ctl/state" || {
  echo "fake upstream did not come up:" >&2; cat "$rundir/fake.log" >&2; exit 1; }

envsubst '${FAKE_UPSTREAM_URL}' < "$here/dual-key.yaml.tmpl" > "$rundir/dual-key.yaml"

bin="${DORANG_BIN:-$rundir/dorang}"
if [ ! -x "$bin" ]; then ( cd "$repo" && go build -o "$bin" ./cmd/dorang ); fi

"$bin" -config "$rundir/dual-key.yaml" -check
"$bin" -config "$rundir/dual-key.yaml" -listen "127.0.0.1:$port" \
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
python3 "$here/dualkey.py" "$@" || rc=$?

if [ -n "${DORANG_KEEP_LOGS:-}" ]; then
  cp "$rundir/dorang.log" "${DORANG_KEEP_LOGS}/dorang.log" 2>/dev/null || true
  cp "$rundir/fake.log"   "${DORANG_KEEP_LOGS}/fake.log"   2>/dev/null || true
fi
exit "$rc"
