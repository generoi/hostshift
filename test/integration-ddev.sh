#!/usr/bin/env bash
# End-to-end through real DDEV: parent checkout, git worktree, add-on, router.
#
# This exists because of what it would have caught. Three defects shipped in one
# day and every one passed `test/addon-command.sh` green:
#
#   * the post-start hook exited 127 on every `ddev start`, because it sourced
#     .ddev/.env and one value contains a space — nothing ran the hook body;
#   * a comma-separated `--map` that the *published* image parses as one pair,
#     so a two-site worktree 421'd both variants — nothing ran the published
#     image;
#   * `ddev hostshift check` certifying all of it as correct.
#
# The unit-ish suite tests what the host command *writes*. This tests what DDEV
# then *serves*, which is the only claim a user cares about.
#
# The upstream is a two-line PHP file rather than WordPress: this is about
# routing, hostname mapping and the add-on, and internal/e2e already covers a
# real CMS. That keeps it near two minutes.
#
# HOSTSHIFT_IMAGE defaults to the published image on purpose — that is what a
# developer running `ddev add-on get` actually gets, and image skew is a defect
# class this suite exists to catch. CI runs it twice, once with the image built
# from the checkout.

set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Per-run project names. Hardcoded ones meant a run colliding with a previous
# one's leftovers reported `ok` for assertions satisfied by *those* containers,
# while neither project under test had started at all.
tag="it$$"
GO="${GO:-go}"
IMAGE="${HOSTSHIFT_IMAGE:-ghcr.io/generoi/hostshift:latest}"
work="${HOSTSHIFT_TEST_ROOT:-$(mktemp -d)}/integration"

fails=0
pass() { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n     %s\n' "$1" "$2"; fails=$((fails + 1)); }
contains() { case "$3" in *"$2"*) pass "$1" ;; *) fail "$1" "want to contain: $2
     got: $3" ;; esac; }

projects=()
cleanup() {
  # By name, not by directory. `ddev delete` run from an approot that no longer
  # exists cannot find the project, and `rm -rf "$work"` below destroys the only
  # handle — leaving a registered project nothing can remove.
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

# newsite DIR NAME [EXTRA-HOSTNAME] — a git repo with a DDEV project and a web
# root that says which project served it and on which hostname.
newsite() {
  mkdir -p "$1/.ddev" "$1/web"
  {
    echo "type: php"
    echo "docroot: web"
    [ -n "${3:-}" ] && printf 'additional_hostnames:\n  - %s\n' "$3"
  } > "$1/.ddev/config.yaml"
  printf '<?php echo "PROJECT=%s HOST=", $_SERVER["HTTP_HOST"], "\\n";' "$2" > "$1/web/index.php"
  git -C "$1" init -q -b main
  git -C "$1" add .ddev/config.yaml web/index.php
  git -C "$1" -c user.email=t@t -c user.name=t -c commit.gpgsign=false commit -q -m init
}

# installaddon DIR — what `ddev add-on get generoi/hostshift` leaves behind.
installaddon() {
  mkdir -p "$1/.ddev/commands/host"
  cp "$repo/ddev/docker-compose.hostshift.yaml" "$1/.ddev/"
  cp "$repo/ddev/config.hostshift.yaml" "$1/.ddev/"
  cp "$repo/ddev/commands/host/hostshift" "$1/.ddev/commands/host/"
  chmod +x "$1/.ddev/commands/host/hostshift"
  # The compose file names the image; honour HOSTSHIFT_IMAGE.
  if [ "$IMAGE" != "ghcr.io/generoi/hostshift:latest" ]; then
    sed -i.bak "s|ghcr.io/generoi/hostshift:latest|$IMAGE|" "$1/.ddev/docker-compose.hostshift.yaml"
    rm -f "$1/.ddev/docker-compose.hostshift.yaml.bak"
  fi
}

get() { curl -sk --max-time 20 "$1" 2>&1; }

echo "== a single-site worktree, zero committed config  (image: $IMAGE)"

main="$work/${tag}"
newsite "$main" parent
git -C "$main" worktree add -q -b wt-a "$work/${tag}-wt-a"
wt="$work/${tag}-wt-a"
printf '<?php echo "PROJECT=worktree HOST=", $_SERVER["HTTP_HOST"], "\\n";' > "$wt/web/index.php"
installaddon "$wt"

projects+=("$main" "$wt")
(cd "$main" && ddev start -y >/dev/null 2>&1) || fail "the parent starts" ""
(cd "$wt" && ddev hostshift init >/dev/null 2>&1) || fail "init succeeds in the worktree" ""

start_out="$(cd "$wt" && ddev start -y 2>&1)" || fail "the worktree starts" "$start_out"

# The hook ran, and did not blow up. Exit 127 here was invisible to every other
# test in the repo.
contains "the post-start hook prints the URLs it serves" "hostshift is configured to serve" "$start_out"
case "$start_out" in
  *"Task failed"*|*"exit status 127"*|*"No such file or directory"*)
    fail "the post-start hook does not fail the start" "$start_out" ;;
  *) pass "the post-start hook does not fail the start" ;;
esac

contains "the variant serves the worktree" "PROJECT=worktree" \
  "$(get https://wt-a--${tag}.ddev.site/)"
contains "and it reaches the app as the canonical hostname" "HOST=${tag}.ddev.site" \
  "$(get https://wt-a--${tag}.ddev.site/)"
