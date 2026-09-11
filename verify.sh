#!/usr/bin/env bash
# Run everything that can be checked without hardware or a network peer.
#
# One command, because a spike's evidence is worthless if reproducing it needs
# a tour. What this does NOT cover at all is the things that need the world:
# R1's tunnel run (spike/r1-websocket), and R2/R5, which need an Android device
# and a willing friend. There are no steps for those.
#
# FOUR STEPS HERE SKIP rather than fail when what they need is absent, and a
# skip is not a pass:
#
#   Dart            both the analyze and the conformance runner, when `dart` is
#                   not on PATH or at $DART.
#   the database    the store's schema tests, when CATENARY_TEST_DATABASE_URL is
#                   unset. `go test` still runs; those tests call t.Skip.
#   the served page CANT-26's client-rule check, on the same condition — its
#                   input is a page captured from a real Postgres or it is a
#                   fixture, and a fixture would prove nothing.
#   R6              when the Purser checkout the spike's `replace` points at is
#                   absent, which is every machine but one.
#
# They skip rather than fail because CI runs this file whole, and a step that
# can never pass there is a red light everyone learns to ignore. CI forces the
# first two present — `setup-dart` and the Postgres service — so only R6
# actually skips there.
#
# THE FIRST STEP RESOLVES DEPENDENCIES, and only when they are missing. Without
# it this script was green on its SECOND run in a fresh worktree and red on its
# first, for reasons having nothing to do with the change under test — which is
# the same "red light everyone learns to ignore" the paragraph above is about,
# arriving from the other direction. See CANT-94.
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
# CHECKED IN THE FUNCTION, not at the call site, and this file runs without
# `set -e` so it has to be checked somewhere. An unwritable TMPDIR or a full
# /tmp leaves the caller with an empty string, every redirect below fails, and
# the two emptiness gates then test a file that was never created — the
# identical false PASS this whole change exists to remove, reintroduced by its
# own fix. CANT-87 checked one call site; CANT-88 is the second one it missed,
# inside the guard step, where an empty directory made BOTH assertions pass.
#
# SETS A VARIABLE rather than printing, and that is the whole trick. The
# obvious version — `mktemp -d … || { …; exit 1; }` in the body, callers
# writing `x="$(new_logdir)"` — does not work: a command substitution runs the
# function in a SUBSHELL, so the exit kills the subshell, the caller is handed
# "" and the run carries on. Verified rather than assumed. Assigning to
# LOGDIR_OUT and calling `new_logdir` as a plain statement keeps the exit in
# this shell, and closes every call site including ones added later.
new_logdir() {
  LOGDIR_OUT="$(mktemp -d "${TMPDIR:-/tmp}/catenary-verify.XXXXXX")" && [ -n "$LOGDIR_OUT" ] || {
    printf 'verify.sh: cannot create a log directory under %s\n' "${TMPDIR:-/tmp}" >&2
    exit 1
  }
}
new_logdir; LOGDIR="$LOGDIR_OUT"

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

# DEPENDENCIES FIRST, BECAUSE ONE COMMAND HAS TO MEAN ONE COMMAND.
#
# CANT-94. This file's own header promises a spike's evidence is reproducible
# without a tour, and it was not: `ci.yml` runs `npm ci` and `dart pub get`
# BEFORE calling this script, so CI and a local run were never making the same
# claim. A cold worktree failed SIX steps — vue-tsc, the TypeScript conformance
# runner, dart analyze, the web smoke test, the captured-response validator and
# the read-state check — none of them about the change under test. The dart one
# was the only one usually seen, because anyone who had run this before had
# already typed `npm ci` out of habit.
#
# GUARDED, so a warm run costs nothing. `npm ci` is not idempotent in the way
# that matters here: it DELETES node_modules before installing, so running it
# unconditionally would add a minute to every verification on a tree that was
# already fine.
#
# It does not abort on failure. This file deliberately runs without `set -e` and
# reports every step; a failed install shows up here AND as the steps that
# depend on it, which is more informative than stopping at the first one.
step "dependencies — resolved here so CI and a local run make the same claim"
if [ -d "$ROOT/web/node_modules" ]; then
  printf '   \033[33mSKIP\033[0m web/node_modules present — nothing to install\n'
else
  (cd "$ROOT/web" && npm ci) >"$LOGDIR/v-npm-ci.log" 2>&1
  result $? "npm ci — web/node_modules was absent"
fi
# Inside the same `command -v dart` guard the Dart step uses, so the skip lane
# still skips rather than failing on a machine with no SDK.
if command -v dart >/dev/null; then
  if [ -f "$ROOT/dart/.dart_tool/package_config.json" ]; then
    printf '   \033[33mSKIP\033[0m dart/.dart_tool present — nothing to resolve\n'
  else
    (cd "$ROOT/dart" && dart pub get) >"$LOGDIR/v-dart-pub-get.log" 2>&1
    result $? "dart pub get — dart/.dart_tool was absent"
  fi
else
  printf '   \033[33mSKIP\033[0m dart not on PATH — nothing to resolve\n'
fi

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
  # `-p 1` IS LOAD BEARING, and only in this branch. There is ONE test
  # database, and every package that touches it starts by rolling every
  # migration back and reapplying — which is the right way to get a known
  # state and the wrong thing to do to another package's rows. Go runs
  # different packages' tests in PARALLEL by default, so the moment a second
  # package grew a database test (cmd/catenary, CANT-26) the two started
  # resetting the schema out from under each other: `relation "log_counter"
  # does not exist` on one side and a duplicate log_seq on the other, both
  # intermittent, neither about the code.
  #
  # The no-database branch below does not need it: those tests share nothing.
  (cd "$ROOT" && go test -p 1 ./...) >"$LOGDIR/v-go-svc.log" 2>&1
  result $? "go test ./... (with a database — includes CANT-19's log_seq commit-ordering property)"
