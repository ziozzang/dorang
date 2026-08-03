#!/usr/bin/env bash
# Regenerate the `models:` block of a dorang config from the upstream's own
# model list, so a client sees the same surface through dorang that it saw
# directly.
#
#   ./gen-models.sh <upstream-base-url> <upstream-api-key> <config.yaml>
#
# Everything above the "generated below this line" marker in the config is kept
# verbatim; everything below it is replaced. Run it again whenever the upstream
# model set changes and reload with SIGHUP — dorang hot-reloads the file and
# in-flight requests keep the snapshot they started with.
#
# The API key is a positional argument rather than a flag so it can be supplied
# from an environment variable without appearing in a shell history file:
#
#   ./gen-models.sh http://127.0.0.1:4000/v1 "$UPSTREAM_KEY" ./config/dorang.yaml

set -euo pipefail

BASE=${1:?usage: gen-models.sh <base-url> <api-key> <config.yaml>}
KEY=${2:?usage: gen-models.sh <base-url> <api-key> <config.yaml>}
CFG=${3:?usage: gen-models.sh <base-url> <api-key> <config.yaml>}

MARKER='# --- generated below this line by gen-models.sh'

grep -q -- "$MARKER" "$CFG" || {
  echo "gen-models.sh: $CFG has no generated-block marker; add the line:" >&2
  echo "  $MARKER ---" >&2
  exit 1
}

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

# Keep everything above the marker.
sed -n "1,/$(printf '%s' "$MARKER" | sed 's/[].[^$\\*\/]/\\&/g')/p" "$CFG" > "$tmp"

curl -fsS -m 30 -H "Authorization: Bearer $KEY" "$BASE/models" \
  | python3 -c '
import json, sys
ids = sorted({m["id"] for m in json.load(sys.stdin)["data"]})
if not ids:
    sys.exit("gen-models.sh: upstream returned no models")
print("models:")
for m in ids:
    q = json.dumps(m)          # a model id is opaque: quote it, never split it
    print(f"  - name: {q}")
    print( "    deployments:")
    print(f"      - {{provider: litellm, upstream_model: {q}, credentials: [litellm-1]}}")
sys.stderr.write(f"gen-models.sh: {len(ids)} models\n")
' >> "$tmp"

cat "$tmp" > "$CFG"
