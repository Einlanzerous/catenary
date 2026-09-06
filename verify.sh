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

# ONE DIRECTORY PER RUN. These logs are read AFTER the step that wrote them, so
# two runs must not be able to read or truncate each other's.
#
# CANT-87. The paths used to be fixed — ten names, and one of them shared by six
# steps — while two gates assert on log EMPTINESS rather than on an exit status:
#
#     (cd "$ROOT/server" && gofmt -l .) >LOG
#     [ ! -s LOG ]; result $? "gofmt -l server/ is empty"
#
# `>` truncates on open, so run B's redirect landing between run A's gofmt and
# run A's `-s` test made A read an empty file and print PASS on a gate that had
# just failed — with an empty parenthetical, so the output did not even hint at
# it. CLAUDE.md prescribes one worktree per epic, which makes two runs at once
# the normal case and not an exotic one; the CANT-12 comment further down
# already reasoned exactly this way about the staleness-proof backups, and this
# is the same argument applied to the logs that reasoning left behind.
new_logdir() { mktemp -d "${TMPDIR:-/tmp}/catenary-verify.XXXXXX"; }
# CHECKED, and this file runs without `set -e` so it has to be. An unwritable
# TMPDIR or a full /tmp leaves LOGDIR empty, every redirect below fails, and the
# two emptiness gates then test a file that was never created — which is the
# identical false PASS this whole change exists to remove, reintroduced by its
# own fix. Fourteen other steps would go red, so a run could not report all
# green; the two that would lie are exactly the two that matter.
LOGDIR="$(new_logdir)" || { printf 'verify.sh: cannot create a log directory under %s\n' "${TMPDIR:-/tmp}" >&2; exit 1; }

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
(cd "$ROOT/web" && npm run --silent gen:check) >"$LOGDIR/v-gen-check.log" 2>&1
result $? "gen:check"

step "R4 · TypeScript"
(cd "$ROOT/web" && npx --no-install vue-tsc --noEmit) >"$LOGDIR/v-tsc.log" 2>&1
result $? "vue-tsc --noEmit"
(cd "$ROOT/web" && npm run --silent conformance) >"$LOGDIR/v-ts.log" 2>&1
result $? "$(grep -oE 'all green — [0-9]+ vectors|[0-9]+ of [0-9]+ FAILED' "$LOGDIR/v-ts.log" | tail -1)"

step "R4 · Dart"
if command -v dart >/dev/null; then
  (cd "$ROOT/dart" && dart analyze) >"$LOGDIR/v-dart-analyze.log" 2>&1
  result $? "dart analyze"
  (cd "$ROOT/dart" && dart run bin/conformance.dart) >"$LOGDIR/v-dart.log" 2>&1
  result $? "$(grep -oE 'all green — [0-9]+ vectors|[0-9]+ of [0-9]+ FAILED' "$LOGDIR/v-dart.log" | tail -1)"
else
  printf '   \033[33mSKIP\033[0m dart not on PATH (set DART=/path/to/dart-sdk/bin)\n'
fi

step "R4 · Go"
# CANT-15: gofmt-clean AND byte-identical to a fresh generator run AND green,
# all three at once. Any two of them were always easy; the third is why the
# formatting had to move into the generator rather than onto the file.
#
# CANT-82 moved the generated file OUT of this module, so this line no longer
# makes the gofmt half of that claim — `gofmt -l ./internal` in the service
# step below does, and `gen:check` above makes the byte-identical half. What is
# left here is the three importers and the spike binaries, which is still worth
# checking and is no longer the sentence above it.
(cd "$ROOT/server" && gofmt -l .) >"$LOGDIR/v-fmt-server.log" 2>&1
[ ! -s "$LOGDIR/v-fmt-server.log" ]; result $? "gofmt -l server/ is empty$( [ -s "$LOGDIR/v-fmt-server.log" ] && printf ' (%s)' "$(tr '\n' ' ' <"$LOGDIR/v-fmt-server.log")" )"
(cd "$ROOT/server" && go vet ./...) >"$LOGDIR/v-vet-server.log" 2>&1
result $? "go vet"
# CANT-11: verify.sh used to run `go test ./...` for spike/r6-purser only, while
# CLAUDE.md's Testing section names it for the whole tree. If CI ran it and this
# did not, the two would diverge on the first Go test anyone wrote here — and
# "./verify.sh green before anything is handed over" would stop being the same
# claim as green CI.
(cd "$ROOT/server" && go test ./...) >"$LOGDIR/v-go-test.log" 2>&1
result $? "go test ./... ($(grep -c 'no test files\|^ok' "$LOGDIR/v-go-test.log") packages)"
(cd "$ROOT/server" && go run ./cmd/conformance) >"$LOGDIR/v-go.log" 2>&1
result $? "$(grep -oE 'all green — .*vectors|[0-9]+ of [0-9]+ FAILED' "$LOGDIR/v-go.log" | tail -1)"