else
  (cd "$ROOT" && go test ./...) >"$LOGDIR/v-go-svc.log" 2>&1
  result $? "go test ./... ($(grep -c 'no test files\|^ok' "$LOGDIR/v-go-svc.log") packages)"
  printf '   \033[33mNOTE\033[0m CATENARY_TEST_DATABASE_URL unset — the schema and log_seq ordering tests skipped.\n'
  printf '        docker run -d --name cant-pg -e POSTGRES_HOST_AUTH_METHOD=trust -e POSTGRES_DB=catenary_test -p 55440:5432 postgres:16-alpine\n'
  printf '        export CATENARY_TEST_DATABASE_URL=postgres://postgres@127.0.0.1:55440/catenary_test?sslmode=disable\n'
fi

# CANT-26 · the web client's OWN unread rules, over a page this server built.
#
# Two steps rather than one because they are two different claims. The Go test
# writes a real /sync response — seven members, a mark at seq 1, a run of five
# above it, three from other people — straight out of Postgres through the real
# serveSync, nothing tidied. The node step then decodes it with the GENERATED
# decoder and calls newCount and unreadCount from web/src/store.ts, so the
# numbers are checked by the client's own code and not by a second copy of the
# rule that would agree with the first by construction.
#
# Only with a database, because the input is real data or it is a fixture.
if [ -n "${CATENARY_TEST_DATABASE_URL:-}" ]; then
  step "CANT-26 · the client's unread rules over a real served page"
  (cd "$ROOT" && CATENARY_SYNC_CAPTURE="$LOGDIR/served-sync.json" \
    go test ./cmd/catenary/ -run TestTheServedPageCarriesTheCanvasNumbers -count=1) \
    >"$LOGDIR/v-capture.log" 2>&1
  result $? "a real /sync page is served from Postgres"
  (cd "$ROOT/web" && npm run --silent readstate -- "$LOGDIR/served-sync.json") >"$LOGDIR/v-readstate.log" 2>&1
  result $? "$(grep -cE '^ok  ' "$LOGDIR/v-readstate.log") of the client's own assertions pass against it"
else
  # Announced rather than absent. Every other skipping lane says so, and a step
  # that simply does not appear is a run that proved less than it looks —
  # which is the thing this file's own header exists to prevent.
  step "CANT-26 · the client's unread rules over a real served page"
  printf '   \033[33mSKIP\033[0m CATENARY_TEST_DATABASE_URL unset — the input is a real page or it is a fixture.\n'
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
# CANT-88, and it is proved two ways because one alone would have missed it.
#
# DYNAMICALLY: a broken allocator must STOP the run, not hand back an empty
# string. In a subshell, so this file survives proving it.
( TMPDIR=/nonexistent-catenary-probe-dir; new_logdir; printf 'RETURNED\n' ) >/dev/null 2>&1
[ $? -ne 0 ]; result $? "a broken allocator stops the run rather than returning an empty directory"

# STATICALLY: the dynamic proof above only holds while nobody wraps the call in
# a command substitution, which would run it in a subshell and turn its exit
# back into an empty string at the caller. That is the exact trap the function's
# comment describes, and it is one keystroke away at every future call site.
# BOTH substitution forms. Matching only `$(` left backticks through, which is
# the same one-keystroke gap as the `v`-anchored pattern this file already
# widened once below — and the same shape as the bug this guard exists to catch.
# `$( new_logdir )` with spaces was the third.
#
# A plain `( … )` subshell is deliberately NOT matched: the dynamic probe above
# uses one on purpose, and it is safe because it does not capture the output.
# What swallows the exit AND hands back "" is the substitution, not the subshell.
scan_logdir_substitution() { grep -nE '^[^#]*(\$\(|`)[[:space:]]*new_logdir' "$1"; }
bad_sub=$(scan_logdir_substitution "$ROOT/verify.sh")
[ -z "$bad_sub" ]; result $? "new_logdir is never called in a command substitution$( [ -n "$bad_sub" ] && printf ' — %s' "$(echo "$bad_sub" | tr '\n' ' ')" )"
{
  printf 'x="%snew_logdir)"\n' '$('
  printf 'y=%snew_logdir%s\n' '`' '`'
  printf 'z="%s new_logdir )"\n' '$('
} > "$LOGDIR/probe-sub.sh"
probe_sub=$(scan_logdir_substitution "$LOGDIR/probe-sub.sh")
rm -f "$LOGDIR/probe-sub.sh"
[ "$(printf '%s\n' "$probe_sub" | grep -c .)" -eq 3 ]; result $? "planted substitutions all make it fail — \$( ), backticks, and spaced"

new_logdir; other="$LOGDIR_OUT"
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
# green guard. No /tmp literal in this file is a false positive, and there are
# two reasons rather than one: most are `${TMPDIR:-/tmp}`-prefixed, which is
# `/tmp}` and not `/tmp/` — and the probe below passes bare `/tmp` as printf
# ARGUMENTS, which is why its own source line does not match the scan it feeds.
#
# Both halves have to be stated. A previous version claimed every literal was
# TMPDIR-prefixed, which is not true of the probe, and a reader auditing the
# file against that sentence would "fix" the probe to `${TMPDIR:-/tmp}` — at
# which point it plants `>/home/…/gen.log`, the scan matches nothing, and the
# assertion that the scan bites goes red. Said without a count on purpose: the
# count was wrong within one commit of being written.
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
