#!/usr/bin/env bash
# What the proxy actually serves, through the real router and the real image.
#
# `test/integration-ddev.sh` proves the add-on wires a worktree up: the variant
# hostname routes, the app sees the canonical Host, the parent keeps its own.
# That is one request and one header. Everything else hostshift claims — the
# request direction, redirects, cookies, Vary, validators, compression, JSON,
# binary passthrough — was asserted only in `internal/proxy` against httptest,
# or in `internal/e2e`, which needs a two-minute WordPress bootstrap and is
# skipped by default. So the surfaces where a failure is *silent* were the ones
# no routinely-run suite touched:
#
#   * the request direction. Nothing between `go test` and a live WordPress ever
#     asserted that a form POST, a REST write or a `redirect_to` query reaches
#     the application in canonical space. Get that wrong and every page still
#     loads, every link still works, and the database quietly fills with
#     worktree hostnames — the failure PLAN tests 30 and 31 exist for;
#   * the self-redirect carve-out, the *one* place a canonical origin is
#     allowed to reach the browser. Too eager and a login redirect leaves the
#     worktree; too shy and the browser loops;
#   * `Range`, which turns the rewriter off from outside — 206 skips every
#     surface, so a client could read the document whole with its canonical
#     origins intact;
#   * validators. An `ETag` that survives a rewrite means the next revalidation
#     304s and the browser serves a cached canonical-bearing body;
#   * whether what `check` reports is what is *running*.
#
# A separate script rather than more of integration-ddev.sh, for two reasons.
# It needs a fixture application — ten endpoints that say what they received —
# where that suite deliberately has a two-line one. And it starts two DDEV
# projects rather than six: a developer with a full fleet up runs out of docker
# address pools somewhere around the fifth, and a suite that cannot finish
# proves nothing. The parent checkout here is never started at all — the map is
# read off its `.ddev/config.yaml` on disk, and only the variant is under test.
#
# HOSTSHIFT_IMAGE defaults to the published image, for the same reason
# integration-ddev.sh does: that is what `ddev add-on get` gives a developer.
#
# About two and a half minutes: two `ddev start`s, one `ddev stop`/`ddev start`
# cycle, and roughly forty requests that cost about twenty seconds between them.
# The HTTP assertions are nearly free; the container lifecycle is the bill.
#
# "check catches a proxy container that is not running" was red when it was
# written: `check` read the stopped container's Config.Env, which docker keeps,
# so it printed the hostnames the proxy *would* answer on while every request to
# them was a 502. df4d064 made it look at `.State.Running` too. Keep the
# assertion — that is the shape of failure this whole file is aimed at, and the
# `ddev stop` path below cannot see it, because `ddev stop` removes the
# container rather than leaving it exited.

set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Per-run project names, as in integration-ddev.sh: a hardcoded name means a run
# colliding with a previous one's leftovers reports `ok` for assertions
# satisfied by *those* containers.
tag="pit$$"
GO="${GO:-go}"
PUBLISHED="ghcr.io/generoi/hostshift:latest"
IMAGE="${HOSTSHIFT_IMAGE:-$PUBLISHED}"
work="${HOSTSHIFT_TEST_ROOT:-$(mktemp -d)}/proxy"

fails=0
pass() { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n     %s\n' "$1" "$2"; fails=$((fails + 1)); }
skip() { printf '  skip %s\n     %s\n' "$1" "$2"; }
contains() { case "$3" in *"$2"*) pass "$1" ;; *) fail "$1" "want to contain: $2
     got: $3" ;; esac; }
lacks() { case "$3" in *"$2"*) fail "$1" "want NOT to contain: $2
     got: $3" ;; *) pass "$1" ;; esac; }