step "R4 · the staleness guard actually fails the build — once per generated file"
# CANT-12 criterion 3: proved PER PIPELINE, not once overall. The check has
# always walked every target; the PROOF used to touch one file, so three of the
# four were covered by assumption. openapi.yaml joining the set is exactly the
# case that would have gone unnoticed.
# ONE BACKUP PER FILE, and a trap. The single-file version this replaces used
# /tmp/generated.ts.bak, so two concurrent runs collided on a name and restored
# IDENTICAL bytes — harmless. Widening the proof to four files while keeping one
# name would make the collision cross-file: run A backs up generated.go, run B
# overwrites the backup with generated.ts, run A restores TypeScript into
# generated.go. The loop cannot notice, because gen:check walks every target and
# the corruption it just caused keeps the assertion passing. A check that goes
# green BECAUSE of the damage it did is worse than no check, and CLAUDE.md
# prescribes one worktree per epic, so concurrent runs are the normal case here.
#
# The trap covers the other half: this file runs without `set -e`, so a ^C or a
# failing cp between the append and the restore leaves a hand-edited generated
# file behind — now four times per run rather than once.
_gen_restore() { [ -n "${_gen_bak:-}" ] && [ -f "$_gen_bak" ] && cp "$_gen_bak" "$ROOT/$_gen_cur" && rm -f "$_gen_bak"; }
trap _gen_restore EXIT INT TERM
for gen in web/src/wire/generated.ts dart/lib/src/generated.dart internal/wire/generated.go schema/openapi.yaml; do
  _gen_cur="$gen"; _gen_bak="$(mktemp "${TMPDIR:-/tmp}/$(basename "$gen").XXXXXX.bak")"
  cp "$ROOT/$gen" "$_gen_bak"
  printf '\n# hand edit\n' >> "$ROOT/$gen"
  (cd "$ROOT/web" && npm run --silent gen:check) >/dev/null 2>&1
  rc=$?
  cp "$_gen_bak" "$ROOT/$gen"; rm -f "$_gen_bak"; _gen_bak=""
  [ $rc -ne 0 ]; result $? "a hand edit to $(basename "$gen") makes gen:check exit non-zero"
done
trap - EXIT INT TERM

step "R4 · web smoke test (render assertions + conformance)"
(cd "$ROOT/web" && npm run --silent smoke) >"$LOGDIR/v-smoke.log" 2>&1
result $? "$(grep -cE '^ok  ' "$LOGDIR/v-smoke.log") assertions passed"

step "R4 · a REAL server response validates against the generated decoder"
(cd "$ROOT/web" && npm run --silent validate SyncResponse "$ROOT/spike/r1-websocket/captured-sync-response.json") >"$LOGDIR/v-validate.log" 2>&1
result $? "captured /sync from the Go rig decodes as SyncResponse"

step "CANT-17/13 · the service binary — vet, gofmt, test"
(cd "$ROOT" && gofmt -l ./cmd ./internal ./migrations) >"$LOGDIR/v-fmt.log" 2>&1
[ ! -s "$LOGDIR/v-fmt.log" ]; result $? "gofmt -l is empty$( [ -s "$LOGDIR/v-fmt.log" ] && printf ' (%s)' "$(tr '\n' ' ' <"$LOGDIR/v-fmt.log")" )"
(cd "$ROOT" && go vet ./...) >"$LOGDIR/v-vet.log" 2>&1
result $? "go vet"

