#!/usr/bin/env bash
# R1 / IDEA-24, second half of the exit criterion:
#
#   "A forced kill (drop the network, kill the app, sever the tunnel) resumes
#    with zero loss and zero duplication, verified by comparing the client's
#    message set against the server's log for that conversation."
#
# The shape of the test matters. It is not "restart it and see if it works" —
# messages are published DURING the window when the client is dead, so there is
# something to lose. Then the client's durable journal is compared against the
# server's own log, set against set.
#
# kill -9, not a graceful stop: a clean shutdown would let the client flush
# state it will not have in the real failure, which is a phone going into a
# tunnel.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="${BIN:-/home/magos/projects/catenary/server/bin}"
URL="$(cat "$HERE/.url2")"
TOKEN="$(cat "$HERE/.token2")"
NAME=kill
WORK="$HERE/killtest"

rm -rf "$WORK"; mkdir -p "$WORK"

pub() { # pub <n> <tag>
  for i in $(seq 1 "$1"); do
    curl -s -m 20 -H "Authorization: Bearer $TOKEN" \
      "$URL/publish?client_id=$(uuid_for "$2$i")&text=$2$i" >/dev/null || echo "publish failed: $2$i" >&2
  done
}

# Deterministic v4-shaped uuid from a label, so a republish is the SAME
# idempotency key and the server's dedup path is exercised rather than bypassed.
uuid_for() {
  local h; h=$(printf '%s' "$1" | md5sum | cut -c1-32)
  printf '%s-%s-4%s-8%s-%s' "${h:0:8}" "${h:8:4}" "${h:13:3}" "${h:17:3}" "${h:20:12}"
}

echo "== phase 1: 5 messages, client running =="
pub 5 a
"$BIN/r1client" -url "$URL" -token "$TOKEN" -dir "$WORK" -name "$NAME" \
  -status-every 30s -run-for 12s >"$WORK/run1.log" 2>&1
echo "journal after phase 1: $(wc -l < "$WORK/$NAME.journal") lines, cursor $(cat "$WORK/$NAME.cursor")"

echo
echo "== phase 2: client running, kill -9 mid-stream =="
"$BIN/r1client" -url "$URL" -token "$TOKEN" -dir "$WORK" -name "$NAME" \
  -status-every 30s -run-for 120s >"$WORK/run2.log" 2>&1 &
CLIENT_PID=$!
sleep 4
pub 5 b
sleep 2
kill -9 $CLIENT_PID 2>/dev/null
wait $CLIENT_PID 2>/dev/null
echo "killed pid $CLIENT_PID; journal now $(wc -l < "$WORK/$NAME.journal") lines, cursor $(cat "$WORK/$NAME.cursor")"

echo
echo "== phase 3: 10 more messages while the client is dead =="
pub 10 c
echo "server head is now $(curl -s "$URL/healthz" | grep -oE '"head":[0-9]+' | cut -d: -f2)"

echo
echo "== phase 4: restart, resume from cursor =="
"$BIN/r1client" -url "$URL" -token "$TOKEN" -dir "$WORK" -name "$NAME" \
  -status-every 30s -run-for 15s >"$WORK/run3.log" 2>&1
echo "journal after resume: $(wc -l < "$WORK/$NAME.journal") lines, cursor $(cat "$WORK/$NAME.cursor")"

echo
echo "== phase 5: republish the same idempotency keys =="
# A retry after an ambiguous failure must be free. Same client_id => the server
# replays the original ack and creates no second message.
pub 5 b
echo "server head after republish: $(curl -s "$URL/healthz" | grep -oE '"head":[0-9]+' | cut -d: -f2)"

echo
echo "== verify: client journal vs server log =="
curl -s -H "Authorization: Bearer $TOKEN" "$URL/sync?after=0&limit=1000" \
  | python3 -c 'import sys,json; [print(m["log_seq"], m["id"], sep="\t") for m in json.load(sys.stdin)["messages"]]' \
  > "$WORK/server.log"

python3 - "$WORK/$NAME.journal" "$WORK/server.log" <<'PY'
import sys, collections
jpath, spath = sys.argv[1], sys.argv[2]
j = [l.strip() for l in open(jpath) if l.strip()]
s = [l.strip() for l in open(spath) if l.strip()]

jc = collections.Counter(j)
dupes = {k: v for k, v in jc.items() if v > 1}
missing = [x for x in s if x not in jc]
extra = [x for x in j if x not in set(s)]

print(f"server messages : {len(s)}")
print(f"client journal  : {len(j)} lines, {len(jc)} distinct")
print(f"DUPLICATED      : {len(dupes)}  {list(dupes)[:5]}")
print(f"LOST            : {len(missing)} {missing[:5]}")
print(f"PHANTOM         : {len(extra)}  {extra[:5]}")

ordered = j == sorted(j, key=lambda l: int(l.split('\t')[0]))
print(f"journal in seq order: {ordered}")

ok = not dupes and not missing and not extra and ordered
print()
print("RESULT:", "PASS — zero loss, zero duplication" if ok else "FAIL")
sys.exit(0 if ok else 1)
PY