# Header names come back lowercase over HTTP/2 and capitalised over HTTP/1.1, so
# match them case-insensitively rather than pinning the protocol the router
# happens to negotiate.
hdrhas() { if printf '%s' "$3" | grep -qi "^$2"; then pass "$1"
           else fail "$1" "no header matching ^$2 in:
     $3"; fi; }
hdrnot() { if printf '%s' "$3" | grep -qi "^$2"; then fail "$1" "^$2 survived in:
     $3"
           else pass "$1"; fi; }

projects=()
cleanup() {
  # By name, not by directory: `ddev delete` from an approot that no longer
  # exists cannot find the project, and `rm -rf "$work"` destroys the handle.
  for p in ${projects[@]+"${projects[@]}"}; do
    ddev delete --omit-snapshot -y "$(basename "$p")" >/dev/null 2>&1 || true
  done
  rm -rf "$work"
}
trap cleanup EXIT

command -v ddev >/dev/null || { echo "ddev is not installed; skipping"; exit 0; }
docker info >/dev/null 2>&1 || { echo "docker is not running; skipping"; exit 0; }

mkdir -p "$work/bin"
"$GO" build -o "$work/bin/hostshift" "$repo/cmd/hostshift"
export PATH="$work/bin:$PATH"

# newsite DIR NAME — a git repo with a DDEV project and a one-line web root.
newsite() {
  mkdir -p "$1/.ddev" "$1/web"
  printf 'type: php\ndocroot: web\ndefault_container_timeout: "600"\n' \
    > "$1/.ddev/config.yaml"
  printf '<?php echo "PROJECT=%s HOST=", $_SERVER["HTTP_HOST"], "\\n";' "$2" > "$1/web/index.php"
  git -C "$1" init -q -b main
  git -C "$1" add .ddev/config.yaml web/index.php
  git -C "$1" -c user.email=t@t -c user.name=t -c commit.gpgsign=false commit -q -m init
}

# fixtures DIR CANONICAL-HOST — the application under test.
#
# Every endpoint that reports what it received answers base64. That is the whole
# trick: a plain echo is rewritten canonical->variant on the way out and reports
# the variant back whatever the request direction did, so it cannot fail. A host
# does not survive base64, so what these print is what the application saw.
#
# This used to rely on `text/plain` being outside the rewritable set instead.
# cf661ac put it inside — wp-admin/async-upload.php sends JSON under it — and
# the eight request-direction assertions below have been unpassable ever since,
# for 43 commits, whatever the proxy did. They are the only cover PLAN tests 30
# and 31 have, so the class they guard was untested the whole time and a
# permanently-red suite is what hid it. Encoding does not depend on a set that
# can grow again.
fixtures() {
  local d="$1/web" c="$2"
  cat > "$d/req.php" <<'PHP'
<?php
header('Content-Type: text/plain');
$b = file_get_contents('php://input');
$o  = "HOST=" . ($_SERVER['HTTP_HOST'] ?? '-') . "\n";
$o .= "URI=" . ($_SERVER['REQUEST_URI'] ?? '-') . "\n";
$o .= "QUERY=" . ($_SERVER['QUERY_STRING'] ?? '-') . "\n";
$o .= "REFERER=" . ($_SERVER['HTTP_REFERER'] ?? '-') . "\n";
$o .= "ORIGIN=" . ($_SERVER['HTTP_ORIGIN'] ?? '-') . "\n";
$o .= "XFPROTO=" . ($_SERVER['HTTP_X_FORWARDED_PROTO'] ?? '-') . "\n";
$o .= "XFHOST=" . ($_SERVER['HTTP_X_FORWARDED_HOST'] ?? '-') . "\n";
$o .= "XFPORT=" . ($_SERVER['HTTP_X_FORWARDED_PORT'] ?? '-') . "\n";
$o .= "CT=" . ($_SERVER['CONTENT_TYPE'] ?? '-') . "\n";
$o .= "BODY=" . $b . "\n";
foreach ($_POST as $k => $v) { $o .= "POST[$k]=" . (is_array($v) ? implode(',', $v) : $v) . "\n"; }
echo base64_encode($o);
PHP
  # The database half of PLAN tests 30 and 31 without a database: what the
  # application *persists* is read back through a surface nothing rewrites.
  cat > "$d/store.php" <<'PHP'
<?php
header('Content-Type: text/plain');
file_put_contents(sys_get_temp_dir() . '/hs_stored', $_POST['url'] ?? '');
echo "stored\n";
PHP
  cat > "$d/stored.php" <<'PHP'
<?php
header('Content-Type: text/plain');
echo base64_encode("STORED=" . @file_get_contents(sys_get_temp_dir() . '/hs_stored') . "\n");
PHP
  # HTTP_HOST is the canonical host by the time PHP sees it, so `self` builds
  # exactly the URL the browser asked for, in canonical space — which is what
  # the fleet's redirect-uploads.conf does, and what the carve-out is for.
  cat > "$d/redir.php" <<'PHP'
<?php
$h = $_SERVER['HTTP_HOST'];
switch ($_GET['m'] ?? '') {
  case 'self':  $loc = 'https://' . $h . $_SERVER['REQUEST_URI']; break;
  case 'other': $loc = 'https://' . $h . '/wp-admin/'; break;
  default:      $loc = 'https://third.example.org/x'; break;
}
header('Content-Type: text/plain');
header('Location: ' . $loc, true, 302);
echo "redirecting\n";
PHP
  # ms_cookie_constants() sets COOKIE_DOMAIN from the network domain on a
  # subdomain multisite, so the cookie arrives scoped to a host the browser is
  # not on and is discarded — login fails outright, with nothing in any log.
  cat > "$d/cookie.php" <<'PHP'
<?php
header('Content-Type: text/plain');
header('Set-Cookie: hs_session=abc; Domain=.' . $_SERVER['HTTP_HOST'] . '; Path=/; HttpOnly', false);
header('Set-Cookie: hs_third=def; Domain=.example.org; Path=/', false);
echo "cookies\n";
PHP
  # json_encode escapes the slashes, which is PLAN test 4's form exactly.
  cat > "$d/json.php" <<'PHP'
<?php
header('Content-Type: application/json');
$h = $_SERVER['HTTP_HOST'];
echo json_encode(['link' => "https://$h/a", 'html' => "<a href=\"https://$h/b\">b</a>"]);
PHP
  cat > "$d/gz.php" <<'PHP'
<?php
ini_set('zlib.output_compression', 'Off');
$body = '<html><body><a href="https://' . $_SERVER['HTTP_HOST'] . '/gz">gz</a></body></html>';
header('Content-Type: text/html; charset=utf-8');
header('Content-Encoding: gzip');
echo gzencode($body);
PHP
  cat > "$d/bin.php" <<'PHP'
<?php
header('Content-Type: image/png');
echo "\x89PNG\r\n\x1a\n";
echo "https://" . $_SERVER['HTTP_HOST'] . "/inside-a-binary\n";
PHP
  # Static, so nginx supplies ETag, Last-Modified, Content-Length and
  # Accept-Ranges — the four things a rewritten response must not keep, and the
  # only way to get a real one is to let a real server produce them.
  printf '<!doctype html>\n<a href="https://%s/asset">a</a>\n' "$c" > "$d/asset.html"
}

# installaddon DIR — what `ddev add-on get generoi/hostshift` leaves behind.
installaddon() {
  mkdir -p "$1/.ddev/commands/host"
  cp "$repo/ddev/docker-compose.hostshift.yaml" "$1/.ddev/"
  cp "$repo/ddev/config.hostshift.yaml" "$1/.ddev/"
  cp "$repo/ddev/commands/host/hostshift" "$1/.ddev/commands/host/"
  chmod +x "$1/.ddev/commands/host/hostshift"
  if [ "$IMAGE" != "$PUBLISHED" ]; then
    sed -i.bak "s|$PUBLISHED|$IMAGE|" "$1/.ddev/docker-compose.hostshift.yaml"
    rm -f "$1/.ddev/docker-compose.hostshift.yaml.bak"
  fi
}

get()    { curl -sk --max-time 20 "$@" 2>&1; }
# report — get from an endpoint that answers base64, decoded. See fixtures.
#
# Falls back to the raw body when it does not decode, so a curl error or an
# nginx error page is shown rather than an empty string — which would fail the
# assertion with nothing in it to read.
report() {
  local raw dec
  raw="$(get "$@")"
  dec="$(printf '%s' "$raw" | tr -d '\r\n' | base64 -d 2>/dev/null || true)"
  if [ -n "$dec" ]; then printf '%s' "$dec"; else printf '%s' "$raw"; fi
}
hdrs()   { curl -sk --max-time 20 -o /dev/null -D - "$@" 2>&1; }
status() { curl -sk --max-time 20 -o /dev/null -w '%{http_code}' "$@" 2>/dev/null || true; }

# ---------------------------------------------------------------------------

main="$work/${tag}"
newsite "$main" parent
git -C "$main" worktree add -q -b wt-a "$work/${tag}-wt-a"
wt="$work/${tag}-wt-a"
printf '<?php echo "PROJECT=worktree HOST=", $_SERVER["HTTP_HOST"], "\\n";' > "$wt/web/index.php"
C="${tag}.ddev.site"                 # canonical: the parent's hostname
V="wt-a--${tag}.ddev.site"           # variant:   what the browser is on
D="${tag}-wt-a.ddev.site"            # the worktree's own hostname, straight to web
fixtures "$wt" "$C"
installaddon "$wt"
projects+=("$wt")

echo "== a worktree serving at the variant hostname  (image: $IMAGE)"

(cd "$wt" && ddev hostshift init >/dev/null 2>&1) || fail "init succeeds in the worktree" ""
start_out="$(cd "$wt" && ddev start -y 2>&1)" || fail "the worktree starts" "$start_out"
contains "the post-start hook names the variant it is serving" "https://$V" "$start_out"

echo "== the request direction: what the application receives"

# One request carries the query, the Referer and the Origin, because they are
# rewritten by three different code paths and a round trip costs a second.
r="$(report -H "Referer: https://$V/some/page" -H "Origin: https://$V" \
  "https://$V/req.php?redirect_to=https%3A%2F%2F$V%2Fwp-admin%2F")"