contains "the canonical hostname still serves the parent" "PROJECT=parent" \
  "$(get https://${tag}.ddev.site/)"
contains "the worktree's own hostname still serves the worktree" "PROJECT=worktree" \
  "$(get https://${tag}-wt-a.ddev.site/)"

# Mailpit rides web's VIRTUAL_HOST, which the add-on narrows.
code="$(curl -sk -o /dev/null -w '%{http_code}' --max-time 20 https://${tag}-wt-a.ddev.site:8026/ 2>/dev/null || true)"
[ "$code" = "200" ] && pass "mailpit still routes" || fail "mailpit still routes" "http $code"

out="$(cd "$wt" && ddev hostshift check 2>&1)" && pass "check passes a live worktree" \
  || fail "check passes a live worktree" "$out"

echo "== a multisite worktree — two canonical hostnames, two variants"

m2="$work/${tag}2"
newsite "$m2" parent2 "shop.$tag"2
git -C "$m2" worktree add -q -b wt-b "$work/${tag}2-wt-b"
wt2="$work/${tag}2-wt-b"
printf '<?php echo "PROJECT=worktree2 HOST=", $_SERVER["HTTP_HOST"], "\\n";' > "$wt2/web/index.php"
installaddon "$wt2"
projects+=("$m2" "$wt2")

(cd "$m2" && ddev start -y >/dev/null 2>&1) || fail "the multisite parent starts" ""
(cd "$wt2" && ddev hostshift init >/dev/null 2>&1) || fail "init succeeds on a multisite" ""
(cd "$wt2" && ddev start -y >/dev/null 2>&1) || fail "the multisite worktree starts" ""

# The case a comma-separated --map broke against the published image: both
# variants 421'd while everything reported success.
contains "the first variant serves the worktree" "PROJECT=worktree2" \
  "$(get https://wt-b--${tag}2.ddev.site/)"
contains "and reaches the app as its canonical hostname" "HOST=${tag}2.ddev.site" \
  "$(get https://wt-b--${tag}2.ddev.site/)"
contains "the second variant serves the worktree" "PROJECT=worktree2" \
  "$(get https://wt-b--shop.${tag}2.ddev.site/)"
contains "and reaches the app as *its* canonical hostname" "HOST=shop.${tag}2.ddev.site" \
  "$(get https://wt-b--shop.${tag}2.ddev.site/)"

# The whole reason the compose file narrows web's VIRTUAL_HOST. A worktree
# inherits additional_hostnames verbatim and traefik prefers the longer rule, so
# without the narrowing the worktree serves the parent's blog — silently, to
# whoever else is working on it. Nothing asserted this.
contains "and the parent keeps its own blog hostname" "PROJECT=parent2" \
  "$(get https://shop.${tag}2.ddev.site/)"

echo "== a worktree with a committed hostshift.yaml — the container resolves its own map"

# The one per-project shape neither this suite nor CI exercised. Both scenarios
# above have no hostshift.yaml, so the *host* resolves the map and hands it over
# as --from/--to and the container never opens .ddev/config.yaml. With the file
# present the map args are empty by design and the container reads it itself —
# which is the multisite worked example, and acmecorp' exact configuration.
m3="$work/${tag}3"
newsite "$m3" parent3
git -C "$m3" worktree add -q -b wt-c "$work/${tag}3-wt-c"
wt3="$work/${tag}3-wt-c"
printf '<?php echo "PROJECT=worktree3 HOST=", $_SERVER["HTTP_HOST"], "\\n";' > "$wt3/web/index.php"
cat > "$wt3/hostshift.yaml" <<YAML
sites:
  - name: main
    canonical: https://${tag}3.ddev.site
    aliases:
      - https://${tag}3.staging.example
YAML
installaddon "$wt3"
projects+=("$m3" "$wt3")

(cd "$m3" && ddev start -y >/dev/null 2>&1) || fail "the hostshift.yaml parent starts" ""
(cd "$wt3" && ddev hostshift init >/dev/null 2>&1) || fail "init succeeds with a hostshift.yaml" ""
out3="$(cd "$wt3" && ddev start -y 2>&1)" || fail "the hostshift.yaml worktree starts" "$out3"

contains "the map args are empty, so the container reads the mounted file" \
  "HOSTSHIFT_MAP_ARGS=" "$(cat "$wt3/.ddev/.env")"
contains "the variant serves the worktree from a mounted hostshift.yaml" "PROJECT=worktree3" \
  "$(get https://wt-c--${tag}3.ddev.site/)"
contains "and reaches the app as the canonical hostname" "HOST=${tag}3.ddev.site" \
  "$(get https://wt-c--${tag}3.ddev.site/)"

echo "== drift is noticed rather than served"

sed -i.bak 's/^HOSTSHIFT_WEB_HOSTS=.*/HOSTSHIFT_WEB_HOSTS=renamed.ddev.site/' "$wt/.ddev/.env"
rm -f "$wt/.ddev/.env.bak"
if (cd "$wt" && ddev hostshift check >/dev/null 2>&1); then
  fail "check catches a stale .ddev/.env" "exited 0"
else
  pass "check catches a stale .ddev/.env"
fi
(cd "$wt" && ddev hostshift init >/dev/null 2>&1) || true

echo
if [ "$fails" -gt 0 ]; then echo "$fails failure(s)"; exit 1; fi
echo "all passed"
