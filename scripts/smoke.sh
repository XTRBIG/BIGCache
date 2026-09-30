#!/usr/bin/env bash
# End-to-end smoke test: file-backed cache + two file-backed "HDDs", served
# over NBD on localhost, exercised with the built-in NBD client via
# 'go test', then shut down and inspected. No root required.
set -euo pipefail
cd "$(dirname "$0")/.."
WORK=$(mktemp -d)
trap 'kill $SERVE_PID 2>/dev/null || true; rm -rf "$WORK"' EXIT

go build -o "$WORK/bigcache" ./cmd/bigcache
truncate -s 64M "$WORK/hdd1.img"
truncate -s 32M "$WORK/hdd2.img"
cat > "$WORK/config.json" <<JSON
{
  "cache_device": "$WORK/ssd.img",
  "cache_size": "16M",
  "block_size": "16K",
  "listen": "127.0.0.1:20809",
  "control_listen": "127.0.0.1:20810",
  "flush_interval": "1s",
  "volumes": [
    {"name": "hdd1", "device": "$WORK/hdd1.img", "write_policy": "writeback"},
    {"name": "hdd2", "device": "$WORK/hdd2.img", "write_policy": "writethrough"}
  ]
}
JSON

"$WORK/bigcache" init -c "$WORK/config.json"
"$WORK/bigcache" inspect -c "$WORK/config.json"
"$WORK/bigcache" serve -c "$WORK/config.json" > "$WORK/serve.log" 2>&1 &
SERVE_PID=$!
for i in $(seq 1 50); do
  curl -sf http://127.0.0.1:20810/healthz >/dev/null 2>&1 && break
  sleep 0.1
done
curl -sf http://127.0.0.1:20810/healthz >/dev/null || { cat "$WORK/serve.log"; echo "server did not start"; exit 1; }

"$WORK/bigcache" list --server 127.0.0.1:20809 | tee "$WORK/list.txt"
grep -qx hdd1 "$WORK/list.txt" && grep -qx hdd2 "$WORK/list.txt"

# Drive the export with the NBD client from the test suite.
BIGCACHE_SMOKE_ADDR=127.0.0.1:20809 go test -count=1 -run TestSmokeNBD ./internal/nbd/ -v 2>&1 | grep -E "^(=== RUN|--- |PASS|FAIL|ok)"

"$WORK/bigcache" stats --control 127.0.0.1:20810
"$WORK/bigcache" flush --control 127.0.0.1:20810 --volume hdd1
"$WORK/bigcache" stats --control 127.0.0.1:20810 --json | grep -q '"dirty_slots": 0'

kill -TERM $SERVE_PID
wait $SERVE_PID
grep -q "cache closed cleanly" "$WORK/serve.log" || { cat "$WORK/serve.log"; exit 1; }
"$WORK/bigcache" inspect -c "$WORK/config.json" | tee "$WORK/inspect.txt"
grep -q "clean shutdown: true" "$WORK/inspect.txt"
# The pattern written through NBD must be on the HDD image now.
python3 - "$WORK/hdd1.img" <<'PY'
import sys
d = open(sys.argv[1], 'rb').read()
assert d[1_000_000:1_000_000+8] == b'BIGCACHE', d[1_000_000:1_000_000+8]
print("HDD image contains the data written through NBD")
PY
echo "SMOKE TEST PASSED"
