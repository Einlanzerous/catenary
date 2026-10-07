#!/usr/bin/env bash
# deploy/probe-client.sh <base-url> — is the web client served by this image?
#
# CANT-241, the check that would have caught CANT-231: seventeen releases
# answered `GET /` with a 404 because no image ever contained the client, and
# nothing before or after a merge ever asked for `/`. ONE SCRIPT, TWO CALLERS:
# ci.yml's `image` job runs it against an image built from the pull request,
# and publish.yml's smoke job against the digest it just pushed — so the
# pre-merge check and the post-publish check cannot drift into two lists.
#
# It asserts, from outside the container, with curl:
#   - GET / is 200, text/html, the app's mount point, `no-cache` with an ETag,
#     nosniff, and the interim Content-Security-Policy (ruling 2 → B);
#   - GET / with If-None-Match set to that ETag is 304;
#   - the /assets/*.js and /assets/*.css the document names are each 200,
#     non-empty, text/javascript and text/css, and immutable;
#   - GET /favicon.svg is 200 and image/svg+xml;
#   - GET /assets/ is 404, GET /enroll is 405, GET /sync with no credential is
#     401 with a JSON body — the API is still the API;
#   - GET /nope is 404 (ruling 1 → A: there is no fallback).
#
# It stops at the FIRST failed assertion and exits non-zero, so the line it
# prints last is the reason.
set -euo pipefail

base="${1:?usage: probe-client.sh <base-url>}"
base="${base%/}"

# The interim policy. CANT-242 replaces it with the full one, in the same
# change that replaces internal/api's documentCSP.
want_csp="frame-ancestors 'none'"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "ok    $*"; }

# fetch <name> <path> [curl args…] — body to $work/<name>.body, headers to
# $work/<name>.head, and prints the status code.
fetch() {
  local name="$1" path="$2"; shift 2
  # --max-time: a container that accepts and never answers must fail this
  # probe, not hold the job until GitHub kills it.
  curl -sS --max-time 10 -o "$work/$name.body" -D "$work/$name.head" -w '%{http_code}' "$@" "$base$path"
}

# header <name> <Header-Name> — the header's value, CR stripped, or empty.
header() {
  awk -v h="$(printf '%s' "$2" | tr 'A-Z' 'a-z')" -F': ' '
    { k = tolower($1) } k == h { sub(/\r$/, ""); sub(/^[^:]*: /, ""); print; exit }
  ' "$work/$1.head"
}

# --- the document --------------------------------------------------------------

code="$(fetch doc /)"
[ "$code" = 200 ] || fail "GET / = $code, want 200 — this image serves no web client"
ct="$(header doc Content-Type)"
case "$ct" in text/html*) ;; *) fail "GET / Content-Type = '$ct', want text/html" ;; esac
grep -q '<div id="app">' "$work/doc.body" || fail "GET / does not contain <div id=\"app\">"
[ "$(header doc Cache-Control)" = "no-cache" ] || fail "GET / Cache-Control = '$(header doc Cache-Control)', want no-cache"
etag="$(header doc ETag)"
[ -n "$etag" ] || fail "GET / carries no ETag"
[ "$(header doc X-Content-Type-Options)" = "nosniff" ] || fail "GET / X-Content-Type-Options = '$(header doc X-Content-Type-Options)', want nosniff"
[ "$(header doc Content-Security-Policy)" = "$want_csp" ] || fail "GET / Content-Security-Policy = '$(header doc Content-Security-Policy)', want \"$want_csp\""
pass "GET / is the document: 200 $ct, no-cache, ETag $etag, nosniff, CSP \"$want_csp\""

code="$(fetch reval / -H "If-None-Match: $etag")"
[ "$code" = 304 ] || fail "GET / with If-None-Match: $etag = $code, want 304"
pass "GET / with its ETag is 304"

# --- the assets the document names ---------------------------------------------

js="$(grep -oE '/assets/[A-Za-z0-9._-]+\.js' "$work/doc.body" | head -1 || true)"
css="$(grep -oE '/assets/[A-Za-z0-9._-]+\.css' "$work/doc.body" | head -1 || true)"
[ -n "$js" ] || fail "the document names no /assets/*.js"
[ -n "$css" ] || fail "the document names no /assets/*.css"

asset() {
  local name="$1" path="$2" want="$3"
  local code ct
  code="$(fetch "$name" "$path")"
  [ "$code" = 200 ] || fail "GET $path = $code, want 200"
  [ -s "$work/$name.body" ] || fail "GET $path is empty"
  ct="$(header "$name" Content-Type)"
  case "$ct" in "$want"*) ;; *) fail "GET $path Content-Type = '$ct', want $want" ;; esac
  case "$(header "$name" Cache-Control)" in
    *immutable*) ;;
    *) fail "GET $path Cache-Control = '$(header "$name" Cache-Control)', want immutable" ;;
  esac
  pass "GET $path is 200 $ct, $(wc -c <"$work/$name.body" | tr -d ' ') bytes, immutable"
}
asset js "$js" text/javascript
asset css "$css" text/css

code="$(fetch icon /favicon.svg)"
[ "$code" = 200 ] || fail "GET /favicon.svg = $code, want 200"
[ "$(header icon Content-Type)" = "image/svg+xml" ] || fail "GET /favicon.svg Content-Type = '$(header icon Content-Type)', want image/svg+xml"
pass "GET /favicon.svg is 200 image/svg+xml"

# --- the API is still the API ----------------------------------------------------

code="$(fetch dir /assets/)"
[ "$code" = 404 ] || fail "GET /assets/ = $code, want 404 — there is no directory listing"
pass "GET /assets/ is 404"

code="$(fetch enroll /enroll)"
[ "$code" = 405 ] || fail "GET /enroll = $code, want 405"
pass "GET /enroll is 405"

code="$(fetch sync /sync)"
[ "$code" = 401 ] || fail "GET /sync with no credential = $code, want 401"
case "$(header sync Content-Type)" in application/json*) ;; *) fail "GET /sync 401 Content-Type = '$(header sync Content-Type)', want JSON" ;; esac
pass "GET /sync with no credential is 401 JSON: $(cat "$work/sync.body")"

# Ruling 1 → A: no fallback to the document.
code="$(fetch nope /nope)"
[ "$code" = 404 ] || fail "GET /nope = $code, want 404 — an unknown path is not the document"
pass "GET /nope is 404"

echo "the web client is served: $base"
