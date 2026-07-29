#!/usr/bin/env bash
# The cutover run: a real agent client doing real work against dorang, and the
# same work against the LiteLLM dorang would replace.
#
#   ./cutover.sh                     # both legs, default model
#   CUTOVER_MODEL=glm-5.1 ./cutover.sh
#   ./cutover.sh dorang              # one leg only
#   ./cutover.sh litellm
#
# WHY THIS EXISTS ALONGSIDE run.sh
#   `parity.py` compares one request against one request. It cannot see a
#   conversation. Tool calls assembled across streamed chunks, the assistant
#   turn that carries `tool_calls` and no content, the tool-result turn going
#   back up, usage accumulating over a session, the model name surviving the
#   round trip into the next request — all of that is invisible to a request
#   table and all of it is what a client actually does. This script drives
#   jikjicode, which is an agent client that speaks to real providers, and
#   makes dorang the only thing that differs between the two legs.
#
# WHAT IT COSTS
#   One agent session per leg. The task is three tool calls on a five-line
#   file, max_output is capped in the rendered roster, and the orchestration
#   runtime is off so no sub-agent is spawned. Two sessions, not a sweep.
#
# WHAT IT LEAVES BEHIND
#   $RUNDIR (printed at the end) holds both transcripts, dorang's log, and
#   dorang's own ledger for the session. Nothing is written into the repository
#   and nothing under the operator's home directory is modified.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
port="${DORANG_CUTOVER_PORT:-4198}"
legs="${1:-both}"

# shellcheck disable=SC1090
set -a; . ~/env; set +a
: "${LITELLM_OPENAI_BASE_URL:?not set -- source ~/env}"
: "${LITELLM_OPENAI_API_KEY:?not set -- source ~/env}"

command -v jikjicode >/dev/null || { echo "jikjicode not on PATH" >&2; exit 1; }

# The model has to be one the LiteLLM deployment actually serves AND one that
# can drive a tool-calling loop. `run.sh` names which ids answer at all; several
# of the 39 are retired upstream and answer 500 on LiteLLM itself.
model="${CUTOVER_MODEL:-gpt-oss:20b}"

rundir="$(mktemp -d)"
trap 'kill "${dorang_pid:-}" 2>/dev/null || true; echo "artifacts: $rundir"' EXIT

export DORANG_STATE_DIR="$rundir/state"
export DORANG_MASTER_KEY="sk-cutover-$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
export DORANG_KEY_PEPPER="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
mkdir -p "$DORANG_STATE_DIR"

# The same front-proxy configuration the request table runs, plus the one
# setting a cutover needs and a parity table cannot see: `legacy_headers`
# mirrors the reference proxy's response header names, and a dashboard reading
# x-litellm-response-cost does not error when they stop arriving — it reports
# zero. See docs/CONFIG.md §21a.
envsubst '${LITELLM_OPENAI_BASE_URL}' < "$here/dorang.yaml.tmpl" > "$rundir/dorang.yaml"
cat >> "$rundir/dorang.yaml" <<'YAML'

compat:
  legacy_headers: true
YAML

bin="${DORANG_BIN:-$rundir/dorang}"
if [ ! -x "$bin" ]; then
  ( cd "$repo" && go build -o "$bin" ./cmd/dorang )
fi

"$bin" -config "$rundir/dorang.yaml" -check
"$bin" -config "$rundir/dorang.yaml" -listen "127.0.0.1:$port" \
  > "$rundir/dorang.log" 2>&1 &
dorang_pid=$!
for _ in $(seq 1 60); do
  curl -sf -m 2 -o /dev/null "http://127.0.0.1:$port/health/liveliness" && break
  sleep 0.5
done
curl -sf -m 2 -o /dev/null "http://127.0.0.1:$port/health/liveliness" || {
  echo "dorang did not come up:" >&2; cat "$rundir/dorang.log" >&2; exit 1; }

# The task. Small, deterministic, and unachievable without tools: the answer is
# a property of a file the model cannot see until it reads it, and the check is
# a file it must write. Three tool calls minimum, so the tool-call round trip
# happens at least three times in one conversation.
read -r -d '' TASK <<'EOF' || true
Read the file notes.txt in the workspace. Count how many lines it has.
Then write a file summary.md whose entire contents is exactly one line:
notes.txt has N lines
with N replaced by the count. Then read summary.md back and tell me what it says.
EOF

new_workspace() {
  local ws="$rundir/ws-$1"
  mkdir -p "$ws"
  printf 'alpha\nbeta\ngamma\ndelta\nepsilon\n' > "$ws/notes.txt"
  printf '# scratch workspace for the dorang cutover run\n' > "$ws/README.md"
  git -C "$ws" init -q
  git -C "$ws" -c user.email=cutover@localhost -c user.name=cutover add -A
  git -C "$ws" -c user.email=cutover@localhost -c user.name=cutover commit -qm init
  echo "$ws"
}

run_leg() {                      # run_leg <name> <base_url> <key_env>
  local leg="$1" base="$2" keyenv="$3" ws
  ws="$(new_workspace "$leg")"
  CUTOVER_MODEL="$model" CUTOVER_BASE_URL="$base" CUTOVER_KEY_ENV="$keyenv" \
    envsubst '${CUTOVER_MODEL} ${CUTOVER_BASE_URL} ${CUTOVER_KEY_ENV}' \
    < "$here/jikjicode.yaml.tmpl" > "$rundir/jc-$leg.yaml"

  echo "=== leg: $leg  model=$model ==="
  set +e
  jikjicode exec --config "$rundir/jc-$leg.yaml" -C "$ws" \
    --output-format stream-json --include-partial-messages \
    --max-steps 12 --timeout 10m "$TASK" \
    > "$rundir/$leg.jsonl" 2> "$rundir/$leg.err"
  local rc=$?
  set -e
  echo "exit=$rc  events=$(wc -l < "$rundir/$leg.jsonl")  workspace=$ws"
  [ -f "$ws/summary.md" ] && echo "summary.md: $(cat "$ws/summary.md")" \
                          || echo "summary.md: NOT WRITTEN"
  return 0
}

case "$legs" in
  dorang|both)
    run_leg dorang "http://127.0.0.1:$port/v1" DORANG_MASTER_KEY ;;
esac
case "$legs" in
  litellm|both)
    run_leg litellm "$LITELLM_OPENAI_BASE_URL" LITELLM_OPENAI_API_KEY ;;
esac

# dorang's own account of the session, which is the half a client cannot see.
#
# The range is mandatory (DESIGN §9.3 refuses an unbounded ledger search) and so
# is a filter; both spellings are recorded, because the answer to the one
# WITHOUT a filter is itself a finding — see REPORT.md C3.
day="$(date -u +%Y-%m-%d)"
tomorrow="$(date -u -d '+1 day' +%Y-%m-%d)"
auth=(-H "Authorization: Bearer $DORANG_MASTER_KEY")
base="http://127.0.0.1:$port"
curl -s -m 10 "${auth[@]}" \
  "$base/spend/logs?start_date=$day&end_date=$tomorrow&limit=100" \
  > "$rundir/spend-logs-nofilter.json" || true
curl -s -m 10 "${auth[@]}" \
  "$base/spend/logs?start_date=$day&end_date=$tomorrow&limit=100&key_id=master" \
  > "$rundir/spend-logs.json" || true
curl -s -m 10 "${auth[@]}" "$base/metrics" > "$rundir/metrics.txt" || true
echo "--- dorang's own ledger ---"
head -c 400 "$rundir/spend-logs.json"; echo