contains "the application sees the canonical Host" "HOST=$C" "$r"
# wp_validate_redirect() checks redirect_to against home_url()'s host and
# silently discards anything else, so login returns to the wrong place.
contains "a percent-encoded redirect_to in the query arrives canonical" \
  "QUERY=redirect_to=https%3A%2F%2F$C%2Fwp-admin%2F" "$r"
# functions.php runs the referer through wp_validate_redirect($ref, false), so
# without this wp_get_referer() is false throughout wp-admin.
contains "Referer arrives canonical" "REFERER=https://$C/some/page" "$r"
contains "Origin arrives canonical" "ORIGIN=https://$C" "$r"
# SetXForwarded writes http, because hostshift listens plain; is_ssl() is then
# false behind the router and wp-login.php redirects forever.
contains "X-Forwarded-Proto says https" "XFPROTO=https" "$r"
# SetXForwarded fills X-Forwarded-Host with the *variant*, and anything that
# prefers it over Host puts the variant straight back inside WordPress —
# undoing the whole request direction.
contains "X-Forwarded-Host is not forwarded" "XFHOST=-" "$r"
contains "X-Forwarded-Port is not forwarded" "XFPORT=-" "$r"

r="$(report -d "url=https://$V/x&other=keep" "https://$V/req.php")"
contains "a form POST body arrives canonical" "POST[url]=https://$C/x" "$r"
lacks "and carries no variant origin at all" "$V" "$r"

