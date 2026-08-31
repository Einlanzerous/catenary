#!/usr/bin/env bash
# R2 / IDEA-25 — the phased delivery test.
#
# Conditions in increasing order of how much they hurt. The ticket is explicit
# that every word of "a cold, Doze-sleeping device receives a notification
# within seconds, consistently, over a multi-hour test" is load-bearing, so each
# one is set up deliberately and VERIFIED rather than assumed.
#
# Two distinctions that decide whether this measures anything:
#
#   * `am kill` is NOT `am force-stop`. A user-initiated force-stop suppresses
#     FCM until the app is next opened — that is correct Android behaviour, not
#     a delivery failure, and testing it would produce a false negative. `am
#     kill` evicts the process the way memory pressure would, which is what
#     "cold" actually means here.
#
#   * Doze only engages on an UNPLUGGED device. A phone on the charger never
#     enters it, so a test run over USB measures nothing. This script refuses to
#     run the doze phase if the device is powered.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PKG=dev.catenary.pushprove
# DEVICE selects which adb target; URLF/TOKENF select which collector. Each
# device gets its own collector because the harness tracks one token at a time,
# and pointing a second phone at a running soak would silently hijack it.
DEVICE="${DEVICE:-}"
URLF="${URLF:-$HERE/secrets/.collector-url}"
TOKENF="${TOKENF:-$HERE/secrets/.collector-token}"
U="$(cat "$URLF")"
T="$(cat "$TOKENF")"
adb() { command adb ${DEVICE:+-s "$DEVICE"} "$@"; }
N="${N:-3}"          # probes per phase
GAP="${GAP:-45}"     # seconds between probes within a phase

api() { curl -s -m 30 -H "Authorization: Bearer $T" "$U$1" ; }
send() { api "/send?phase=$1" -o /dev/null -w '' >/dev/null; }

banner() { printf '\n\033[1m══ %s ══\033[0m\n' "$1"; }

probe_burst() { # probe_burst <phase>
  for i in $(seq 1 "$N"); do
    send "$1"
    sleep "$GAP"
  done
}

banner "phase: background (app not foreground, screen off)"
adb shell input keyevent KEYCODE_HOME >/dev/null
sleep 2
adb shell input keyevent KEYCODE_SLEEP >/dev/null
sleep 3
echo "screen: $(adb shell dumpsys power | grep -oE 'mWakefulness=[A-Za-z]+' | head -1)"
probe_burst background

banner "phase: cold (process evicted, NOT force-stopped)"
adb shell am kill "$PKG" >/dev/null 2>&1
sleep 3
PID="$(adb shell pidof $PKG 2>/dev/null | tr -d '\r')"
if [ -n "$PID" ]; then
  echo "WARNING: process still alive (pid $PID) — 'cold' is not cold"
else
  echo "process evicted: no pid for $PKG"
fi
probe_burst cold

banner "phase: doze (forced deep idle, verified)"
POWERED=$(adb shell dumpsys battery | grep -cE '(AC|USB|Wireless) powered: true')
if [ "$POWERED" -ne 0 ]; then
  echo "REFUSING: device is on power. Doze does not engage while charging —"
  echo "unplug it and re-run. Running this phase plugged in would produce a"
  echo "green result that means nothing."
else
  adb shell dumpsys deviceidle enable deep >/dev/null 2>&1
  # force-idle sometimes needs a couple of nudges to walk the state machine
  for _ in 1 2 3; do
    adb shell dumpsys deviceidle force-idle >/dev/null 2>&1
    STATE="$(adb shell dumpsys deviceidle get deep 2>/dev/null | tr -d '\r')"
    [ "$STATE" = "IDLE" ] && break
    sleep 2
  done
  echo "deep idle state: ${STATE:-unknown}"
  if [ "${STATE:-}" != "IDLE" ]; then
    echo "WARNING: not actually in deep Doze — treat this phase as invalid"
  fi
  probe_burst doze
  echo "restoring normal power management"
  adb shell dumpsys deviceidle unforce >/dev/null 2>&1
  adb shell dumpsys battery reset >/dev/null 2>&1
fi

banner "results so far"
api /report
