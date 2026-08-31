#!/usr/bin/env bash
# Run everything that can be checked without hardware or a network peer.
#
# One command, because a spike's evidence is worthless if reproducing it needs
# a tour. What this does NOT cover is the two things that need the world: R1's
# tunnel run (spike/r1-websocket) and R2/R5, which need an Android device and a
# willing friend.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DART="${DART:-$HOME/tools/dart-sdk/bin}"
[ -d "$DART" ] && export PATH="$DART:$PATH"

fails=0
step() {
  printf '\n\033[1m== %s\033[0m\n' "$1"
}
result() {
  if [ "$1" -eq 0 ]; then
    printf '   \033[32mPASS\033[0m %s\n' "$2"
  else
    printf '   \033[31mFAIL\033[0m %s\n' "$2"
    fails=$((fails + 1))
  fi
}

step "R4 · codegen is current (the generated files match the schema)"
(cd "$ROOT/web" && npm run --silent gen:check) >/tmp/v.log 2>&1
result $? "gen:check"

step "R4 · TypeScript"
(cd "$ROOT/web" && npx --no-install vue-tsc --noEmit) >/tmp/v.log 2>&1
result $? "vue-tsc --noEmit"
(cd "$ROOT/web" && npm run --silent conformance) >/tmp/v-ts.log 2>&1
result $? "$(grep -oE 'all green — [0-9]+ vectors|[0-9]+ of [0-9]+ FAILED' /tmp/v-ts.log | tail -1)"

step "R4 · Dart"
if command -v dart >/dev/null; then
  (cd "$ROOT/dart" && dart analyze) >/tmp/v.log 2>&1
  result $? "dart analyze"
  (cd "$ROOT/dart" && dart run bin/conformance.dart) >/tmp/v-dart.log 2>&1
  result $? "$(grep -oE 'all green — [0-9]+ vectors|[0-9]+ of [0-9]+ FAILED' /tmp/v-dart.log | tail -1)"
else
  printf '   \033[33mSKIP\033[0m dart not on PATH (set DART=/path/to/dart-sdk/bin)\n'
fi

step "R4 · Go"
(cd "$ROOT/server" && go vet ./...) >/tmp/v.log 2>&1
result $? "go vet"
(cd "$ROOT/server" && go run ./cmd/conformance) >/tmp/v-go.log 2>&1
result $? "$(grep -oE 'all green — .*vectors|[0-9]+ of [0-9]+ FAILED' /tmp/v-go.log | tail -1)"

step "R4 · the staleness guard actually fails the build"
cp "$ROOT/web/src/wire/generated.ts" /tmp/generated.ts.bak
printf '\n// hand edit\n' >> "$ROOT/web/src/wire/generated.ts"
(cd "$ROOT/web" && npm run --silent gen:check) >/dev/null 2>&1
rc=$?
cp /tmp/generated.ts.bak "$ROOT/web/src/wire/generated.ts"
[ $rc -ne 0 ]; result $? "a hand edit makes gen:check exit non-zero"

step "R4 · web smoke test (render assertions + conformance)"
(cd "$ROOT/web" && npm run --silent smoke) >/tmp/v-smoke.log 2>&1
result $? "$(grep -cE '^ok  ' /tmp/v-smoke.log) assertions passed"

step "R4 · a REAL server response validates against the generated decoder"
(cd "$ROOT/web" && npm run --silent validate SyncResponse "$ROOT/spike/r1-websocket/captured-sync-response.json") >/tmp/v.log 2>&1
result $? "captured /sync from the Go rig decodes as SyncResponse"

step "R6 · Purser connector stub"
(cd "$ROOT/spike/r6-purser" && go test ./...) >/tmp/v-r6.log 2>&1
result $? "go test (7 tests against the real connector.Connector)"

step "R3 · whisper.cpp results are on record"
[ -s "$ROOT/spike/r3-whisper/results/bench.csv" ]
result $? "spike/r3-whisper/results/bench.csv"

printf '\n'
if [ "$fails" -eq 0 ]; then
  printf '\033[32mall green\033[0m\n'
else
  printf '\033[31m%d step(s) failed\033[0m — logs in /tmp/v-*.log\n' "$fails"
fi
exit $((fails > 0))