r="$(report -H 'Content-Type: application/json' -d "{\"u\":\"https://$V/x\"}" "https://$V/req.php")"
contains "a JSON write body arrives canonical" "BODY={\"u\":\"https://$C/x\"}" "$r"

r="$(report -F "url=https://$V/x" "https://$V/req.php")"
contains "a multipart field arrives canonical" "POST[url]=https://$C/x" "$r"

# What the application *stored*, read back through a surface nothing rewrites.
# This is the assertion PLAN tests 30 and 31 are about: the browser round trip
# can look perfect while the clone fills with worktree hostnames.
get -d "url=https://$V/stored-target/" "https://$V/store.php" >/dev/null
contains "and what the application stores is canonical" \
  "STORED=https://$C/stored-target/" "$(report "https://$V/stored.php")"

echo "== the response direction"

h="$(hdrs "https://$V/redir.php?m=other")"
hdrhas "a redirect to another canonical URL comes back as the variant" \
  "location: https://$V/wp-admin/" "$h"
h="$(hdrs "https://$V/redir.php?m=third")"
hdrhas "a third-party Location is left alone" "location: https://third.example.org/x" "$h"
# The single enumerated exception to "no canonical origin reaches the browser".
# Rewriting this one sends the browser back to the request it just made.
h="$(hdrs "https://$V/redir.php?m=self")"
hdrhas "the self-redirect carve-out passes the canonical Location through" \
  "location: https://$C/redir.php?m=self" "$h"

