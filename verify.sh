#!/usr/bin/env bash
# Run everything that can be checked without hardware or a network peer.
#
# One command, because a spike's evidence is worthless if reproducing it needs
# a tour. What this does NOT cover at all is the things that need the world:
# R1's tunnel run (spike/r1-websocket), and R2/R5, which need an Android device
# and a willing friend. There are no steps for those.
#
# THREE STEPS HERE SKIP rather than fail when what they need is absent, and a
# skip is not a pass:
#
#   Dart          both the analyze and the conformance runner, when `dart` is
#                 not on PATH or at $DART.
#   the database  the store's schema tests, when CATENARY_TEST_DATABASE_URL is
#                 unset. `go test` still runs; those tests call t.Skip.
#   R6            when the Purser checkout the spike's `replace` points at is
#                 absent, which is every machine but one.
#
# They skip rather than fail because CI runs this file whole, and a step that
# can never pass there is a red light everyone learns to ignore. CI forces the
# first two present — `setup-dart` and the Postgres service — so only R6
# actually skips there.
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
# CANT-15: gofmt-clean AND byte-identical to a fresh generator run AND green,
# all three at once. Any two of them were always easy; the third is why the
# formatting had to move into the generator rather than onto the file.
(cd "$ROOT/server" && gofmt -l .) >/tmp/v-fmt-server.log 2>&1
[ ! -s /tmp/v-fmt-server.log ]; result $? "gofmt -l server/ is empty$( [ -s /tmp/v-fmt-server.log ] && printf ' (%s)' "$(tr '\n' ' ' </tmp/v-fmt-server.log)" )"
(cd "$ROOT/server" && go vet ./...) >/tmp/v.log 2>&1
result $? "go vet"
# CANT-11: verify.sh used to run `go test ./...` for spike/r6-purser only, while
# CLAUDE.md's Testing section names it for the whole tree. If CI ran it and this
# did not, the two would diverge on the first Go test anyone wrote here — and
# "./verify.sh green before anything is handed over" would stop being the same
# claim as green CI.
(cd "$ROOT/server" && go test ./...) >/tmp/v-go-test.log 2>&1
result $? "go test ./... ($(grep -c 'no test files\|^ok' /tmp/v-go-test.log) packages)"
(cd "$ROOT/server" && go run ./cmd/conformance) >/tmp/v-go.log 2>&1
result $? "$(grep -oE 'all green — .*vectors|[0-9]+ of [0-9]+ FAILED' /tmp/v-go.log | tail -1)"

step "R4 · the staleness guard actually fails the build — once per generated file"
# CANT-12 criterion 3: proved PER PIPELINE, not once overall. The check has
# always walked every target; the PROOF used to touch one file, so three of the
# four were covered by assumption. openapi.yaml joining the set is exactly the
# case that would have gone unnoticed.
for gen in web/src/wire/generated.ts dart/lib/src/generated.dart server/internal/wire/generated.go schema/openapi.yaml; do
  cp "$ROOT/$gen" /tmp/gen.bak
  printf '\n# hand edit\n' >> "$ROOT/$gen"
  (cd "$ROOT/web" && npm run --silent gen:check) >/dev/null 2>&1
  rc=$?
  cp /tmp/gen.bak "$ROOT/$gen"
  [ $rc -ne 0 ]; result $? "a hand edit to $(basename "$gen") makes gen:check exit non-zero"
done

step "R4 · web smoke test (render assertions + conformance)"
(cd "$ROOT/web" && npm run --silent smoke) >/tmp/v-smoke.log 2>&1
result $? "$(grep -cE '^ok  ' /tmp/v-smoke.log) assertions passed"

step "R4 · a REAL server response validates against the generated decoder"
(cd "$ROOT/web" && npm run --silent validate SyncResponse "$ROOT/spike/r1-websocket/captured-sync-response.json") >/tmp/v.log 2>&1
result $? "captured /sync from the Go rig decodes as SyncResponse"

step "CANT-17/13 · the service binary — vet, gofmt, test"
(cd "$ROOT" && gofmt -l ./cmd ./internal ./migrations) >/tmp/v-fmt.log 2>&1
[ ! -s /tmp/v-fmt.log ]; result $? "gofmt -l is empty$( [ -s /tmp/v-fmt.log ] && printf ' (%s)' "$(tr '\n' ' ' </tmp/v-fmt.log)" )"
(cd "$ROOT" && go vet ./...) >/tmp/v.log 2>&1
result $? "go vet"

