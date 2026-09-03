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
trap cleanup EXIT INT TERM HUP

command -v ddev >/dev/null || { echo "ddev is not installed; skipping"; exit 0; }
docker info >/dev/null 2>&1 || { echo "docker is not running; skipping"; exit 0; }

# Leftovers from an interrupted run.
#
# `trap cleanup EXIT` does not survive a process-group kill, and three
# interrupted runs in one session left seven registered projects behind — each
# holding a docker network and an address from a pool that is not large, and
# each one a project a later reader has to decide whether it is safe to delete.
#
# They are recognisable without ambiguity: this suite names every project
# `it<pid>` and creates it under a temp root, so a registered project matching
# that shape, under a temp root, whose pid is no longer running is this suite's
# and is dead. All three are required: the name alone would match a live run's
# projects, the temp root keeps a developer's own project out of scope, and the
# dead pid is what says the run is over. The approot is not the signal — an
# interrupted run never reaches the `rm -rf` either, so the directory is still
# there.
sweep_stale() {
  command -v jq >/dev/null || return 0
  ddev list -j 2>/dev/null \
    | jq -r '.raw[]? | select(.name | test("^it[0-9]+(-.*)?$")) | "\(.name)\t\(.approot)"' \
      2>/dev/null \
    | while IFS=$'\t' read -r name approot; do
        [ -n "$name" ] || continue
        # Under a temp root, so a project of the developer's that happens to be
        # named this way is never in scope.
        case "$approot" in
          /tmp/*|/var/folders/*|"${TMPDIR:-/nonexistent}"*) ;;
          *) continue ;;
        esac
        # And the run that made it is gone. The pid is in the name, and the
        # current run's own pid is alive, so this cannot reach its own projects.
        pid="${name#it}"; pid="${pid%%-*}"
        [ -n "$pid" ] || continue
        kill -0 "$pid" 2>/dev/null && continue
        echo "  (removing $name, left by an interrupted run)" >&2
        ddev delete --omit-snapshot -y "$name" >/dev/null 2>&1 || true
      done
}
sweep_stale

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
    # DDEV waits 120s for a container to become healthy and then fails the
    # start. On a machine already running the fleet, a *bare* project — no
    # add-on, no hostshift — measured 2m34s to come up, so the suite failed on
    # `ddev start` with a health-check timeout and every assertion after it,
    # while nothing in this repository was wrong. The failure text names this
    # setting; a suite that a busy machine turns red is a suite nobody can read.
    echo "default_container_timeout: \"600\""
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

# ready waits for a URL to answer at all, before the assertions that care what it
# answers.
#
# `ddev start` returns before traefik has finished reconfiguring, and a teardown
# running beside it — another suite's, or an agent's — churns the router further.
# The symptom is an empty body from one hostname while its neighbours are fine,
# which reads exactly like a routing bug and cost two investigations to identify.
#
# This is a readiness wait, not a retry-until-green: it gives up after 30 seconds
# and the assertions then run and fail as they would have, so a hostname that
# genuinely never serves is still a failure with the same message.
ready() {
  for _ in $(seq 1 15); do
    [ -n "$(curl -sk --max-time 5 -o /dev/null -w '%{http_code}' "$1" 2>/dev/null | grep -v '^000$')" ] && return 0
    sleep 2
  done
  return 0
}

echo "== a single-site worktree, zero committed config  (image: $IMAGE)"

main="$work/${tag}"
newsite "$main" parent
git -C "$main" worktree add -q -b wt-a "$work/${tag}-wt-a"
wt="$work/${tag}-wt-a"
printf '<?php echo "PROJECT=worktree HOST=", $_SERVER["HTTP_HOST"], "\\n";' > "$wt/web/index.php"
installaddon "$wt"

projects+=("$main" "$wt")
out="$(cd "$main" && ddev start -y 2>&1)" || fail "the parent starts" "$out"
out="$(cd "$wt" && ddev hostshift init 2>&1)" || fail "init succeeds in the worktree" "$out"

start_out="$(cd "$wt" && ddev start -y 2>&1)" || fail "the worktree starts" "$start_out"

# The hook ran, and did not blow up. Exit 127 here was invisible to every other
# test in the repo.
#
# The hostname, not the sentence around it. Matching a fixed phrase went red the
# day `check`'s wording changed from "is configured to serve" to "is serving" —
# a rename that broke nothing — while a hook printing the *wrong* hostname, or
# the parent's, would have passed it. What the hook is for is telling you which
# URL this checkout answers on.
contains "the post-start hook prints the URL it serves" "https://wt-a--${tag}.ddev.site" "$start_out"
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

# ...and stops passing it the moment a *running* sibling claims the same
# hostname. Two projects on one hostname is an error nowhere in DDEV — traefik
# breaks the tie by rule length and the loser is silently unreachable — so this
# refusal is what stands between a developer and reviewing the wrong branch's
# code at the right URL.
#
variant="$(sed -n 's/^HOSTSHIFT_VARIANTS=//p' "$wt/.ddev/.env" | cut -d, -f1)"

# A directory left behind by a deleted worktree is not a claim. `git worktree
# remove` refuses while untracked files are present, so the directory outliving
# the project is the common case — and refusing there failed the post-start hook
# on every start while routing was entirely correct.
dead="$work/${tag}-dead"
mkdir -p "$dead/.ddev"
printf 'HOSTSHIFT_VARIANTS=%s\n' "$variant" > "$dead/.ddev/.env"
# The parent is up but runs no proxy, so it is the same case: a .ddev/.env that
# claims a variant nothing is serving. The variants live on the *hostshift*
# container, so asking about web read every add-on-removed, crashed or
# never-restarted sibling as a live claimant, and failed the post-start hook on
# every start of a correct deployment.
cp "$main/.ddev/.env" "$work/main-env.bak" 2>/dev/null || : > "$work/main-env.bak"
printf 'HOSTSHIFT_VARIANTS=%s\n' "$variant" >> "$main/.ddev/.env"
out="$(cd "$wt" && ddev hostshift check 2>&1)" \
  && pass "a running project with no proxy of its own claims nothing" \
  || fail "a running project with no proxy of its own claims nothing" "$out"
cp "$work/main-env.bak" "$main/.ddev/.env"

out="$(cd "$wt" && ddev hostshift check 2>&1)" \
  && pass "and a stopped project's leftover directory is a warning, not a failure" \
  || fail "and a stopped project's leftover directory is a warning, not a failure" "$out"
contains "which still says what it found" "already claims" "$out"
rm -rf "$dead"

echo "== copy-db"

# The only subcommand that destroys something. It streams the parent's database
# over the compose network into this worktree's, and there is no undo — so the
# thing worth testing is not that the copy works but that it refuses when it
# would overwrite. Both halves are new and neither had any coverage.
sql() { (cd "$1" && ddev exec -s web bash -c "mysql -h db -udb -pdb -N -B -e \"$2\" db" 2>/dev/null) || true; }
sql "$main" "create table hs_probe (id int); insert into hs_probe values (42);" >/dev/null
out="$(cd "$wt" && ddev hostshift copy-db 2>&1)" || fail "copy-db copies the parent's database" "$out"
contains "copy-db copies the parent's database" "42" "$(sql "$wt" "select id from hs_probe")"

# Replace, not merge — and this is the assertion that distinguishes them. The one
# above passed before the fix too. `mysqldump db | mysql` drops only the tables
# the dump contains, so a worktree whose database came from an older pull kept
# everything the parent no longer has, while the refusal message promised a
# replace. Needs a table the *worktree* has and the parent does not; it goes in
# after the first copy, so the copy above still runs against an empty database.
sql "$wt" "create table hs_only_here (id int); insert into hs_only_here values (7);" >/dev/null
out="$(cd "$wt" && ddev hostshift copy-db --force 2>&1)" \
  || fail "and drops what only the worktree had" "$out"
if [ -n "$(sql "$wt" "select id from hs_only_here" 2>&1 | grep -x 7 || true)" ]; then
  fail "and drops what only the worktree had" "hs_only_here survived, so it merged"
else
  pass "and drops what only the worktree had"
fi

# Running it twice is the accident this refusal is for: the second run silently
# replaced whatever the first one's work had put there.
if (cd "$wt" && ddev hostshift copy-db >/dev/null 2>&1); then
  fail "copy-db refuses a database that already has tables" "exited 0"
else
  pass "copy-db refuses a database that already has tables"
fi
contains "and says how to mean it" "--force" \
  "$(cd "$wt" && ddev hostshift copy-db 2>&1 || true)"
out="$(cd "$wt" && ddev hostshift copy-db --force 2>&1)" \
  && pass "and --force goes through" || fail "and --force goes through" "$out"

# In the parent there is no other checkout to copy from, and the parent's own
# database is the one every worktree is sharing. Copying it into itself is the
# one thing that cannot be recovered from.
if (cd "$main" && ddev hostshift copy-db >/dev/null 2>&1); then
  fail "copy-db refuses to run outside a worktree" "exited 0"
else
  pass "copy-db refuses to run outside a worktree"
fi

echo "== a multisite worktree — two canonical hostnames, two variants"

m2="$work/${tag}2"
newsite "$m2" parent2 "shop.$tag"2
git -C "$m2" worktree add -q -b wt-b "$work/${tag}2-wt-b"
wt2="$work/${tag}2-wt-b"
printf '<?php echo "PROJECT=worktree2 HOST=", $_SERVER["HTTP_HOST"], "\\n";' > "$wt2/web/index.php"
installaddon "$wt2"
projects+=("$m2" "$wt2")

out="$(cd "$m2" && ddev start -y 2>&1)" || fail "the multisite parent starts" "$out"
out="$(cd "$wt2" && ddev hostshift init 2>&1)" || fail "init succeeds on a multisite" "$out"
out="$(cd "$wt2" && ddev start -y 2>&1)" || fail "the multisite worktree starts" "$out"
# Both blogs, because the second one is the one that has been seen empty while
# the first was already answering.
ready "https://wt-b--${tag}2.ddev.site/"
ready "https://wt-b--shop.${tag}2.ddev.site/"
ready "https://shop.${tag}2.ddev.site/"

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

out="$(cd "$m3" && ddev start -y 2>&1)" || fail "the hostshift.yaml parent starts" "$out"
out="$(cd "$wt3" && ddev hostshift init 2>&1)" || fail "init succeeds with a hostshift.yaml" "$out"
out3="$(cd "$wt3" && ddev start -y 2>&1)" || fail "the hostshift.yaml worktree starts" "$out3"

# The whole line, compared for equality. `contains "HOSTSHIFT_MAP_ARGS="` was
# satisfied by every possible value of that variable — it is a prefix of all of
# them — so the one assertion guarding "no flat map is handed over when a
# hostshift.yaml is mounted" could not fail, and did not notice when the
# variable was renamed out of existence. What must be true is that the proxy
# gets a slug and nothing else: a `--from/--to` pair here beats the mounted
# file, and the file's aliases then silently never rewrite.
# This map has an alias on a domain that is not a DDEV hostname, and nothing
# has generated a loopback file — so web can reach it for real. That is what
# wp-cron and Site Health do under production-canonical, with sslverify off,
# against a database that believes it is production. The shipped
# docker-compose.hostshift-loopback.yaml carries `www.example.com` as a
# placeholder, so a project that never edited it passed every guardrail here
# and reported "hostshift is serving".
out="$(cd "$wt3" && ddev hostshift check 2>&1 || true)"
contains "check notices that loopback containment is not in place" \
  "not pinned to the" "$out"
contains "and names the hostname that is not contained" "${tag}3.staging.example" "$out"

# And it stops once containment is real. Without this the check could warn
# unconditionally and the assertion above would still pass — which is the shape
# of every guardrail in this file that turned out not to guard.
(cd "$wt3" && ddev hostshift loopback > .ddev/docker-compose.hostshift-loopback.yaml) \
  || fail "loopback emits a compose file" ""
out="$(cd "$wt3" && ddev restart -y 2>&1)" || fail "restart with containment" "$out"
out="$(cd "$wt3" && ddev hostshift check 2>&1 || true)"
case "$out" in
  *"not pinned to the"*)
    fail "and goes quiet once containment is in place" "$out" ;;
  *) pass "and goes quiet once containment is in place" ;;
esac

args="$(sed -n 's/^HOSTSHIFT_ARGS=//p' "$wt3/.ddev/.env")"
[ "$args" = "--slug wt-c" ] \
  && pass "no flat map is handed over, so the container reads the mounted file" \
  || fail "no flat map is handed over, so the container reads the mounted file" \
       "want: --slug wt-c
     got:  $args"
contains "the variant serves the worktree from a mounted hostshift.yaml" "PROJECT=worktree3" \
  "$(get https://wt-c--${tag}3.ddev.site/)"
contains "and reaches the app as the canonical hostname" "HOST=${tag}3.ddev.site" \
  "$(get https://wt-c--${tag}3.ddev.site/)"

echo "== the real install path: ddev add-on get"

# Nothing anywhere ran `ddev add-on get` — installaddon() above copies the files
# by hand, and both suites say so in a comment. That is exactly why a pre-flight
# in install.yaml shipped firing on 100% of installs: post_install_actions run
# with cwd = <project>/.ddev, and nothing ever executed them.
#
# No containers here. What needs covering is install-time behaviour; that the
# installed files then work is what every scenario above already asserts, and a
# fourth project pair does not fit on a machine with a working fleet on it.
m4="$work/${tag}4"
newsite "$m4" parent4
install_out="$(cd "$m4" && ddev add-on get "$repo/ddev" 2>&1)" \
  || fail "ddev add-on get succeeds" "$install_out"
projects+=("$m4")

contains "the add-on installs the host command" "commands/host/hostshift" "$install_out"

# A .ddev project that is not a git checkout. DDEV runs install actions under
# `set -eu -o pipefail`, and `git rev-parse` exits 128 outside a repository — so
# an unguarded call aborted the install and left the project with nothing.
nogit="$work/${tag}-nogit"
mkdir -p "$nogit/.ddev"
printf 'name: %s-nogit\ntype: php\n' "$tag" > "$nogit/.ddev/config.yaml"
ng_out="$(cd "$nogit" && ddev add-on get "$repo/ddev" 2>&1)" || true
projects+=("$nogit")
if [ -f "$nogit/.ddev/commands/host/hostshift" ]; then
  pass "and installs into a project that is not a git checkout"
else
  fail "and installs into a project that is not a git checkout" "$ng_out"
fi

# The files it installs are ignored in the *checkout*, not in a commit. A
# .gitignore block is branch-scoped, so adopting hostshift leaves them untracked
# on every branch that predates the adoption — which is where `git add -A`
# commits them.
if [ -z "$(git -C "$m4" status --porcelain -- .ddev)" ]; then
  pass "and nothing it installs shows up as untracked"
else
  fail "and nothing it installs shows up as untracked" "$(git -C "$m4" status --porcelain -- .ddev)"
fi
# The project's own committed statement about its hostnames is not ignored.
printf 'sites:\n  - canonical: https://x.example\n' > "$m4/hostshift.yaml"
if git -C "$m4" check-ignore -q hostshift.yaml; then
  fail "but the repo's own hostshift.yaml still is not" "it is ignored"
else
  pass "but the repo's own hostshift.yaml still is not"
fi
rm -f "$m4/hostshift.yaml"

# Removing the add-on from one worktree must not un-ignore it in the others.
#
# info/exclude is shared by the repository and all its linked worktrees, which
# is the property that makes one write cover them all on install — and is
# exactly what made removal reach too far. `ddev add-on remove` in a worktree
# un-ignored the add-on's files and .ddev/.env in the parent and in every other
# worktree, where it is still installed and running, and said nothing. The next
# `git add -A` over there commits them, .ddev/.env included.
rmwt="$work/${tag}-rmwt"
git -C "$m4" worktree add -q -b "${tag}-rm" "$rmwt" >/dev/null 2>&1
mkdir -p "$rmwt/.ddev"
printf 'name: %s-rm\ntype: php\n' "$tag" > "$rmwt/.ddev/config.yaml"
(cd "$rmwt" && ddev add-on get "$repo/ddev" >/dev/null 2>&1) || true
projects+=("$rmwt")
# `additional_hostnames: []` is what `ddev config` writes into every project by
# default, and the removal note matched the key alone — so it told a developer
# their worktree may hijack the parent's hostnames at the moment they were
# tearing it down, on every ordinary parent, when nothing is inherited at all.
cp "$m4/.ddev/config.yaml" "$work/m4cfg.hold"
printf 'additional_hostnames: []\n' >> "$m4/.ddev/config.yaml"
rm_out="$(cd "$rmwt" && ddev add-on remove hostshift 2>&1)" || true
cp "$work/m4cfg.hold" "$m4/.ddev/config.yaml"
case "$rm_out" in
  *"may reach this worktree instead"*)
    fail "an empty additional_hostnames is not an inherited hostname" "$rm_out" ;;
  *) pass "an empty additional_hostnames is not an inherited hostname" ;;
esac
if git -C "$m4" check-ignore -q .ddev/.env; then
  pass "removing it from one worktree leaves the others ignored"
else
  fail "removing it from one worktree leaves the others ignored" "$rm_out"
fi
git -C "$m4" worktree remove --force "$rmwt" >/dev/null 2>&1 || true
git -C "$m4" branch -D "${tag}-rm" >/dev/null 2>&1 || true

case "$install_out" in
  *"predates this add-on"*)
    fail "a fresh install is not told its command is stale" "$install_out" ;;
  *) pass "a fresh install is not told its command is stale" ;;
esac
if head -5 "$m4/.ddev/commands/host/hostshift" | grep -q '#ddev-generated'; then
  pass "the installed command carries the marker that lets it be upgraded"
else
  fail "the installed command carries the marker that lets it be upgraded" "no marker"
fi
# Re-installing over a marked command must be silent about staleness, and must
# actually replace it.
printf '# edited\n' >> "$m4/.ddev/commands/host/hostshift"
again="$(cd "$m4" && ddev add-on get "$repo/ddev" 2>&1)" || true
case "$again" in
  *"predates this add-on"*) fail "re-installing over a marked command is silent" "$again" ;;
  *) pass "re-installing over a marked command is silent" ;;
esac
if grep -q '^# edited' "$m4/.ddev/commands/host/hostshift"; then
  fail "and replaces it" "the edit survived, so an upgrade would not land"
else
  pass "and replaces it"
fi

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