h="$(hdrs "https://$V/cookie.php")"
contains "a Set-Cookie Domain= naming the canonical host is dropped" \
  "hs_session=abc; Path=/; HttpOnly" "$h"
lacks "and nothing is left scoped to a host the browser is not on" "Domain=.$C" "$h"
contains "a third-party cookie domain is left alone" \
  "hs_third=def; Domain=.example.org; Path=/" "$h"

r="$(get "https://$V/json.php")"
contains "a JSON response is rewritten, escaped slashes and all" \
  "https:\\/\\/$V\\/a" "$r"
# With the scheme, because the canonical host is a *suffix* of the variant one —
# a bare-hostname check here could never fail.
lacks "and carries no canonical origin" "https:\\/\\/$C" "$r"
if printf '%s' "$r" | python3 -c 'import json,sys; json.load(sys.stdin)' 2>/dev/null; then
  pass "and is still valid JSON"
else
  fail "and is still valid JSON" "$r"
fi

# The fleet has no compression config of its own, but an application that sets
# Content-Encoding itself is normal, and a proxy that cannot decode one either
# corrupts the page or ships it unrewritten.
r="$(get --compressed "https://$V/gz.php")"
contains "a gzip-encoded HTML response is decoded and rewritten" \
  "href=\"https://$V/gz\"" "$r"

# Learning that a 4 MB JPEG contains no canonical host would mean reading it
# through the automaton. The gate is Content-Type, and the cost of getting it
# wrong is a corrupted asset.
r="$(get "https://$V/bin.php")"
contains "a non-rewritable content type passes through untouched" \
  "https://$C/inside-a-binary" "$r"
lacks "and is not rewritten" "$V" "$r"

# Headers are rewritten for every response, so every response varies — including
# the 302, whose Location has just been moved into variant space. A shared cache
# keyed on path alone hands variant A's redirect to a browser on variant B.
hdrhas "a rewritten page carries Vary: Host" "vary:.*host" "$(hdrs "https://$V/asset.html")"
hdrhas "and so does a redirect" "vary:.*host" "$(hdrs "https://$V/redir.php?m=other")"

# Both halves, or the assertion passes on a server that never sent them. And a
# guard first: forwarding the upstream's length with a longer body truncates the
# response, and a truncated response satisfies every `hdrnot` below for entirely
# the wrong reason.
direct="$(hdrs "https://$D/asset.html")"
proxied="$(hdrs "https://$V/asset.html")"
contains "the rewritten file arrives intact" "https://$V/asset" "$(get "https://$V/asset.html")"
hdrhas "the same file served straight from web carries an ETag" "etag:" "$direct"
hdrnot "a rewritten response drops the upstream ETag" "etag:" "$proxied"
hdrnot "and Last-Modified" "last-modified:" "$proxied"
hdrnot "and Content-Length" "content-length:" "$proxied"

echo "== the engine cannot be turned off from outside"

# A 206 skips every rewriter, so forwarding Range let any client read the
# document whole with its canonical origins intact.
code="$(status -r 0-10 "https://$D/asset.html")"
[ "$code" = "206" ] && pass "web on its own answers a Range request with 206" \
  || fail "web on its own answers a Range request with 206" "http $code"