# The store's tests need a real Postgres 16 and there is deliberately no DSN
# baked in: a test that silently points at a developer's own database is a test
# that eventually drops it. Everything else in the root module still runs.
if [ -n "${CATENARY_TEST_DATABASE_URL:-}" ]; then
  (cd "$ROOT" && go test ./...) >"$LOGDIR/v-go-svc.log" 2>&1
  result $? "go test ./... (with a database — includes CANT-19's log_seq commit-ordering property)"
else
  (cd "$ROOT" && go test ./...) >"$LOGDIR/v-go-svc.log" 2>&1
  result $? "go test ./... ($(grep -c 'no test files\|^ok' "$LOGDIR/v-go-svc.log") packages)"
  printf '   \033[33mNOTE\033[0m CATENARY_TEST_DATABASE_URL unset — the schema and log_seq ordering tests skipped.\n'
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

step "CANT-87 · this run's logs cannot be read or truncated by another run"
# The bug this replaced was a check that went GREEN because a concurrent run
# damaged its input — which the CANT-12 comment above calls worse than no check
# at all. So the fix gets the same treatment as the guard above it: proved here,
# not asserted in a commit message.
#
# TWO properties, because either one alone was already true while the bug was
# live. The allocator has to hand out a fresh directory per run, AND every step
# has to actually use it: a unique directory that nothing writes to fixes
# nothing.
other="$(new_logdir)"
[ "$other" != "$LOGDIR" ]; result $? "a second run gets its own log directory"
printf 'planted\n' > "$LOGDIR/collide.log"
: > "$other/collide.log"
[ -s "$LOGDIR/collide.log" ]; result $? "the other run's redirect cannot truncate this run's log"
rm -rf "$other" "$LOGDIR/collide.log"

# And grep rather than trust, in the idiom of the guard above: a step added
# later with a hardcoded path reintroduces the bug, and the two assertions
# above would both still pass. Comment prose is exempt by construction —
# `[^#]*` cannot cross a `#`, so only a real code occurrence matches.
# ANY fixed log path, not just the ten names that happened to exist. The
# assertion below claims "no step writes to a fixed path", and a step added
# later as `>/tmp/gen.log` would have sailed past a `v`-anchored pattern under a
# green guard. Neither remaining /tmp literal in this file is a false positive:
# both are `${TMPDIR:-/tmp}`-prefixed, which is `/tmp}` and not `/tmp/`.
scan_fixed_logs() { grep -nE '^[^#]*(>|<|[[:space:]])/tmp/[a-z0-9._-]*\.log' "$1"; }
bad_logs=$(scan_fixed_logs "$ROOT/verify.sh")
[ -z "$bad_logs" ]; result $? "no step writes to a fixed path$( [ -n "$bad_logs" ] && printf ' — %s' "$(echo "$bad_logs" | tr '\n' ' ')" )"

# Assembled rather than written out, so this line does not match the scan it is
# proving — the account-global guard above solves the same self-reference by
# exempting verify.sh from its own tree scan. (It caught this line on its first
# run, which is as good a demonstration that the grep bites as the probe is.)
printf 'foo >%s/v-planted.log 2>&1\nbar >%s/gen.log 2>&1\nbaz >%s/build-output.log 2>&1\n' /tmp /tmp /tmp > "$LOGDIR/probe.sh"
probe_logs=$(scan_fixed_logs "$LOGDIR/probe.sh")
rm -f "$LOGDIR/probe.sh"
[ "$(printf '%s\n' "$probe_logs" | grep -c .)" -eq 3 ]; result $? "three planted fixed paths all make it fail — not just the old v-*.log shape"

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
  (cd "$ROOT/spike/r6-purser" && go test ./...) >"$LOGDIR/v-r6.log" 2>&1
  result $? "go test (7 tests against the real connector.Connector)"
fi

step "R3 · whisper.cpp results are on record"
[ -s "$ROOT/spike/r3-whisper/results/bench.csv" ]
result $? "spike/r3-whisper/results/bench.csv"

printf '\n'
if [ "$fails" -eq 0 ]; then
  printf '\033[32mall green\033[0m\n'
  # Removed only on success. A failing run's logs are the whole point of the
  # line below, and a green run leaving one directory behind per invocation
  # would fill /tmp for the same reason the fixed names never did.
  rm -rf "$LOGDIR"
else
  printf '\033[31m%d step(s) failed\033[0m — logs in %s\n' "$fails" "$LOGDIR"
fi
exit $((fails > 0))