# The store's tests need a real Postgres 16 and there is deliberately no DSN
# baked in: a test that silently points at a developer's own database is a test
# that eventually drops it. Everything else in the root module still runs.
if [ -n "${CATENARY_TEST_DATABASE_URL:-}" ]; then
  (cd "$ROOT" && go test ./...) >/tmp/v-go-svc.log 2>&1
  result $? "go test ./... (with a database)"
else
  (cd "$ROOT" && go test ./...) >/tmp/v-go-svc.log 2>&1
  result $? "go test ./... ($(grep -c 'no test files\|^ok' /tmp/v-go-svc.log) packages)"
  printf '   \033[33mNOTE\033[0m CATENARY_TEST_DATABASE_URL unset — the schema tests skipped.\n'
  printf '        docker run -d --name cant-pg -e POSTGRES_HOST_AUTH_METHOD=trust -e POSTGRES_DB=catenary_test -p 55440:5432 postgres:16-alpine\n'
  printf '        export CATENARY_TEST_DATABASE_URL=postgres://postgres@127.0.0.1:55440/catenary_test?sslmode=disable\n'
fi

step "CANT-13 · log_seq is never described as per-account"
# The wire schema records "account-global" as a CORRECTED earlier draft: it
# reads as a per-account counter, which would be dense, and that collapses the
# whole division of labour between the two ordinals. It had reached seven
# places before it was caught, so this greps rather than trusts.
#
# Matched over a rolling 3-line WINDOW, not per line: in the generated wire
# files the correcting sentence wraps, so the phrase and the words that correct
# it land on different lines and a per-line grep reports the correction as the
# crime. (The first attempt at this used `git grep -B2` piped into an awk
# paragraph split, which silently matched nothing at all — the separator git
# emits is `--`, not a blank line. It is checked against a planted line below.)
#
# Two exemptions, both deliberate:
#   spike/    dated evidence and read-only history. Rewriting a report to say
#             something it did not say at the time is its own dishonesty;
#             SPIKE-RESULTS.md carries the correction note instead.
#   verify.sh this guard, which has to name the phrase in order to forbid it.
scan_account_global() {
  git -C "$ROOT" grep -Il --untracked "account-global" -- . ':!spike' ':!verify.sh' 2>/dev/null | while read -r f; do
    awk -v F="$f" '
      { c=b; b=a; a=$0
        if (a ~ /account-global/) {
          w = c " " b " " a
          if (w !~ /earlier draft|known-wrong|corrected|NOT ONE PER ACCOUNT|superseded/)
            printf "%s:%d\n", F, NR
        }
      }' "$ROOT/$f" 2>/dev/null
  done
}
bad=$(scan_account_global)
[ -z "$bad" ]; result $? "no uncorrected use$( [ -n "$bad" ] && printf ' — %s' "$(echo "$bad" | tr '\n' ' ')" )"

# And the guard is proved to bite, because the version before it did not.
printf 'log_seq is account-global.\n' > "$ROOT/.guardprobe.md"
probe=$(scan_account_global)
rm -f "$ROOT/.guardprobe.md"
[ -n "$probe" ]; result $? "a planted line makes it fail"

step "R6 · Purser connector stub"
# R6 needs a PEER REPOSITORY, which the header's "needs the world" list did not
# name. Purser's connector contract lives in its `internal/connector`, so Go
# only lets a package whose import path is inside `github.com/Einlanzerous/purser/...`
# implement it — which is why the spike's go.mod takes that module path and
# `replace`s it at a local checkout. That replace is an absolute path to this
# machine, so the step is machine-local by construction and cannot run anywhere
# the Purser source is absent.
#
# Skipped rather than failed in that case. The alternative is a CI lane that can
# never go green, and a red step everyone learns to ignore is worse than an
# honest skip. R6 is cleared evidence in `spike/`, not a gate on Catenary's own
# code; nothing in this repository can break it.
r6_replace=$(sed -n 's/^replace .* => \(.*\)$/\1/p' "$ROOT/spike/r6-purser/go.mod" | tr -d ' ')
if [ -n "$r6_replace" ] && [ ! -d "$r6_replace" ]; then
  printf '   \033[33mSKIP\033[0m Purser checkout not at %s — R6 needs the peer repo to compile.\n' "$r6_replace"
else
  (cd "$ROOT/spike/r6-purser" && go test ./...) >/tmp/v-r6.log 2>&1
  result $? "go test (7 tests against the real connector.Connector)"
fi

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