code="$(status -r 0-10 "https://$V/asset.html")"
[ "$code" = "200" ] && pass "the proxy answers the same request whole" \
  || fail "the proxy answers the same request whole" "http $code"
contains "and the body it returns is rewritten" "https://$V/asset" \
  "$(get -r 0-10 "https://$V/asset.html")"

# 421 is the honest answer for a Host that is not in the map: proxying it anyway
# would send an unmapped Host upstream and resolve to the wrong blog, silently.
# The router never sends one, so ask the container directly.
code="$(docker exec "ddev-${tag}-wt-a-web" curl -s -o /dev/null -w '%{http_code}' \
  -H 'Host: nope.example' "http://ddev-${tag}-wt-a-hostshift:8080/" 2>&1 || true)"
[ "$code" = "421" ] && pass "an unmapped Host is refused with 421, never proxied" \
  || fail "an unmapped Host is refused with 421, never proxied" "http $code"

echo "== two worktrees of one parent, at once"

# Two branches previewed side by side is the case the add-on exists for, and
# nothing had ever started two. Both map the same canonical origin; a mistake in
# VIRTUAL_HOST or in the narrowing means one silently serves the other's code at
# the URL its author is reviewing.
git -C "$main" worktree add -q -b wt-b "$work/${tag}-wt-b"
wt2="$work/${tag}-wt-b"
printf '<?php echo "PROJECT=worktree-b HOST=", $_SERVER["HTTP_HOST"], "\\n";' > "$wt2/web/index.php"
installaddon "$wt2"
projects+=("$wt2")
(cd "$wt2" && ddev hostshift init >/dev/null 2>&1) || fail "init succeeds in the second worktree" ""
out="$(cd "$wt2" && ddev start -y 2>&1)" || fail "the second worktree starts" "$out"

# Both proxies are up, so this is a real collision rather than a leftover file:
# point the second worktree's .ddev/.env at the first one's variant and it is
# genuinely contending for that hostname. This is the assertion the liveness gate
# exists for, and it needs two *running proxies* — which is why it lives here and
# not in the suite where the only other project is a parent with no proxy.
v1="$(sed -n 's/^HOSTSHIFT_VARIANTS=//p' "$wt/.ddev/.env" | cut -d, -f1)"
cp "$wt2/.ddev/.env" "$work/wt2-env.bak"
# Replaced, not appended: the scan reads the *first* HOSTSHIFT_VARIANTS line and
# quits, so a second one below it is never seen and the rival claims nothing.
sed -i.bak "s|^HOSTSHIFT_VARIANTS=.*|HOSTSHIFT_VARIANTS=$v1|" "$wt2/.ddev/.env"
rm -f "$wt2/.ddev/.env.bak"
if (cd "$wt" && ddev hostshift check >/dev/null 2>&1); then
  fail "check refuses when a running proxy claims the same hostname" \
    "exit 0 — a live collision was reported as healthy"
else
  pass "check refuses when a running proxy claims the same hostname"
fi
cp "$work/wt2-env.bak" "$wt2/.ddev/.env"
(cd "$wt" && ddev hostshift check >/dev/null 2>&1) \
  && pass "and passes again once the rival stops claiming it" \
  || fail "and passes again once the rival stops claiming it" "$(cd "$wt" && ddev hostshift check 2>&1)"

contains "the first worktree's variant still serves the first worktree" \
  "PROJECT=worktree HOST=$C" "$(get "https://$V/")"
contains "the second worktree's variant serves the second worktree" \
  "PROJECT=worktree-b HOST=$C" "$(get "https://wt-b--${tag}.ddev.site/")"

echo "== what is running, not what is written"

# `check` compares .ddev/.env to a recomputation of .ddev/.env, which cannot see
# the commonest failure there is — `init` without the `ddev restart` it just told
# you to run. Passing --slug keeps the file half current, so only the container
# comparison can catch this.
# Invoked directly rather than through `ddev`, so DDEV_APPROOT is unset and init
# does not restart — which is the only way to produce this state now that it
# does. That auto-restart is the real fix for "init without restart"; this
# assertion still has to prove the container comparison catches the state if it
# ever arises another way.
(cd "$wt" && ./.ddev/commands/host/hostshift init --slug wt-z >/dev/null 2>&1) || true
if (cd "$wt" && ddev hostshift check --slug wt-z >/dev/null 2>&1); then
  fail "check catches a slug the running proxy does not answer on" "exited 0"
else
  pass "check catches a slug the running proxy does not answer on"
fi
# --slug-from-branch, because a slug passed to `init` is now recorded and sticks:
# a bare `init` that silently reverted it was failing the post-start hook forever
# on every deliberately-slugged project, and telling the developer to run the
# command that undid their choice.
(cd "$wt" && ddev hostshift init --slug-from-branch >/dev/null 2>&1) || true

# The compose service is `restart: "no"`, so a proxy that dies stays dead while
# `ddev start` has already returned success — which is how an image that parses
# a flag differently gets shipped. `docker stop` reproduces that state exactly,
# and it is *not* the same state as `ddev stop`, which removes the container:
# docker keeps Config.Env on an exited one, so a `check` that reads only the env
# reports the hostnames the proxy would answer on while every request is a 502.
# That is what this caught (fixed in df4d064).
docker stop "ddev-${tag}-wt-a-hostshift" >/dev/null 2>&1 || true
code="$(status "https://$V/")"
[ "$code" = "200" ] && fail "a stopped proxy really does stop serving" "http $code" \
  || pass "a stopped proxy really does stop serving"
if (cd "$wt" && ddev hostshift check >/dev/null 2>&1); then
  fail "check catches a proxy container that is not running" "exited 0 while the container was stopped and https://$V returned $code"
else
  pass "check catches a proxy container that is not running"
fi
docker start "ddev-${tag}-wt-a-hostshift" >/dev/null 2>&1 || true

# The compose file is not versioned with the image it names, and twice now it
# has asked `:latest` for a flag the published binary did not have. An image
# built locally under the published tag — which test/bootstrap-ddev.sh does,
# deliberately — makes this suite silently test the checkout instead.
running="$(docker inspect "ddev-${tag}-wt-a-hostshift" --format '{{.Config.Image}}' 2>/dev/null || true)"
[ "$running" = "$IMAGE" ] && pass "the proxy runs the image the compose file names" \
  || fail "the proxy runs the image the compose file names" "want $IMAGE, got $running"
if [ "$IMAGE" = "$PUBLISHED" ]; then
  digests="$(docker image inspect "$IMAGE" --format '{{len .RepoDigests}}' 2>/dev/null || echo 0)"
  [ "${digests:-0}" -gt 0 ] && pass "and the published tag is one that was pulled, not built locally" \
    || fail "and the published tag is one that was pulled, not built locally" \
      "$IMAGE has no repo digest — something built over the tag, so this run tested that build"
else
  skip "and the published tag is one that was pulled, not built locally" \
    "HOSTSHIFT_IMAGE=$IMAGE is not the published tag"
fi

echo "== ddev stop, ddev start"

(cd "$wt" && ddev stop >/dev/null 2>&1) || fail "the worktree stops" ""
code="$(status "https://$V/")"
[ "$code" = "200" ] && fail "a stopped project serves nothing at the variant" "http $code" \
  || pass "a stopped project serves nothing at the variant"
if (cd "$wt" && ddev hostshift check >/dev/null 2>&1); then
  fail "check says so rather than reporting success" "exited 0"
else
  pass "check says so rather than reporting success"
fi

start_out="$(cd "$wt" && ddev start -y 2>&1)" || fail "the worktree starts again" "$start_out"
contains "the hook names the variant on a cold start too" "https://$V" "$start_out"
contains "and the variant serves the worktree again" "PROJECT=worktree HOST=$C" "$(get "https://$V/")"
contains "with the request direction still in place" "HOST=$C" "$(report "https://$V/req.php")"

echo
if [ "$fails" -gt 0 ]; then echo "$fails failure(s)"; exit 1; fi
echo "all passed"
