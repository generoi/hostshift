#!/usr/bin/env bash
# Tests for ddev/commands/host/hostshift, the add-on's half of the split.
#
# The binary's half has Go tests; this half is shell, and it is where the
# opinions live — slug derivation, which checkout the map comes from, which
# hostnames web keeps, what gets written where. When `hostshift init` moved out
# of the binary its tests did not come with it, and the audits found six
# regressions in the untested code: the map built against the wrong checkout,
# --dry-run writing files, wp-cli emitting invalid YAML two ways, FQDNs written
# under the wrong key, unconditional hostname subtraction, and a slug pipeline
# that aborts on a non-ASCII branch name.
#
# No Docker and no DDEV: the command only needs `hostshift` on PATH, git, and a
# .ddev/ directory. Run with `make test-addon` or directly.

set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cmd="$repo/ddev/commands/host/hostshift"
GO="${GO:-go}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Stamped, so the image-version comparison is exercisable. An unstamped build
# reports "dev", which check deliberately says nothing about — a developer
# running their own binary against a published image built it themselves.
"$GO" build -ldflags "-X main.version=vtest" -o "$work/hostshift" "$repo/cmd/hostshift"
export PATH="$work:$PATH"

fails=0
pass() { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n     %s\n' "$1" "$2"; fails=$((fails + 1)); }

# check NAME EXPECTED ACTUAL
check() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "want: $2
     got:  $3"; fi
}
contains() {
  case "$3" in *"$2"*) pass "$1" ;; *) fail "$1" "want to contain: $2
     got: $3" ;; esac
}

# newproject DIR [CONFIG] — a git repo with a .ddev/config.yaml.
newproject() {
  mkdir -p "$1/.ddev"
  printf '%b\n' "${2:-additional_hostnames:\n  - blog}" > "$1/.ddev/config.yaml"
  git -C "$1" init -q -b main
  # Tracked, because that is what makes a worktree inherit it — and what makes
  # a worktree that wants containers of its own override `name` elsewhere.
  git -C "$1" add .ddev/config.yaml
  # gpgsign off: these are throwaway fixtures, and a developer whose global
  # config signs commits should not have the suite fail on a locked agent.
  git -C "$1" -c user.email=t@t -c user.name=t -c commit.gpgsign=false commit -q -m init
}

echo "== slug derivation"

d="$work/slug"; newproject "$d"
(cd "$d" && git checkout -q -b feature/ABC-123)
out="$(cd "$d" && "$cmd" env 2>/dev/null || true)"
contains "branch becomes a hostname label" "feature-abc-123--slug.ddev.site" "$out"

# BSD sed under a UTF-8 locale aborted with "RE error: illegal byte sequence",
# and en_US.UTF-8 is the macOS default.
(cd "$d" && git checkout -q -b 'feature/käyttöliittymä')
if out="$(cd "$d" && LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8 "$cmd" env 2>&1)"; then
  # A hostname, not the variable name — "HOSTSHIFT_VARIANTS=" is a prefix of
  # every possible value, so it could not fail.
  # Equality on the line: "--slug.ddev.site" was a *suffix* of every possible
  # variant here, because the fixture project is called "slug".
  check "a non-ASCII branch still derives a slug" \
    "HOSTSHIFT_VARIANTS=feature-k-ytt-liittym--slug.ddev.site,feature-k-ytt-liittym--blog.ddev.site" \
    "$(printf '%s\n' "$out" | sed -n 's/^\(HOSTSHIFT_VARIANTS=.*\)$/\1/p')"
else
  fail "a non-ASCII branch still derives a slug" "exited non-zero: $out"
fi

# The truncation used to happen after the trim, so a slug cut at 30 characters
# could end in a hyphen and derive "…abc---site.ddev.site". Asserted on the
# variant hostname: the earlier version read HOSTSHIFT_SLUG, which no longer
# exists, so it compared an always-empty string and could not fail.
(cd "$d" && git checkout -q -b abcdefghijklmnopqrstuvwxyzabc/x)
out="$(cd "$d" && "$cmd" env 2>/dev/null | sed -n 's/^HOSTSHIFT_VARIANTS=//p' || true)"
case "$out" in
  ""|*"---"*) fail "a truncated slug never ends in a hyphen" "variants=${out:-none}" ;;
  *) pass "a truncated slug never ends in a hyphen" ;;
esac

(cd "$d" && git checkout -q -b '___')
if (cd "$d" && "$cmd" env >/dev/null 2>&1); then
  fail "a branch with no usable characters is refused" "exited 0"
else
  contains "a branch with no usable characters is refused" "pass --slug" \
    "$(cd "$d" && "$cmd" env 2>&1 || true)"
fi

echo "== the map comes from the checkout whose database it is"

# docroot, because the database gate has to run wp-cli from it. `ddev exec -s
# web` lands in /var/www/html; WordPress lives under the docroot, and stock DDEV
# WordPress uses `web`. Leaving it out of the fixture is what let the gate be a
# silent no-op while this suite passed.
main="$work/acme"; newproject "$main" 'name: acme\ndocroot: web\nadditional_hostnames:\n  - nat.acme'
wt="$work/acme-wt-a"
git -C "$main" worktree add -q -b feature/x "$wt"
mkdir -p "$wt/.ddev"
printf 'name: acme-wt-a\n' > "$wt/.ddev/config.worktree.local.yaml"

out="$(cd "$wt" && "$cmd" env --slug wt-a 2>/dev/null || true)"
contains "a worktree maps the parent's hostnames, not its own" \
  "HOSTSHIFT_VARIANTS=wt-a--acme.ddev.site,wt-a--nat.acme.ddev.site" "$out"
contains "web keeps the worktree's own hostname" \
  "HOSTSHIFT_WEB_HOSTS=acme-wt-a.ddev.site" "$out"

# A worktree of a repo that pins `name:` is the *same* DDEV project as its
# parent. Subtracting the parent's hostnames unconditionally emptied
# HOSTSHIFT_WEB_HOSTS, and web then answered only on its primary hostname.
same="$work/pinned-wt"
pinned="$work/pinned"; newproject "$pinned" 'name: pinned\nadditional_hostnames:\n  - nat.pinned'
git -C "$pinned" worktree add -q -b feature/y "$same"
out="$(cd "$same" && "$cmd" env --slug wt-b 2>/dev/null || true)"
contains "a same-project worktree keeps its hostnames on web" \
  "HOSTSHIFT_WEB_HOSTS=pinned.ddev.site,nat.pinned.ddev.site" "$out"

# Falling back silently is the failure this path exists to prevent: the map gets
# built against the hostname the worktree is served at, and nothing is rewritten.
mv "$main/.ddev/config.yaml" "$main/.ddev/hidden.yaml"
out="$(cd "$wt" && "$cmd" env --slug wt-a 2>&1 || true)"
contains "an unreadable parent is a warning, not a silent wrong map" \
  "no DDEV" "$out"
mv "$main/.ddev/hidden.yaml" "$main/.ddev/config.yaml"

# ...but a same-project worktree is not that case, and must not be warned about.
out="$(cd "$same" && "$cmd" env --slug wt-b 2>&1 || true)"
case "$out" in
  *"no DDEV"*) fail "a same-project worktree is not warned about" "$out" ;;
  *) pass "a same-project worktree is not warned about" ;;
esac

# hostshift.yaml is a deliberate statement about which hostnames the database
# holds; --from would override it.
printf 'sites:\n  - canonical: https://acme.fi\n    base: https://acme.ddev.site\n' > "$wt/hostshift.yaml"
out="$(cd "$wt" && "$cmd" env --slug wt-a 2>/dev/null || true)"
# By name, not by line number — the block gains and loses variables.
# Equality: the expected string was a *prefix* of the broken output too, so
# dropping the guard that makes hostshift.yaml win still passed.
check "hostshift.yaml wins over the parent's hostnames" \
  "HOSTSHIFT_VARIANTS=wt-a--acme.ddev.site" \
  "$(printf '%s\n' "$out" | sed -n '/^HOSTSHIFT_VARIANTS=/p')"
# ...and then the container reads that file itself, since it is mounted and
# carries aliases a canonical=variant list cannot express.
contains "a mounted hostshift.yaml is resolved with a slug, not a map" \
  "HOSTSHIFT_ARGS=--slug wt-a" "$out"
rm "$wt/hostshift.yaml"

# DDEV derives `name` from the directory but not additional_hostnames, so a
# worktree registers the parent's extra hostnames as its own — and traefik,
# which has no cross-project uniqueness check, resolves the tie by rule length,
# which a worktree's longer directory name always wins. `ddev start` says
# nothing about it.
# `init --dry-run`, because the note is init's: it says those hostnames serve
# this worktree "until `ddev restart`", which is true when init says it and
# false ever after — so env, wp-cli and loopback printing it told a developer
# with a healthy worktree that it was stealing a hostname it was not.
out="$(cd "$wt" && "$cmd" init --dry-run --slug wt-a 2>&1 || true)"
contains "an inherited hostname the parent also serves is called out" \
  "also registered nat.acme.ddev.site" "$out"
# ...and not by the read-only subcommands, where it is false.
out="$(cd "$wt" && "$cmd" env --slug wt-a 2>&1 || true)"
case "$out" in
  *"also registered"*) fail "and env does not repeat it" "$out" ;;
  *) pass "and env does not repeat it" ;;
esac

# The map the proxy cannot work out for itself: the compose service mounts only
# this worktree, so the parent's hostnames are unknowable inside the container —
# `-C /project` there built its map from the directory basename, literally
# "project", and every request 421'd.
contains "the resolved map is handed to the container" \
  "HOSTSHIFT_ARGS=--from https://acme.ddev.site --to https://wt-a--acme.ddev.site" "$out"

# The parent moved or was deleted. `git rev-parse --git-common-dir` then *fails*
# rather than naming an unreadable path, so testing its output alone read as
# "not a worktree" — the map silently took the worktree's own hostname as
# canonical, and the narrowing subtracted nothing, so the worktree kept the
# parent's blog hostnames permanently rather than until a restart.
mv "$main" "$main-moved"
out="$(cd "$wt" && "$cmd" env --slug wt-a 2>&1 || true)"
contains "a parent that moved away is a warning, not a silent wrong map" \
  "no DDEV" "$out"
mv "$main-moved" "$main"

# Two branches whose slugs collide give two projects identical VIRTUAL_HOST
# entries; DDEV validates hostname syntax per project and no more, and traefik
# resolves the tie by rule length rather than complaining.
twin="$work/acme-wt-twin"
git -C "$main" worktree add -q -b twin "$twin"
mkdir -p "$twin/.ddev"; printf 'name: acme-wt-twin\n' > "$twin/.ddev/config.worktree.local.yaml"
(cd "$wt" && "$cmd" init --slug clash >/dev/null 2>&1) || fail "init exited non-zero" ""
out="$(cd "$twin" && "$cmd" env --slug clash 2>&1 || true)"
contains "a slug another project already claims is a warning" "already claims" "$out"
out="$(cd "$twin" && "$cmd" env --slug distinct 2>&1 || true)"
case "$out" in *"already claims"*) fail "a distinct slug is not warned about" "$out" ;;
                *) pass "a distinct slug is not warned about" ;; esac
git -C "$main" worktree remove --force "$twin"
# and put $wt back as the dry-run test below expects to find it
rm -f "$wt/.ddev/.env"

# Installed but not configured is inert — except in a worktree of a project that
# declares additional_hostnames, where `ddev add-on get`'s own advice to run
# `ddev restart` registers the parent's blog hostnames on this project's web and
# the router prefers this one. The moment the add-on could say so is the moment
# it created the hazard, and it said nothing.
mv "$wt/.ddev/.env" "$work/env.hold" 2>/dev/null || true
out="$(cd "$wt" && HOSTSHIFT_HOOK=1 "$cmd" check 2>&1 || true)"
contains "an unconfigured worktree of a multi-hostname parent is warned" \
  "not configured" "$out"
contains "and names the hostnames at stake" "nat.acme.ddev.site" "$out"

# ...and when the CLI is not on PATH, it says that rather than going quiet.
#
# This branch short-circuits above the `command -v hostshift` guard, so a missing
# binary left the question unanswered with no warning and no reason — in exactly
# the situation the warning exists for.
out="$(cd "$wt" && HOSTSHIFT_HOOK=1 PATH="/usr/bin:/bin" "$cmd" check 2>&1 || true)"
contains "a missing CLI is reported, not swallowed" "cannot be checked without" "$out"
# ...and a project whose parent declares nothing extra stays silent.
cp "$main/.ddev/config.yaml" "$work/pcfg.hold"
printf 'name: acme\n' > "$main/.ddev/config.yaml"
out="$(cd "$wt" && HOSTSHIFT_HOOK=1 "$cmd" check 2>&1 || true)"
case "$out" in
  "") pass "and one whose parent declares nothing extra stays silent" ;;
  *) fail "and one whose parent declares nothing extra stays silent" "$out" ;;
esac
# ...including the empty list `ddev config` writes into every project.
#
# `grep '^additional_hostnames:'` matches `additional_hostnames: []`, so this
# fired on every unconfigured worktree of every ordinary parent — telling a
# developer their worktree was hijacking hostnames the two projects did not
# share. install.yaml's removal path carries 25 lines of awk written for exactly
# this case; the check path had a bare grep and was only ever tested against a
# config with no key at all.
printf 'name: acme\nadditional_hostnames: []\n' > "$main/.ddev/config.yaml"
out="$(cd "$wt" && HOSTSHIFT_HOOK=1 "$cmd" check 2>&1 || true)"
case "$out" in
  "") pass "an empty additional_hostnames list is not a declaration" ;;
  *) fail "an empty additional_hostnames list is not a declaration" "$out" ;;
esac
printf 'name: acme\nadditional_hostnames: [] # none\n' > "$main/.ddev/config.yaml"
out="$(cd "$wt" && HOSTSHIFT_HOOK=1 "$cmd" check 2>&1 || true)"
case "$out" in
  "") pass "and neither is one with a trailing comment" ;;
  *) fail "and neither is one with a trailing comment" "$out" ;;
esac
cp "$work/pcfg.hold" "$main/.ddev/config.yaml"
mv "$work/env.hold" "$wt/.ddev/.env" 2>/dev/null || true

# A parent that has adopted a committed hostshift.yaml, and a worktree whose
# branch predates it. `hostshift hosts -C <dir>` reads only DDEV config, so the
# declaration is invisible from here — and the map then names the parent's
# ddev.site hostnames, which is the wrong side: the shared database holds the
# canonical ones, nothing is rewritten, and every link points at the live site.
printf 'version: 1\nsites:\n  - {name: main, canonical: https://www.acme.example, base: https://acme.ddev.site}\n' \
  > "$main/hostshift.yaml"
out="$(cd "$wt" && "$cmd" env --slug wt-a 2>&1 || true)"
contains "a parent's hostshift.yaml a worktree cannot see is called out" \
  "declares the hostnames its database" "$out"

# And the case that made the warning a permanent, unconditional refusal.
#
# The condition was pure file existence: the parent has a hostshift.yaml this
# branch does not. But the most ordinary reason to adopt one is an alias, with
# `canonical` naming the hostname the map already uses — and then the worktree's
# map names exactly what the parent declares, nothing is wrong, and `check`
# still said "would rewrite nothing, every link would point at the canonical
# site" and exited 2. Measured against a live deployment: 200s, 32 origins
# rewritten, `hostshift diff` GREEN, refused on every `ddev start` with nothing
# that could ever clear it.
#
# Comparing two declarations is not the discovery §4.1 forbids — the database
# gate was wrong four rounds running because it interrogated the application.
# This asks two config files what they say, which is what builds the map anyway.
printf 'version: 1\nsites:\n  - {name: main, canonical: https://acme.ddev.site, aliases: [https://nat.acme.ddev.site]}\n' \
  > "$main/hostshift.yaml"
out="$(cd "$wt" && "$cmd" env --slug wt-a 2>&1 || true)"
case "$out" in
  *"declares the hostnames its database"*)
    fail "a parent declaring what the map already names is not called out" "$out" ;;
  *) pass "a parent declaring what the map already names is not called out" ;;
esac
# An alias, which is one of exactly two things the README says a hostshift.yaml
# is for — and the shape the comparison was written to clear, which it did not.
#
# --canonical-hosts is documented "aliases included", so the parent's set always
# carried a hostname the worktree's map does not name, the sets were never
# equal, and the refusal stood on every start of a worktree of any repo that had
# adopted aliases. Measured against a live deployment: every page 200, zero
# canonical origins remaining, `ddev start` reporting Task failed.
printf 'version: 1\nsites:\n  - {name: main, canonical: https://acme.ddev.site, aliases: [https://acme.staging.example.net]}\n' \
  > "$main/hostshift.yaml"
out="$(cd "$wt" && "$cmd" env --slug wt-a 2>&1 || true)"
case "$out" in
  *"declares the hostnames its database"*)
    fail "a parent declaring an alias is not called out" "$out" ;;
  *) pass "a parent declaring an alias is not called out" ;;
esac

# But a differing *port* is a differing origin, and must still be called out.
#
# --canonical-hosts emits hostnames, so it dropped the port and the two sides
# compared equal — the warning was *cleared* while the engine, which matches on
# exact origin equality, correctly served content holding `:8443` straight
# through. Wrong in both directions from the one flag.
printf 'version: 1\nsites:\n  - {name: main, canonical: https://acme.ddev.site:8443, base: https://acme.ddev.site:8443}\n' \
  > "$main/hostshift.yaml"
out="$(cd "$wt" && "$cmd" env --slug wt-a 2>&1 || true)"
contains "a parent declaring a different port is still called out" \
  "declares the hostnames its database" "$out"

# A parent declaring a production canonical *and* a DDEV hostname.
#
# Set overlap cleared on the DDEV one and said nothing, while the production
# origins were served to the browser unrewritten — `check` exit 0, and
# `hostshift diff` GREEN too, because that origin is not in its map either. This
# gate was the only thing that was ever going to catch it. What matters is
# whether the map covers everything the parent declares, so the test is subset.
printf 'version: 1\nsites:\n  - {name: prod, canonical: https://www.acme.example}\n  - {name: local, canonical: https://acme.ddev.site}\n' \
  > "$main/hostshift.yaml"
out="$(cd "$wt" && "$cmd" env --slug wt-a 2>&1 || true)"
contains "a declared canonical the map does not cover is called out" \
  "declares the hostnames its database" "$out"

# And the case the warning exists for: a production canonical, where the map
# really was built from the wrong side and would rewrite nothing.
printf 'version: 1\nsites:\n  - {name: main, canonical: https://www.acme.example, base: https://acme.ddev.site}\n' \
  > "$main/hostshift.yaml"
out="$(cd "$wt" && "$cmd" env --slug wt-a 2>&1 || true)"
contains "a production canonical the map does not name is still called out" \
  "declares the hostnames its database" "$out"
rm -f "$main/hostshift.yaml"

# The remedy must not be the action that breaks the site.
#
# A developer whose database holds DDEV hostnames — what `copy-db` and a
# search-replace leave behind — followed "copy it here", and every page became a
# wp-signup.php redirect: `hostshift diff` went from GREEN to 8 errors RED.
printf 'version: 1\nsites:\n  - {name: main, canonical: https://www.acme.example, base: https://acme.ddev.site}\n' \
  > "$main/hostshift.yaml"
out="$(cd "$wt" && "$cmd" env --slug wt-a 2>&1 || true)"
contains "the remedy says what to do when the database has not moved" \
  "already right and there is nothing to do" "$out"
case "$out" in
  *"or copy it here"*)
    fail "the remedy no longer offers the action that breaks the site" "$out" ;;
  *) pass "the remedy no longer offers the action that breaks the site" ;;
esac
rm -f "$main/hostshift.yaml"

# A declaration the comparison cannot read is not agreement. Silence here would
# reintroduce the leak the warning exists for, so a parent whose hostshift.yaml
# does not parse must still be called out.
#
# (The comparison also refuses to treat two *empty* answers as a match. Nothing
# below drives that: it needs the worktree's own map to fail at the same moment,
# and the script has already exited by then if it did. It is guesswork-free
# insurance, not a tested path, and this comment is the honest label.)
printf 'version: 1\nsites: [[[ not yaml\n' > "$main/hostshift.yaml"
out="$(cd "$wt" && "$cmd" env --slug wt-a 2>&1 || true)"
contains "a parent declaration that will not parse is still called out" \
  "declares the hostnames its database" "$out"
rm -f "$main/hostshift.yaml"

# A colliding project that is not a sibling directory.
#
# The scan read `../*/.ddev/.env` and nothing else, so two worktrees of one
# parent in different directories — `git worktree add ~/worktrees/foo`, or two
# developers with worktrees under their own homes — collided in silence: both
# .ddev/.env files claiming the same variants, both projects up, and check in
# each one printing "hostshift is serving" and exiting 0. A *dead* fake sibling
# warned immediately, which is the wrong way round.
far="$work/elsewhere/faraway"
mkdir -p "$far/.ddev"
printf 'HOSTSHIFT_VARIANTS=wt-a--acme.ddev.site\n' > "$far/.ddev/.env"
printf 'name: faraway\n' > "$far/.ddev/config.yaml"
reg="$work/ddevglobal"
mkdir -p "$reg"
printf 'faraway:\n    approot: %s\n' "$far" > "$reg/project_list.yaml"
out="$(cd "$wt" && DDEV_GLOBAL_CONFIG="$reg" "$cmd" check --slug wt-a 2>&1 || true)"
contains "a colliding project that is not a sibling is found" "already claims" "$out"

# An approot with a space in it. `for x in $list` splits on it, so the check
# that exists for "two developers with worktrees under their own home
# directories" was blind to `~/My Projects/…`.
spaced="$work/My Projects/faraway"
mkdir -p "$spaced/.ddev"
printf 'HOSTSHIFT_VARIANTS=wt-a--acme.ddev.site\n' > "$spaced/.ddev/.env"
printf 'name: spaced\n' > "$spaced/.ddev/config.yaml"
printf 'spaced:\n    approot: %s\n' "$spaced" > "$reg/project_list.yaml"
out="$(cd "$wt" && DDEV_GLOBAL_CONFIG="$reg" "$cmd" check --slug wt-a 2>&1 || true)"
contains "an approot containing a space is not skipped" "already claims" "$out"
rm -rf "$work/My Projects"

# A DDEV install that has never registered a project. The read used to abort the
# whole script under `set -e`, with no diagnostic and exit 1 rather than 2.
rm -f "$reg/project_list.yaml"
# In an `if` condition, because this suite runs under `set -e` and the whole
# point is a command that may exit non-zero.
if out="$(cd "$wt" && DDEV_GLOBAL_CONFIG="$reg" "$cmd" check --slug wt-a 2>&1)"; then
  rc=0
else
  rc=$?
fi
if [ "$rc" = 1 ] && [ -z "$out" ]; then
  fail "a missing project_list.yaml is survivable" "exit 1, silent"
else
  pass "a missing project_list.yaml is survivable"
fi
rm -rf "$work/elsewhere" "$reg"

# The checkout a worktree branches from has no canonical/variant distinction to
# make, and running init there makes it claim its own worktrees' hostnames.
out="$(cd "$main" && "$cmd" env --slug wt-a 2>&1 || true)"
contains "init in the parent checkout is warned about" "not a linked worktree" "$out"

echo "== init"

out="$(cd "$wt" && "$cmd" init --dry-run --slug wt-a 2>/dev/null || true)"
if [ -e "$wt/.ddev/.env" ]; then
  fail "--dry-run writes nothing" "files appeared"
else
  pass "--dry-run writes nothing"
fi
contains "--dry-run shows what it would write" "HOSTSHIFT_VARIANTS=wt-a--acme.ddev.site" "$out"

(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || fail "init exited non-zero" ""
# config.worktree.local.yaml is this harness's own, giving the worktree a DDEV
# name of its own. init must add nothing beside it.
if ls "$wt"/.ddev/*hostshift*.yaml >/dev/null 2>&1; then
  fail "init writes no DDEV config file" "$(ls "$wt"/.ddev/*hostshift*.yaml)"
else
  pass "init writes no DDEV config file"
fi

first="$(cat "$wt/.ddev/.env")"
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || fail "init exited non-zero" ""
check "init is idempotent" "$first" "$(cat "$wt/.ddev/.env")"

# The hook line must parse under every released version of the command.
#
# config.hostshift.yaml carries #ddev-generated, so `ddev add-on get` replaces it
# on upgrade. The command only gained that marker after v0.1.0, so DDEV refuses
# to replace *that* — an upgraded project runs the new hook against the old
# command. A flag the old parser does not know makes it exit 2, and since the
# proxy goes on serving, the post-start check is then dead and silent: every
# later drift goes unreported, on exactly the projects running longest.
#
# Run against the real released command out of the tag, not a stub of it, and
# against every tag there is — a stub only proves what its author remembered.
# The hook is a YAML block scalar now, so take the whole indented body rather
# than one line after the key.
hook="$(awk '
  /^ *- *exec-host: *\|/ { inblock = 1; next }
  inblock && /^        / { sub(/^        /, ""); print; next }
  inblock { exit }
' "$repo/ddev/config.hostshift.yaml")"
[ -n "$hook" ] || hook="$(sed -n 's/^ *- *exec-host: *//p' "$repo/ddev/config.hostshift.yaml")"
[ -n "$hook" ] || fail "the post-start hook line could not be read" ""
for tag in $(cd "$repo" && git tag -l 'v*'); do
  old_cmd="$work/hostshift-$tag"
  (cd "$repo" && git show "$tag:ddev/commands/host/hostshift") > "$old_cmd" 2>/dev/null || continue
  chmod +x "$old_cmd"
  # The hook names its path; point that at the old command and run the line the
  # way DDEV does, `bash -c` from the approot (pkg/ddevapp/task.go, ExecHostTask).
  mkdir -p "$wt/.ddev/commands/host"
  cp "$old_cmd" "$wt/.ddev/commands/host/hostshift"
  # `|| true`: the suite runs under `set -e`, and the whole point is to invoke a
  # command that may fail. What is asserted is *why* it failed, not that it did.
  out="$(cd "$wt" && bash -c "$hook" 2>&1 || true)"
  rm -f "$wt/.ddev/commands/host/hostshift"
  case "$out" in
    *"unknown argument"*|*"usage: ddev hostshift <command>"*)
      fail "the post-start hook parses under $tag's command" "$out" ;;
    *) pass "the post-start hook parses under $tag's command" ;;
  esac
  # ...and it warns, on every start, that the old command is still in place —
  # one line in an `add-on get` output is not enough, and DDEV itself tells
  # developers to strip the marker that keeps the command replaceable.
  case "$out" in
    *"predates this add-on"*) pass "and says the command was not replaced" ;;
    *) fail "and says the command was not replaced" "$out" ;;
  esac
done

# Mid-rebase, a parent's config.yaml has conflict markers and is not valid YAML.
# The outcome is right — fall back to the worktree's own hostnames and warn — but
# "no DDEV config could be read there" reads as "the parent has no DDEV project",
# which sends the reader to look in the wrong place.
cp "$main/.ddev/config.yaml" "$work/parent-config.bak"
printf '<<<<<<< HEAD\nname: acme\n=======\nname: acme2\n>>>>>>> other\n' > "$main/.ddev/config.yaml"
out="$(cd "$wt" && "$cmd" env --slug wt-a 2>&1 || true)"
contains "an unparseable parent config names the conflict markers" \
  "conflict markers" "$out"
cp "$work/parent-config.bak" "$main/.ddev/config.yaml"

# A slug the developer chose must survive `check` and a bare `init`.
#
# check re-derived the slug from the branch whenever there was one, so
# `init --slug review-42` on branch `main` deployed correctly, served correctly,
# and then failed the post-start hook on every start — comparing the deployed
# `--slug review-42` against a freshly derived `--slug main`. The advice it
# printed, `ddev hostshift init`, silently reverted the choice when run as
# written, which in the case that motivates --slug at all lands back on the
# colliding name the collision warning told you to move off.
rm -f "$wt/.ddev/.env"
(cd "$wt" && "$cmd" init --slug picked >/dev/null 2>&1) || fail "init exited non-zero" ""
contains "an explicit slug is recorded" "HOSTSHIFT_SLUG_CHOSEN=picked" "$(cat "$wt/.ddev/.env")"
out="$(cd "$wt" && "$cmd" env 2>&1 || true)"
contains "and a later run with no --slug keeps it" \
  "HOSTSHIFT_VARIANTS=picked--acme.ddev.site" "$out"
# No Docker here, so check gets as far as the missing container and stops. What
# matters is that it does not get there via "out of date" — which is what it
# said before, on a deployment that was correct.
out="$(cd "$wt" && "$cmd" check 2>&1 || true)"
case "$out" in
  *"out of date"*) fail "and check does not call it stale" "$out" ;;
  *) pass "and check does not call it stale" ;;
esac

# ...and there is a way back, or the only route to branch-derived naming is
# hand-editing .ddev/.env.
(cd "$wt" && "$cmd" init --slug-from-branch >/dev/null 2>&1) || fail "init exited non-zero" ""
case "$(cat "$wt/.ddev/.env")" in
  *HOSTSHIFT_SLUG=*) fail "--slug-from-branch forgets the recorded slug" "$(cat "$wt/.ddev/.env")" ;;
  *) pass "--slug-from-branch forgets the recorded slug" ;;
esac

# ...while a slug that came from the branch still tracks the branch.
out="$(cd "$wt" && "$cmd" env 2>&1 || true)"
case "$out" in
  *"picked--acme"*) fail "a branch-derived slug still follows the branch" "$out" ;;
  *) pass "a branch-derived slug still follows the branch" ;;
esac
rm -f "$wt/.ddev/.env"
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || fail "init exited non-zero" ""

out="$(cd "$wt" && "$cmd" env --slug other 2>/dev/null || true)"
contains "a second slug does not compound with the first" \
  "HOSTSHIFT_VARIANTS=other--acme.ddev.site,other--nat.acme.ddev.site" "$out"

printf 'UNRELATED=keep\n' > "$wt/.ddev/.env"
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || fail "init exited non-zero" ""
contains ".env keeps keys that are not ours" "UNRELATED=keep" "$(cat "$wt/.ddev/.env")"
# Not "stays 0644" — that pinned a bug. .ddev/.env is where DDEV documents
# putting project env vars, credentials included, so init must keep whatever
# mode the file already had rather than widening it.
chmod 600 "$wt/.ddev/.env"
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || fail "init exited non-zero" ""
check ".env keeps the mode it had" "-rw-------" \
  "$(ls -l "$wt/.ddev/.env" | cut -c1-10)"

# The write goes through a temp file in .ddev/ so the mv is an atomic rename.
# That temp file is only safe because of the EXIT trap: without it, every failed
# init leaves a `.ddev/.env.XXXXXX` behind — a file DDEV's own .ddev/.gitignore
# does not cover, so it shows up as an untracked file in the developer's repo,
# holding whatever credentials .env held. chmod 000 makes the `cp -p` fail,
# which is the failure path.
chmod 000 "$wt/.ddev/.env"
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || true
chmod 600 "$wt/.ddev/.env"
leftover="$(ls "$wt"/.ddev/.env.?????? 2>/dev/null || true)"
if [ -n "$leftover" ]; then
  fail "a failed init leaves no temp file behind" "$leftover"
else
  pass "a failed init leaves no temp file behind"
fi
rm -f "$wt/.ddev/.env"
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || fail "init exited non-zero" ""
check ".env is 0644 when init creates it" "-rw-r--r--" \
  "$(ls -l "$wt/.ddev/.env" | cut -c1-10)"

# A detached HEAD — a rebase, a bisect, a checked-out tag — is ordinary, and the
# post-start hook runs `check` on every start. This path has shipped broken
# twice and had no coverage in any suite.
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || fail "init exited non-zero" ""
was="$(git -C "$wt" symbolic-ref --short HEAD)"
(cd "$wt" && git checkout -q --detach HEAD)
out="$(cd "$wt" && "$cmd" check 2>&1 || true)"
case "$out" in
  *"no git branch here"*|*"has no letters or digits"*|*"is out of date"*)
    fail "a detached HEAD does not break check" "$out" ;;
  *) pass "a detached HEAD does not break check" ;;
esac
git -C "$wt" checkout -q "$was"

echo "== check"

# `hostshift check` on its own is worthless in a worktree: run without --from it
# resolves layer 1 against the worktree's own config, calls the map injective and
# anchored, and exits 0 — for a map the proxy never sees. It is also what the
# README and the 421 body tell you to run.
# check now asks the container what it is answering on, so a current file with
# nothing running is exactly the commonest failure — `init` without the
# `ddev restart` it just told you to run. Serving is asserted in
# test/integration-ddev.sh, where there is a container to ask.
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || fail "init exited non-zero" ""
out="$(cd "$wt" && "$cmd" check --slug wt-a 2>&1 || true)"
contains "check says so when there is no proxy container" "no ddev-acme-wt-a-hostshift container" "$out"
if (cd "$wt" && "$cmd" check --slug wt-a >/dev/null 2>&1); then
  fail "and exits non-zero" "exited 0"
else
  pass "and exits non-zero"
fi

# The file is checked before the container, so a stale file is reported as a
# stale file rather than as "not running".
sed -i.bak 's/^HOSTSHIFT_VARIANTS=.*/HOSTSHIFT_VARIANTS=stale--acme.ddev.site/' "$wt/.ddev/.env"
rm -f "$wt/.ddev/.env.bak"
if (cd "$wt" && "$cmd" check --slug wt-a >/dev/null 2>&1); then
  fail "check catches a .ddev/.env written for another slug" "exited 0"
else
  pass "check catches a .ddev/.env written for another slug"
fi
contains "and says what to run" "ddev hostshift init" \
  "$(cd "$wt" && "$cmd" check --slug wt-a 2>&1 || true)"

rm -f "$wt/.ddev/.env"
if (cd "$wt" && "$cmd" check --slug wt-a >/dev/null 2>&1); then
  fail "check refuses when nothing is deployed" "exited 0"
else
  pass "check refuses when nothing is deployed"
fi

# The published image splits a comma-separated --map on its first `=`, so a
# multisite map came out as one site with a corrupt variant host and every
# variant 421'd. --from/--to is index-aligned and has always been understood.
out="$(cd "$wt" && "$cmd" env --slug wt-a 2>/dev/null || true)"
contains "a multi-site map is passed as repeated --from/--to" \
  "HOSTSHIFT_ARGS=--from https://acme.ddev.site --to https://wt-a--acme.ddev.site --from https://nat.acme.ddev.site --to https://wt-a--nat.acme.ddev.site" \
  "$out"

# The hook runs this; sourcing .ddev/.env made a project file into host code,
# and broke outright on any value containing a space.
hookbody="$(sed -n '/^hooks:/,$p' "$repo/ddev/config.hostshift.yaml")"
case "$hookbody" in
  *". ./.ddev/.env"*|*"source "*|*". .ddev/.env"*)
    fail "the hook does not source .ddev/.env" "$hookbody" ;;
  *) pass "the hook does not source .ddev/.env" ;;
esac

# A plain checkout with a hostshift.yaml is the other headline use — serving a
# site at a hostname its database does not hold. Warning there is a false alarm.
prod="$work/prodsite"; newproject "$prod" 'name: prodsite\ntype: php'
printf 'sites:\n  - canonical: https://www.example.com\n    base: https://prodsite.ddev.site\n' > "$prod/hostshift.yaml"
out="$(cd "$prod" && "$cmd" env --slug preview 2>&1 || true)"
case "$out" in *"not a linked worktree"*) fail "a declared map is not a false alarm" "$out" ;;
                *) pass "a declared map is not a false alarm" ;; esac

# A submodule has a .git file too, and its "parent" is the superproject's
# modules directory — not a checkout.
sub="$work/host/vendor/sub"; mkdir -p "$sub/.ddev"
printf 'name: sub\ntype: php\n' > "$sub/.ddev/config.yaml"
printf 'gitdir: ../../.git/modules/vendor/sub\n' > "$sub/.git"
out="$(cd "$sub" && "$cmd" env --slug sm 2>&1 || true)"
case "$out" in *"but no DDEV"*) fail "a submodule is not mistaken for a worktree" "$out" ;;
                *) pass "a submodule is not mistaken for a worktree" ;; esac

# The container check asks docker about a name, and deriving that name by hand
# got it wrong: it read .ddev/config.yaml only, while DDEV merges config.*.yaml
# and a worktree of a `name:`-pinned repo must override `name` in exactly such a
# file. 62 of 66 fleet repos pin it, so the pilots' own shape — no `name:` — is
# the one that hides this.
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || fail "init exited non-zero" ""
out="$(cd "$wt" && DDEV_SITENAME=pinned-wt-a "$cmd" check --slug wt-a 2>&1 || true)"
contains "check asks docker about the name DDEV uses, not one it re-derives" \
  "ddev-pinned-wt-a-hostshift" "$out"

# Loopback containment, compared entry by entry.
#
# `acme.fi:127.0.0.1` is a substring of `www.acme.fi:127.0.0.1`, so an unanchored
# match against docker's whole ExtraHosts blob called the apex domain contained
# whenever the `www` name was — the commonest hostname pair WordPress has, and
# exactly what a containment file generated before the apex alias joined the map
# looks like. `check` then said nothing while web resolved the apex over public
# DNS.
#
# A `docker` on PATH ahead of the real one, so the actual comparison runs.
pc="$work/contain"; newproject "$pc"
printf 'sites:\n  - canonical: https://www.contain.fi\n    aliases:\n      - https://contain.fi\n    variant: https://ct--contain.ddev.site\n' \
  > "$pc/hostshift.yaml"
(cd "$pc" && "$cmd" init --slug ct >/dev/null 2>&1) || fail "init for the containment fixture" ""
webhosts="$(sed -n 's/^HOSTSHIFT_WEB_HOSTS=//p' "$pc/.ddev/.env")"
cvariants="$(sed -n 's/^HOSTSHIFT_VARIANTS=//p' "$pc/.ddev/.env")"
cargs="$(sed -n 's/^HOSTSHIFT_ARGS=//p' "$pc/.ddev/.env")"
# Answers as the two containers `check` interrogates: the proxy (running, with
# the map .ddev/.env asks for) and web (serving the right hostnames, and
# containing only the `www` name). Everything else exits non-zero, as the real
# docker does for a container that is not there.
cat > "$work/docker" <<SHIM
#!/usr/bin/env bash
target=""
for a in "\$@"; do
  case "\$a" in
    *-hostshift) target=proxy ;;
    *-web)       target=web ;;
  esac
done
for a in "\$@"; do
  case "\$target:\$a" in
    web:*HostConfig.ExtraHosts*) printf 'www.contain.fi:127.0.0.1\n'; exit 0 ;;
    web:*Config.Env*)            printf 'VIRTUAL_HOST=%s\n' "$webhosts"; exit 0 ;;
    proxy:*State.Running*)
      printf 'true\nhostshift proxy $cargs \nVIRTUAL_HOST=%s\n' "$cvariants"; exit 0 ;;
  esac
done
exit 1
SHIM
chmod +x "$work/docker"
out="$(cd "$pc" && "$cmd" check --slug ct 2>&1 || true)"
rm -f "$work/docker"
contains "the apex domain is not contained by its www sibling" \
  "not pinned to the" "$out"
case "$out" in
  *"    contain.fi"*) pass "and it is the apex that is named" ;;
  *) fail "and it is the apex that is named" "$out" ;;
esac
case "$out" in
  *"    www.contain.fi"*) fail "while the contained name is not named" "$out" ;;
  *) pass "while the contained name is not named" ;;
esac

# And the remedy it prints has to be the one that works. `loopback` writes to
# stdout and exits; a developer who ran the command as it was printed — without
# the redirect — saw the shipped placeholder file unchanged, restarted, and
# believed containment was in place while wp-cron kept reaching production.
contains "the remedy redirects loopback into the file" \
  "ddev hostshift loopback > .ddev/docker-compose.hostshift-loopback.yaml" "$out"

# And the file that remedy writes must not carry DDEV's generated-file marker.
#
# DDEV greps for the literal string `#ddev-generated` anywhere in a file and
# overwrites it on `ddev add-on get`. The header said "this file carries no
# #ddev-generated marker, so edits survive" — and that sentence *was* the
# marker, so an upgrade silently replaced a containment file the developer had
# added hosts to with the shipped www.example.com placeholder. Under
# production-canonical that is wp-cron reaching the client's live site again,
# and `check` only names hostnames hostshift.yaml knows, so hand-added ones go
# unmentioned. The whole point of the paragraph was to say the file is safe.
lb="$work/loopmarker"; newproject "$lb"
printf 'sites:\n  - canonical: https://www.loopmarker.fi\n    variant: https://lm--loopmarker.ddev.site\n' \
  > "$lb/hostshift.yaml"
out="$(cd "$lb" && "$cmd" loopback --slug lm 2>/dev/null || true)"
contains "loopback emits the containment file" "127.0.0.1" "$out"
case "$out" in
  *"#ddev-generated"*)
    fail "and it carries no generated-file marker" "$out" ;;
  *) pass "and it carries no generated-file marker" ;;
esac

# copy-db refuses when the worktree is configured to *use* the parent's
# database. Sharing is as often set in a compose override as in config.*.yaml.
mkdir -p "$wt/.ddev"
printf 'services:\n  web:\n    environment:\n      - DATABASE_URL=mysql://db:db@ddev-acme-db:3306/db\n' \
  > "$wt/.ddev/docker-compose.sharedb.yaml"
out="$(cd "$wt" && "$cmd" copy-db 2>&1 || true)"
contains "copy-db sees sharing configured in a compose override" \
  "configured to *use*" "$out"
rm -f "$wt/.ddev/docker-compose.sharedb.yaml"

# ...and in .ddev/.env.web, DDEV's own documented place for per-service
# environment, which the file scan did not read. Measured live: DB_HOST set
# there, the worktree serving off the parent's database, and copy-db reporting
# "copied" into this worktree's idle db container — so the developer who ran it
# specifically to stop writing to the parent was told they had.
printf 'DB_HOST=ddev-acme-db\n' > "$wt/.ddev/.env.web"
out="$(cd "$wt" && "$cmd" copy-db 2>&1 || true)"
contains "copy-db sees sharing configured in .ddev/.env.web" \
  "configured to *use*" "$out"
rm -f "$wt/.ddev/.env.web"

# Bedrock keeps DB_HOST in the project root .env, not under .ddev/. Neither half
# of the guard saw it: the file list stopped at .ddev/, and `printenv` in the web
# container cannot see it either, because phpdotenv reads that file inside PHP.
# It is the shape most likely in this fleet.
printf 'DB_NAME=db\nDB_HOST=ddev-acme-db\nWP_ENV=development\n' > "$wt/.env"
out="$(cd "$wt" && "$cmd" copy-db 2>&1 || true)"
contains "copy-db sees sharing configured in the project root .env" \
  "configured to *use*" "$out"
rm -f "$wt/.env"

# A comment is not a configuration. This refused, printed the `#` back, and
# --force does not bypass this guard — so a stale note permanently blocked a
# legitimate copy and the message told the developer to remove something they
# already had.
printf '# DB_HOST=ddev-acme-db   # disabled, we have our own db now\n' > "$wt/.ddev/.env.web"
out="$(cd "$wt" && "$cmd" copy-db 2>&1 || true)"
case "$out" in
  *"configured to *use*"*) fail "a commented-out override does not block the copy" "$out" ;;
  *) pass "a commented-out override does not block the copy" ;;
esac
rm -f "$wt/.ddev/.env.web"

# Same for an override renamed aside to turn it off. DDEV's per-service form is
# .ddev/.env.<service>, and a service name has no dot in it.
printf 'DB_HOST=ddev-acme-db\n' > "$wt/.ddev/.env.web.disabled"
out="$(cd "$wt" && "$cmd" copy-db 2>&1 || true)"
case "$out" in
  *"configured to *use*"*) fail "an override renamed aside does not block the copy" "$out" ;;
  *) pass "an override renamed aside does not block the copy" ;;
esac
rm -f "$wt/.ddev/.env.web.disabled"

# Compose writes environment entries as `- "KEY=value"` in its own docs, and
# every quoted spelling walked past the guard.
for form in '      - "DB_HOST=ddev-acme-db"' "      - 'DB_HOST=ddev-acme-db'" '      "DB_HOST": ddev-acme-db'; do
  printf 'services:\n  web:\n    environment:\n%s\n' "$form" > "$wt/.ddev/docker-compose.q.yaml"
  out="$(cd "$wt" && "$cmd" copy-db 2>&1 || true)"
  case "$out" in
    *"configured to *use*"*) pass "copy-db sees a quoted compose entry:$form" ;;
    *) fail "copy-db sees a quoted compose entry:$form" "$out" ;;
  esac
done
rm -f "$wt/.ddev/docker-compose.q.yaml"

# A sibling project whose .ddev/.env cannot be read is not this project's
# problem. It was: the only sed in the file reading a foreign file had no
# `|| true`, so under `set -e` a failed command substitution took out init,
# check, env, loopback and wp-cli at once — including the check the post-start
# hook runs on every start.
sib="$(dirname "$wt")/zz-unreadable-sibling"
mkdir -p "$sib/.ddev"
printf 'HOSTSHIFT_VARIANTS=zz--acme.ddev.site\n' > "$sib/.ddev/.env"
chmod 000 "$sib/.ddev/.env"
out="$(cd "$wt" && "$cmd" env --slug wt-a 2>&1 || true)"
chmod 600 "$sib/.ddev/.env"; rm -rf "$sib"
case "$out" in
  *"Permission denied"*|"") fail "an unreadable sibling .ddev/.env is survivable" "$out" ;;
  *HOSTSHIFT_VARIANTS=*) pass "an unreadable sibling .ddev/.env is survivable" ;;
  *) fail "an unreadable sibling .ddev/.env is survivable" "$out" ;;
esac

# `--slug ""` was refused and `--slug=` was not, so the empty value pinned the
# branch-derived name as though it had been chosen.
if (cd "$wt" && "$cmd" env --slug= >/dev/null 2>&1); then
  fail "--slug= is refused like --slug ''" "exited 0"
else
  pass "--slug= is refused like --slug ''"
fi

# Nothing shared: copy-db must get past the guard. A guard that refuses
# everything passes both tests above and is useless.
out="$(cd "$wt" && "$cmd" copy-db 2>&1 || true)"
case "$out" in
  *"configured to *use*"*) fail "and does not refuse when nothing is shared" "$out" ;;
  *) pass "and does not refuse when nothing is shared" ;;
esac

echo "== check, past the container gate"

# Everything in `check` after the first `docker inspect` had never run.
#
# This suite has no Docker, so every inspect returned empty and check always
# bailed at "no ddev-…-hostshift container" — which meant the map comparison, the
# variant-resolvability test, the collision gate, the running-map diff against
# `docker logs` and the web VIRTUAL_HOST check were all unexecuted, in a file
# where four separate audit rounds have found bugs in exactly those blocks. A
# fake `docker` that emits the shapes the real one does is enough to drive it to
# the end.
fakebin="$work/fakebin"
mkdir -p "$fakebin"
cat > "$fakebin/docker" <<'FAKE'
#!/usr/bin/env bash
# Reads its answers from files the test writes, so each case is explicit.
case "$1" in
  inspect)
    name="$2"
    # The approot label identifies a project whatever it is called, and the
    # scan uses it to recognise its own container after a `name:` edit.
    case "$*" in
      *com.ddev.approot*) cat "${HS_FAKE_DIR}/approot" 2>/dev/null || true; exit 0 ;;
    esac
    case "$name" in
      *-hostshift) cat "${HS_FAKE_DIR}/hostshift-state" 2>/dev/null || true ;;
      *-web)       cat "${HS_FAKE_DIR}/web-env" 2>/dev/null || true ;;
      *)           exit 1 ;;
    esac ;;
  logs) cat "${HS_FAKE_DIR}/logs" 2>/dev/null || true ;;
  # The running proxies, which is the only place a renamed project still exists.
  ps) cat "${HS_FAKE_DIR}/ps" 2>/dev/null || true ;;
esac
exit 0
FAKE
chmod +x "$fakebin/docker"
export HS_FAKE_DIR="$work/fake"
mkdir -p "$HS_FAKE_DIR"

# A healthy worktree: the proxy is up, answering on the variants, started with
# the map .ddev/.env asks for, and web holds exactly the narrowed list.
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || fail "init exited non-zero" ""
env_args="$(sed -n 's/^HOSTSHIFT_ARGS=//p' "$wt/.ddev/.env")"
env_variants="$(sed -n 's/^HOSTSHIFT_VARIANTS=//p' "$wt/.ddev/.env")"
env_web="$(sed -n 's/^HOSTSHIFT_WEB_HOSTS=//p' "$wt/.ddev/.env")"
writefake() {
  printf 'true
proxy %s 
VIRTUAL_HOST=%s
' "$env_args" "$env_variants" > "$HS_FAKE_DIR/hostshift-state"
  printf 'VIRTUAL_HOST=%s
' "$env_web" > "$HS_FAKE_DIR/web-env"
  : > "$HS_FAKE_DIR/logs"
}
writefake
out="$(cd "$wt" && PATH="$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" \
  && pass "check reaches the end and reports what is served" \
  || fail "check reaches the end and reports what is served" "$out"
contains "and names the variants" "hostshift is serving" "$out"

# A proxy started with a different map than the file now asks for — the shape
# that shipped twice as a fleet-wide 421.
writefake
printf 'true
proxy --slug something-else 
VIRTUAL_HOST=%s
' "$env_variants" > "$HS_FAKE_DIR/hostshift-state"
out="$(cd "$wt" && PATH="$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
contains "check catches a proxy running a different map" "different map" "$out"

# A proxy answering on hostnames the file no longer asks for.
writefake
printf 'true
proxy %s 
VIRTUAL_HOST=stale--acme.ddev.site
' "$env_args" > "$HS_FAKE_DIR/hostshift-state"
out="$(cd "$wt" && PATH="$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
contains "check catches a proxy answering on stale hostnames" "the running proxy answers on" "$out"

# A crashed proxy: the container exists, the env is still there, it is not up.
writefake
printf 'false
proxy %s 
VIRTUAL_HOST=%s
' "$env_args" "$env_variants" > "$HS_FAKE_DIR/hostshift-state"
out="$(cd "$wt" && PATH="$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
contains "check catches a proxy that has exited" "has exited" "$out"

# web serving more than the narrowed list — the inversion that made a worktree
# steal the parent's blog, and which check could not see until it looked at web.
writefake
printf 'VIRTUAL_HOST=%s,b.acme.ddev.site
' "$env_web" > "$HS_FAKE_DIR/web-env"
out="$(cd "$wt" && PATH="$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
contains "check catches web serving hostnames it should not" "web is serving a different set" "$out"

# The running map differing from what hostshift.yaml now resolves to. Only
# checked when a hostshift.yaml is present, since otherwise the command line is
# the map and the comparison above already covered it.
printf 'version: 1\nsites:\n  - {name: main, canonical: https://www.acme.example, base: https://acme.ddev.site}\n' \
  > "$wt/hostshift.yaml"
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || true
env_args="$(sed -n 's/^HOSTSHIFT_ARGS=//p' "$wt/.ddev/.env")"
env_variants="$(sed -n 's/^HOSTSHIFT_VARIANTS=//p' "$wt/.ddev/.env")"
env_web="$(sed -n 's/^HOSTSHIFT_WEB_HOSTS=//p' "$wt/.ddev/.env")"
writefake
printf 'hostshift: map from /project/hostshift.yaml\nmain  https://old.example  ->  https://wt-a--acme.ddev.site\nhostshift v1: listening on :80, upstream http://web\n' \
  > "$HS_FAKE_DIR/logs"
out="$(cd "$wt" && PATH="$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
contains "check catches a hostshift.yaml edited without a restart" \
  "running a different map" "$out"

# The image's version against the command's. `ddev add-on get` installs the
# command from the repository and the engine from a published image, so a
# developer can run today's command in front of last week's proxy — measured, a
# feed came back with eighteen canonical URLs unrewritten while check said
# "hostshift is serving", because it compared the logged map and never the
# version.
writefake
printf 'hostshift v0.0.1-old: listening on :80, upstream http://web\n' >> "$HS_FAKE_DIR/logs"
out="$(cd "$wt" && PATH="$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
case "$out" in
  *"the proxy image is"*) pass "check notices an image older than the command" ;;
  *) fail "check notices an image older than the command" "$out" ;;
esac

# v0.1.0's banner carries no version — the version was added to that line after
# it — so requiring one made the warning silent on the only skew that exists.
writefake
printf 'hostshift: listening on :80, upstream http://web\n' >> "$HS_FAKE_DIR/logs"
out="$(cd "$wt" && PATH="$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
case "$out" in
  *"predates v0.2.0"*) pass "and one whose banner predates the version" ;;
  *) fail "and one whose banner predates the version" "$out" ;;
esac
contains "and says what that costs" "serialized length prefixes" "$out"
# ...and the way out. A pull cannot help here — `:latest` is byte-identical to
# the image being warned about, so the developer loops. The sentence saying so
# was on the version-*mismatch* branch, where it is unreachable: that branch is
# entered because the banner carried a version.
contains "and that a pull may not be enough" "built from the repository" "$out"

# …including when this command is a source build.
#
# The version comparison is suppressed when either side says `dev`, so a source
# build does not warn against every released image. That suppression silenced
# the one skew that exists: a developer running a source build in front of the
# published image, which is v0.1.0. Measured on that pair — options.php wrote
# `s:54:` over a 44-byte string, PHP refused the row, and check exited 0.
#
# A bannerless proxy is evidence on its own and needs no comparison.
writefake
printf 'hostshift: listening on :80, upstream http://web\n' >> "$HS_FAKE_DIR/logs"
cat > "$fakebin/hostshift" <<'DEVVER'
#!/usr/bin/env bash
if [ "$1" = "--version" ]; then echo dev; exit 0; fi
exec "$HS_REAL_BIN" "$@"
DEVVER
chmod +x "$fakebin/hostshift"
out="$(cd "$wt" && HS_REAL_BIN="$(command -v hostshift)" PATH="$fakebin:$PATH" \
  "$cmd" check --slug wt-a 2>&1 || true)"
rm -f "$fakebin/hostshift"
case "$out" in
  *"predates v0.2.0"*) pass "a source build does not silence the bannerless warning" ;;
  *) fail "a source build does not silence the bannerless warning" "$out" ;;
esac
# The HTTP probe: a hard routing failure is fatal, an application answering is
# not, and a cold start is retried rather than refused.
#
# It runs inside the post-start hook, the instant after `ddev start` returns,
# when traefik may not have picked up the new router yet — so a single request
# would turn an ordinary race into a refusal on a good project.
fakecurl="$work/fakecurl"
mkdir -p "$fakecurl"
cat > "$fakecurl/curl" <<'FAKECURL'
#!/usr/bin/env bash
# Emits the codes listed in $HS_CURL_CODES, one per call, last one repeating.
# Records which host each call asked for, so a test can assert that every
# variant is probed and not only the first.
f="${HS_CURL_STATE:-/tmp/hs-curl-n}"
n=$(( $(cat "$f" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$f"
# The body too, since check now reads what the page says about itself.
hdr=""
prev=""
_redirected=""
for a in "$@"; do
  case "$a" in https://*) printf '%s\n' "$a" >> "${HS_FAKE_DIR}/curl-hosts" ;; esac
  [ "$prev" = "-D" ] && hdr="$a"
  prev="$a"
done
# Headers when asked for them, so a test can drive a redirect. HS_CURL_LOCATION
# is served once, on the first call that asks for headers — hostshift's own
# first probe does not, so keying this on the call number made it never fire and
# the off-deployment test pass without a redirect ever being offered.
if [ -n "$hdr" ]; then
  if [ -n "${HS_CURL_LOCATION:-}" ] && [ ! -f "${HS_FAKE_DIR}/curl-redirected" ]; then
    : > "${HS_FAKE_DIR}/curl-redirected"
    _redirected=now
    printf 'HTTP/1.1 302 Found\r\nLocation: %s\r\n\r\n' "$HS_CURL_LOCATION" > "$hdr"
  elif [ -n "${HS_CURL_LOCATION2:-}" ] && [ ! -f "${HS_FAKE_DIR}/curl-redirected2" ]; then
    # A second hop, so a test can drive a chain. Which URL a chain is *recorded*
    # under is the whole question: the one that set out, or the one that
    # happened to be current when the redirect arrived.
    : > "${HS_FAKE_DIR}/curl-redirected2"
    _redirected=now
    printf 'HTTP/1.1 302 Found\r\nLocation: %s\r\n\r\n' "$HS_CURL_LOCATION2" > "$hdr"
  else
    printf 'HTTP/1.1 200 OK\r\n\r\n' > "$hdr"
  fi
fi
# An empty body on the call that carries the redirect, which is what a real
# `wp_redirect()` to a hostname the map does not name produces: the proxy leaves
# the Location alone, hostshift declines to follow off the deployment, and what
# comes back is a 302 with nothing in it.
if [ -n "${HS_CURL_EMPTY_ON_REDIRECT:-}" ] && [ -n "$hdr" ] && [ "$_redirected" = "now" ]; then
  # Nothing at all, not even the status code this fake appends elsewhere: the
  # caller here reads stdout *as the body*, and printing "302" into it made the
  # body non-empty, which is precisely the condition under test.
  exit 0
fi

# A second body from the second call on, so a test can put the leak on a page
# other than the first — which is where it is on a multisite.
if [ -n "${HS_CURL_BODY2:-}" ] && [ "$n" -ge 2 ]; then
  printf '%s\n' "$HS_CURL_BODY2"
elif [ -n "${HS_CURL_BODY:-}" ]; then
  printf '%s\n' "$HS_CURL_BODY"
fi
set -- ${HS_CURL_CODES:-200}
[ "$n" -le $# ] && eval "printf '%s' "\${$n}"" || eval "printf '%s' "\${$#}""
exit 0
FAKECURL
chmod +x "$fakecurl/curl"
export HS_CURL_STATE="$work/curl-n"

echo "== check looks at what came back, not only at the configuration"

# check said "the map is injective and anchored" and exited 0 on a deployment
# serving thirty-nine live production links per page, because everything it
# asked about was configuration and nothing asked what the page contained. The
# bytes were already in hand: it fetches one to decide whether the proxy is
# answering at all.
#
# The cause in the report was --dry-run surviving in HOSTSHIFT_ARGS — which the
# preservation added one round earlier is what let it stick — so the fake proxy
# here is running with it, and the fake page carries the canonical origin.
cat > "$fakebin/curl" <<'FAKE'
#!/usr/bin/env bash
# The probe asks for a page and appends the status code; give it both.
cat "${HS_FAKE_DIR}/page" 2>/dev/null
printf '\n200\n'
FAKE
chmod +x "$fakebin/curl"

writefake
# The proxy is up, and running with --dry-run.
printf 'true\nproxy %s --dry-run \nVIRTUAL_HOST=%s\n' "$env_args" "$env_variants" \
  > "$HS_FAKE_DIR/hostshift-state"
printf '<a href="https://acme.ddev.site/a">a</a><link href="https://acme.ddev.site/b">\n' \
  > "$HS_FAKE_DIR/page"

out="$(cd "$wt" && PATH="$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
contains "a canonical origin in the served page is reported" "canonical origin" "$out"

# ...and the pages it lists under that are URLs and nothing else. The list is
# word-split when it is printed, and the redirect page's label reads "(after its
# redirect)", so storing the label in the list turned it into three more lines
# that a reader would take for hostnames.
case "$out" in
  *"
    its"*|*"
    (after"*|*"
    redirect)"*)
    fail "and the pages it lists are URLs" "the label was word-split into the list" ;;
  *) pass "and the pages it lists are URLs" ;;
esac
contains "and the cause is named" "--dry-run" "$out"
if (cd "$wt" && PATH="$fakebin:$PATH" "$cmd" check --slug wt-a >/dev/null 2>&1); then
  fail "check fails when the page carries one" "it exited 0"
else
  pass "check fails when the page carries one"
fi

# And a page with none of them is still healthy, so this is not a check that is
# red on everything — the failure mode its own predecessor had.
#
# The mailpit link is the case that matters: a *declined* candidate. Every
# candidate is counted in the engine's JSON, deliberately, and the first version
# of the sum fell through the one-line `"rewrites": {}` into the candidates
# block — so an ordinary variant page linking the project's own mailpit, or
# carrying a near-miss hostname, reported a leak and failed the post-start hook
# on every `ddev start`.
printf '<a href="https://wt-a--acme.ddev.site/a">a</a><a href="https://acme.ddev.site:8026/">mailpit</a>\n' \
  > "$HS_FAKE_DIR/page"
out="$(cd "$wt" && PATH="$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
case "$out" in
  *"canonical origin"*) fail "a clean page is not reported" "$out" ;;
  *) pass "a clean page is not reported" ;;
esac

# The verdict above is an accident, and the accident is the only thing making
# the exit code non-zero.
#
# `fails=$((fails + 1))` at ddev/commands/host/hostshift:1758 is the whole of
# the scan's bookkeeping: `fails` is never initialised and never read, and the
# check block ends in an unconditional `exit 0` a few lines further down. Under
# this file's `set -u` the increment therefore aborts the script with
# "fails: unbound variable" — which is why it exits non-zero, and which also
# skips the parent-declares verdict and the "hostshift is serving" summary that
# were supposed to follow, ending the run on an internal bash error naming a
# variable no developer has heard of.
#
# And where anything at all defines `fails` — a wrapper script, an exported
# shell variable — the increment succeeds, the block falls through to `exit 0`,
# and check prints "the one thing this tool exists to prevent" followed by
# "hostshift is serving:" and reports success. That is exactly the false GREEN
# the scan was added to close, one line to the side of it.
printf '<a href="https://acme.ddev.site/a">a</a><link href="https://acme.ddev.site/b">\n' \
  > "$HS_FAKE_DIR/page"
out="$(cd "$wt" && PATH="$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
case "$out" in
  *"unbound variable"*)
    fail "check does not abort on its own bookkeeping" "$out" ;;
  *) pass "check does not abort on its own bookkeeping" ;;
esac
if (cd "$wt" && fails=0 PATH="$fakebin:$PATH" "$cmd" check --slug wt-a >/dev/null 2>&1); then
  fail "check fails on a canonical origin however \$fails arrives" "it exited 0"
else
  pass "check fails on a canonical origin however \$fails arrives"
fi

# And the scan sees one spelling of an origin: a literal `//host`.
#
# Every other spelling this project exists to handle is invisible to it. A
# WordPress page carries its origins JSON-escaped in `<script type=
# "application/json">` and every `wp_json_encode` block attribute, and
# percent-encoded in the `redirect_to=` of every wp-login link. Both are
# dereferenceable — the browser decodes them — and both are what a hole in one
# encoder leaves behind after the HTML surface has been rewritten correctly,
# which is the shape of every leak this repository has found. The comma case is
# an ordinary two-origin `content=` attribute: `,` is simply not in the
# boundary class beside `/"'<> ?#:`.
for spelling in json pct refs comma; do
  case "$spelling" in
    json)  printf '<script type="application/json">{"home":"https:\\/\\/acme.ddev.site\\/wp-json\\/"}</script>\n' ;;
    pct)   printf '<a href="/wp-login.php?redirect_to=https%%3A%%2F%%2Facme.ddev.site%%2Fwp-admin%%2F">in</a>\n' ;;
    refs)  printf '<a href="https&#58;&#47;&#47;acme.ddev.site&#47;a">a</a>\n' ;;
    comma) printf '<meta name="x" content="https://acme.ddev.site,https://other.test">\n' ;;
  esac > "$HS_FAKE_DIR/page"
  out="$(cd "$wt" && fails=0 PATH="$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
  contains "a $spelling-encoded canonical origin is reported" "canonical origin" "$out"
done

rm -f "$fakebin/curl"
writefake

writefake
: > "$HS_CURL_STATE"
out="$(cd "$wt" && HS_CURL_CODES="502 502 200" PATH="$fakecurl:$fakebin:$PATH" \
  "$cmd" check --slug wt-a 2>&1)" \
  && pass "a cold-start 502 is retried, not refused" \
  || fail "a cold-start 502 is retried, not refused" "$out"

: > "$HS_CURL_STATE"
out="$(cd "$wt" && HS_CURL_CODES="502" PATH="$fakecurl:$fakebin:$PATH" \
  "$cmd" check --slug wt-a 2>&1 || true)"
contains "a persistent 502 is refused" "so something between the router" "$out"

# A 502 whose body says who failed is not a router fault.
#
# The proxy answers a failed upstream dial with `hostshift: upstream request
# failed: …` in the body, and this command already captures that body. The 502
# branch did not read it, so a stopped `web` — the commonest failure the compose
# file names — was reported as a stale router, with an escalation path ending at
# `ddev poweroff`, a fleet-wide action, for something `ddev start` fixes.
: > "$HS_CURL_STATE"
out="$(cd "$wt" && HS_CURL_CODES="502" \
  HS_CURL_BODY="hostshift: upstream request failed: dial tcp: lookup ddev-acme-wt-a-web: no such host" \
  PATH="$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
contains "a 502 from a dead upstream names the upstream" "cannot reach" "$out"
contains "and sends the developer to ddev start" "ddev start" "$out"
case "$out" in
  *"so something between the router"*)
    fail "and does not blame the router" "$out" ;;
  *) pass "and does not blame the router" ;;
esac
case "$out" in
  *"poweroff"*) fail "and does not escalate to poweroff" "$out" ;;
  *) pass "and does not escalate to poweroff" ;;
esac

# A timeout on the first attempt must not mask a 502 on the later ones. `rc` was
# set once outside the loop, so a cold-start timeout left it sticky and the
# refusal never ran — the failure this probe exists to catch, passed as healthy.
: > "$HS_CURL_STATE"
out="$(cd "$wt" && HS_CURL_CODES="000 502 502" HS_CURL_RC="28 0 0" \
  PATH="$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
contains "a first-attempt timeout does not mask a later 502" \
  "so something between the router" "$out"

# An application answering — a 404, a redirect to wp-signup, an auth challenge —
# means the request got there, which is the question being asked.
for code in 404 302 401 200; do
  : > "$HS_CURL_STATE"
  (cd "$wt" && HS_CURL_CODES="$code" PATH="$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a >/dev/null 2>&1) \
    && pass "http $code is an answer, not a routing failure" \
    || fail "http $code is an answer, not a routing failure" "$code was refused"
done
unset HS_CURL_STATE

# check asks the database which hostnames it holds.
#
# Adopting production-canonical is a pincer that configuration alone cannot see:
# a worktree whose branch predates the parent's hostshift.yaml has a map built
# from DDEV hostnames, which is *correct* while the database still holds them and
# both variants serve 200 — and check refused it on every start. Merge the file
# as that warning says and check goes green while every URL 302s to wp-signup,
# because the database has not moved. One query separates the two.
fakedb="$work/fakedb"
mkdir -p "$fakedb"
cat > "$fakedb/ddev" <<'FAKEDDEV'
#!/usr/bin/env bash
# `wp db query` — the stored row, not get_option('home'), which WordPress
# filters through WP_HOME and which therefore answers with generated config
# rather than with what the database holds.
case "$*" in
  # The working directory is part of the question, not decoration.
  #
  # This used to match on the query alone, so it answered wherever the script
  # ran wp-cli from — and the real `ddev exec -s web` lands in /var/www/html,
  # which is the project root, not the docroot. wp-cli said "This does not seem
  # to be a WordPress installation", the script swallowed it, and the gate was a
  # silent no-op on every project without a root wp-cli.yml — while this test
  # went on passing. An instrument that cannot fail on the real defect is not
  # measuring anything.
  # copy-db's table count, before the dump and again after it fails. Two answers
  # from one query, because that is the whole question the failure diagnosis
  # turns on: what was here before, and what is here now.
  *"information_schema.tables"*)
    _f="${HS_TBL_STATE:-/tmp/hs-tblcalls}"
    _n=$(( $(cat "$_f" 2>/dev/null || echo 0) + 1 ))
    echo "$_n" > "$_f"
    if [ "$_n" = 1 ]; then printf '%s\n' "${HS_FAKE_TABLES_BEFORE:-0}"
    else printf '%s\n' "${HS_FAKE_TABLES_AFTER:-0}"; fi ;;
  # The dump itself, which these tests are here to fail.
  *mysqldump*)
    printf '%s\n' "${HS_FAKE_DUMP_ERR:-ERROR 2005 (HY000): Unknown server host}" >&2
    exit 1 ;;
  *"-d /var/www/html/web"*"option_name='home'"*) printf '%s\n\n' "${HS_FAKE_HOME:-}" ;;
  *"option_name='home'"*)
    echo "Error: This does not seem to be a WordPress installation." >&2
    echo "The used path is: /var/www/html/" >&2
    exit 1 ;;
esac
exit 0
FAKEDDEV
chmod +x "$fakedb/ddev"
writefake

# What copy-db says after a failed dump, which is a message a developer acts on
# destructively.
#
# Round 54 printed "may be half-replaced … re-run with --force" for every failure
# mode, so the ordinary "you forgot to start the parent" case ended by naming the
# one destructive flag. Round 55 replaced that with a table count and compared it
# against *zero* rather than against the count taken before the copy — which the
# same block already holds — so it was confidently wrong in both directions.
#
# A connection that never opened cannot have written anything, and the driver
# says so. This is the commonest failure there is — the parent not running, which
# is *why* the connect failed — and hedging over it sent the developer to verify
# a database nothing had touched.
: > "$work/tblcalls"
out="$(cd "$wt" && HS_TBL_STATE="$work/tblcalls" \
  HS_FAKE_TABLES_BEFORE=1 HS_FAKE_TABLES_AFTER=1 \
  PATH="$fakedb:$fakebin:$PATH" "$cmd" copy-db --force 2>&1 || true)"
contains "a dump that never connected touched nothing" "never connected" "$out"
case "$out" in
  *"does not settle it"*) fail "and it does not hedge over it" "$out" ;;
  *) pass "and it does not hedge over it" ;;
esac

# A failure that is *not* a connect error, on an empty database: nothing was
# there to lose, and saying so is the point.
: > "$work/tblcalls"
out="$(cd "$wt" && HS_TBL_STATE="$work/tblcalls" \
  HS_FAKE_DUMP_ERR="mariadb-dump: Error 2026: TLS/SSL error: unexpected eof" \
  HS_FAKE_TABLES_BEFORE=0 HS_FAKE_TABLES_AFTER=0 \
  PATH="$fakedb:$fakebin:$PATH" "$cmd" copy-db 2>&1 || true)"
contains "an empty database that stayed empty lost nothing" "nothing was lost" "$out"
case "$out" in
  *"half-replaced"*) fail "and it does not claim a half-replaced database" "$out" ;;
  *) pass "and it does not claim a half-replaced database" ;;
esac
case "$out" in
  *"--force"*) fail "and it does not name the destructive flag" "$out" ;;
  *) pass "and it does not name the destructive flag" ;;
esac

# The other direction: the table list intact and the rows gone.
# --add-drop-database recreates the whole list at the top of the stream and fills
# it as the dump arrives, so an unchanged count is exactly what a copy
# interrupted late looks like — measured live at 13 tables either side with 1.2M
# rows gone. A count cannot settle this and the tool must not pretend it can.
: > "$work/tblcalls"
out="$(cd "$wt" && HS_TBL_STATE="$work/tblcalls" \
  HS_FAKE_DUMP_ERR="mariadb-dump: Error 2026: TLS/SSL error: unexpected eof" \
  HS_FAKE_TABLES_BEFORE=13 HS_FAKE_TABLES_AFTER=13 \
  PATH="$fakedb:$fakebin:$PATH" "$cmd" copy-db --force 2>&1 || true)"
case "$out" in
  *"nothing here changed"*|*"nothing was lost"*)
    fail "an unchanged table count does not mean an unchanged database" "$out" ;;
  *) pass "an unchanged table count does not mean an unchanged database" ;;
esac
contains "and it says what a count cannot settle" "does not settle it" "$out"
# The database still holds a hostname the map does not name.
# A warning, never a refusal: this gate has been wrong in three consecutive
# rounds, and PLAN §4.1 says why — the application cannot be interrogated. It
# keeps the signal and gives up the authority, so a wrong answer costs a line of
# noise rather than a failed start.
if out="$(cd "$wt" && HS_FAKE_HOME="https://www.somewhere-else.test" \
  PATH="$fakedb:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)"; then
  pass "a database the map does not name does not fail the start"
else
  fail "a database the map does not name does not fail the start" "$out"
fi
contains "but it does say so" "the database says its home is" "$out"
contains "and never suggests a search-replace" "moves production" "$out"

# ...and when the page proves it — five or more links to that hostname — it is a
# refusal, not a warning.
#
# This is the day-one state of every production-canonical site: the database
# holds production hostnames, no hostshift.yaml has been adopted, and *nothing*
# on the page is rewritten. `check` exited 0, `ddev restart` reported success,
# and `diff` printed GREEN, because all three instruments count only origins the
# map names and the defect is that the map names none of them.
dbleak='<link rel="canonical" href="https://www.somewhere-else.test/">
<link rel="alternate" href="https://www.somewhere-else.test/feed/">
<link rel="https://api.w.org/" href="https://www.somewhere-else.test/wp-json/">
<link rel="shortlink" href="https://www.somewhere-else.test/?p=1">
<a href="https://www.somewhere-else.test/x">t</a>'
out="$(cd "$wt" && HS_FAKE_HOME="https://www.somewhere-else.test" \
  HS_CURL_BODY="$dbleak" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" && rc=0 || rc=$?
[ "$rc" = 2 ] && pass "a page proving the database's home is unmapped is refused" \
  || fail "a page proving the database's home is unmapped is refused" "exit $rc"
contains "and counts the links it found" "links to www.somewhere-else.test" "$out"
contains "and points at hostshift.yaml, not at the database" "Name www.somewhere-else.test in hostshift.yaml" "$out"
# ...and says nothing when it agrees.
out="$(cd "$wt" && HS_FAKE_HOME="https://acme.ddev.site" \
  PATH="$fakedb:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
case "$out" in
  *"the database says its home is"*) fail "and passes when the database agrees with the map" "$out" ;;
  *) pass "and passes when the database agrees with the map" ;;
esac

# The removal note's "does anything get inherited" test, run against the awk
# program install.yaml actually ships rather than a copy of it.
#
# `additional_hostnames: []` is what `ddev config` writes into every project by
# default, so matching the key alone told a developer their worktree might
# hijack the parent's hostnames at the moment they were tearing it down. The
# first fix anchored the empty-flow rule at end-of-line, which left
# `[] # comment` and a flow list closed on the next line still reading as
# inherited.
ahprog="$(sed -n "/&& awk '/,/^      ' /p" "$repo/ddev/install.yaml" \
  | sed -e "1s/.*&& awk '//" -e "\$d")"
ahcase() { # name, config body, expected (yes|no)
  printf 'name: x\n%b\n' "$2" > "$work/ah.yaml"
  if awk "$ahprog" "$work/ah.yaml" 2>/dev/null; then got=yes; else got=no; fi
  check "additional_hostnames $1" "$3" "$got"
}
ahcase "an empty list inherits nothing"          'additional_hostnames: []'            no
ahcase "an empty list with a comment likewise"   'additional_hostnames: [] # none'     no
ahcase "an empty list with spaces likewise"      'additional_hostnames: [  ]'          no
ahcase "a flow list closed on the next line"     'additional_hostnames: [\n]'          no
ahcase "a commented-out key"                     '#additional_hostnames:\n  - blog'    no
ahcase "no key at all"                           'type: php'                           no
ahcase "an inline list inherits"                 'additional_hostnames: [blog]'        yes
ahcase "a multi-item inline list"                'additional_hostnames: [blog, shop]'  yes
ahcase "a block list"                            'additional_hostnames:\n  - blog'     yes
ahcase "a block list after a comment"            'additional_hostnames:\n  # c\n  - b' yes
ahcase "a flow list spanning lines"              'additional_hostnames: [\n  - blog\n]' yes

# What the page says it is.
#
# A stock DDEV WordPress pins WP_HOME to DDEV_PRIMARY_URL — this project's own
# hostname, which is neither canonical nor variant. The preview then serves
# links to `<project>.ddev.site`, nothing is rewritten because nothing on the
# page names an origin the map knows, and every gate passed: injective and
# anchored, containers up, probe 200. Only `hostshift diff` caught it, and the
# README points worktree users at `check`.
# The real shape: WP_HOME pinned puts the hostname in canonical, feed, REST,
# oEmbed and shortlink links, thirty-odd times in <head> alone.
pinned='<link rel="canonical" href="https://acme-wt-a.ddev.site/">
<link rel="alternate" href="https://acme-wt-a.ddev.site/feed/">
<link rel="https://api.w.org/" href="https://acme-wt-a.ddev.site/wp-json/">
<link rel="shortlink" href="https://acme-wt-a.ddev.site/?p=1">
<link rel="alternate" href="https://acme-wt-a.ddev.site/oembed/">
<a href="https://acme-wt-a.ddev.site/x">t</a>'
out="$(cd "$wt" && DDEV_HOSTNAME=acme-wt-a.ddev.site \
  HS_CURL_BODY="$pinned" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
contains "a page answering with a hostname the map does not name is called out" \
  "which is neither a canonical hostname nor a" "$out"
# And the remedy it prints has to name files this project actually has.
#
# The advice was written for stock WordPress — `wp-config-ddev.php` and a
# `#ddev-generated` marker — and this fleet is Bedrock, which has neither. Worse,
# on `wp-bedrock` DDEV writes WP_HOME into the project-root `.env` and rewrites
# it on every `ddev start`, so the one step a developer could act on was undone
# by the next restart, silently.
contains "and the remedy is for stock WordPress when there is no .env" \
  "wp-config-ddev.php" "$out"

# ...and when the page actually carries those hostnames, that is a refusal.
#
# Both leak scans look for origins *the map names*, so the state they cannot see
# is the one where the map names the wrong ones: a worktree whose branch predates
# the hostshift.yaml, against a database already on production hostnames. check
# printed two accurate warnings and exited 0 while the page carried 31 live
# production links, and diff printed GREEN for the same reason.
printf 'version: 1\nsites:\n  - {name: main, canonical: https://www.acme.example, base: https://acme.ddev.site}\n' \
  > "$main/hostshift.yaml"
mv "$wt/hostshift.yaml" "$work/wt-hs.hold" 2>/dev/null || true
cp "$wt/.ddev/.env" "$work/wt-env.hold" 2>/dev/null || true
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || true
env_args="$(sed -n 's/^HOSTSHIFT_ARGS=//p' "$wt/.ddev/.env")"
env_variants="$(sed -n 's/^HOSTSHIFT_VARIANTS=//p' "$wt/.ddev/.env")"
env_web="$(sed -n 's/^HOSTSHIFT_WEB_HOSTS=//p' "$wt/.ddev/.env")"
writefake
leaky='<link rel="canonical" href="https://www.acme.example/">
<link rel="alternate" href="https://www.acme.example/feed/">
<link rel="https://api.w.org/" href="https://www.acme.example/wp-json/">
<link rel="shortlink" href="https://www.acme.example/?p=1">
<a href="https://www.acme.example/x">t</a>'
out="$(cd "$wt" && HS_CURL_BODY="$leaky" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" && rc=0 || rc=$?
[ "$rc" = 2 ] && pass "a page carrying the parent's undeclared hostnames is refused" \
  || fail "a page carrying the parent's undeclared hostnames is refused" "exit $rc"
contains "and the count is measured, not inferred" "links to www.acme.example" "$out"
contains "and it does not suggest a search-replace" "moves production" "$out"

# An origin the map names *nowhere* — the apex sibling of the canonical.
#
# Every leak instrument in this product is map-scoped: the `--dry-run` scan sums
# origins the forward map matches, `diff`'s LEAKS column asks the same map, and
# the three gates that can speak about an unknown hostname each read one named
# list. A hostname on none of those lists is on no list at all, and rounds 50,
# 51 and 52 each found a different symptom of that one defect. Forgetting one
# `aliases:` entry is exactly the mistake hostshift.yaml exists to let you make.
cp "$wt/.ddev/.env" "$work/apex-env.hold" 2>/dev/null || true
mv "$wt/hostshift.yaml" "$work/apex-hs.hold" 2>/dev/null || true
printf 'sites:\n  - canonical: https://www.acme.example\n    variant: https://wt-a--acme.ddev.site\n' \
  > "$wt/hostshift.yaml"
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || true
env_args="$(sed -n 's/^HOSTSHIFT_ARGS=//p' "$wt/.ddev/.env")"
env_variants="$(sed -n 's/^HOSTSHIFT_VARIANTS=//p' "$wt/.ddev/.env")"
env_web="$(sed -n 's/^HOSTSHIFT_WEB_HOSTS=//p' "$wt/.ddev/.env")"
writefake
apexleak='<link rel="canonical" href="https://acme.example/">
<link rel="alternate" href="https://acme.example/feed/">
<link rel="https://api.w.org/" href="https://acme.example/wp-json/">
<link rel="shortlink" href="https://acme.example/?p=1">
<a href="https://acme.example/x">t</a>'
out="$(cd "$wt" && HS_CURL_BODY="$apexleak" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" && rc=0 || rc=$?
[ "$rc" = 2 ] && pass "an origin the map names nowhere is refused" \
  || fail "an origin the map names nowhere is refused" "exit $rc"
contains "and it is named with its count" "reference(s) to" "$out"
contains "and the host it counted" "acme.example" "$out"
contains "and the count is not attributed to one page" "across the pages probed" "$out"
# ...and the pages themselves are listed, which is the half that makes the sum
# actionable — a developer given a total and one page name counts fewer on it
# and doubts the tool.
contains "and the pages it counted are listed" "Counted across:" "$out"
# ...and the number really is the sum. Hardcoding 1 survived every assertion
# above, because "reference(s) to" matches whatever number precedes it.
case "$out" in
  *"— 5 reference(s) to"*|*"— 6 reference(s) to"*) pass "and the count is the sum" ;;
  *) fail "and the count is the sum" "expected the summed count in: $out" ;;
esac
contains "and the remedy is an alias" "as an alias of the canonical" "$out"

# ...and a third party is a note, not a refusal. A real WordPress page carries
# w3.org, gravatar and googleapis; refusing on those is the warning nobody reads.
third='<link rel="stylesheet" href="https://fonts.googleapis.com/a">
<link rel="stylesheet" href="https://fonts.googleapis.com/b">
<script src="https://fonts.googleapis.com/c"></script>
<script src="https://fonts.googleapis.com/d"></script>
<img src="https://fonts.googleapis.com/e">'
out="$(cd "$wt" && HS_CURL_BODY="$third" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" && rc=0 || rc=$?
[ "$rc" = 0 ] && pass "a third-party host is a note, not a refusal" \
  || fail "a third-party host is a note, not a refusal" "exit $rc"
# "reference(s)", not "links": round 55 found six `<svg xmlns>` declarations
# reported as links, and a namespace URI is not one a browser dereferences.
contains "and it is still counted out loud" "reference(s) to fonts.googleapis.com" "$out"

# A page that 302s to wp-signup.php is WordPress saying it does not know the
# hostname, and on a multisite that means nothing is previewable at all.
#
# check could not see this state: its reachability verdict reads the first
# variant only, and the sibling fetches discarded their status entirely — so a
# multisite where every page redirected to wp-signup reported "hostshift is
# serving" and exit 0 on every `ddev start`, while `hostshift diff` went RED on
# the same deployment in one page. The signup page carries only variant origins,
# so the leak scans found nothing either.
: > "$HS_FAKE_DIR/curl-hosts"; rm -f "$HS_FAKE_DIR/curl-redirected"
hs56n="$work/curl-n56"; echo 0 > "$hs56n"
out="$(cd "$wt" && HS_CURL_BODY='<p>signup</p>' HS_CURL_CODES="302 200 200" \
  HS_CURL_STATE="$hs56n" \
  HS_CURL_LOCATION="https://wt-a--acme.ddev.site/wp-signup.php?new=www.acme.example" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" && rc=0 || rc=$?
[ "$rc" = 2 ] && pass "a page that redirects to wp-signup is not healthy" \
  || fail "a page that redirects to wp-signup is not healthy" "exit $rc"
contains "and it names the table that actually decides" "wp_blogs" "$out"
contains "and it does not advise a search-replace" "moves production" "$out"

# ...but one absent subsite is not a broken deployment, and must not throw away
# the leak scans.
#
# Round 56 refused on *any* page that signed up, though its own comment described
# "a multisite where every page 302s". One missing `wp_blogs` row — a partial
# pull, a blog created after the dump, an archived subsite — then declared a
# healthy preview unpreviewable and failed every `ddev start`. And because the
# refusal sits before the unmapped-host scan, the canonical-origin scan and the
# census, `exit 2` threw all three away: measured, a deployment serving four live
# production origins reported the signup redirect *instead* of the leak.
mv "$wt/hostshift.yaml" "$work/multi-hs.hold" 2>/dev/null || true
printf 'sites:\n  - canonical: https://www.acme.example\n    variant: https://wt-a--acme.ddev.site\n  - canonical: https://shop.acme.example\n    variant: https://wt-a--shop.acme.ddev.site\n' \
  > "$wt/hostshift.yaml"
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || true
env_args="$(sed -n 's/^HOSTSHIFT_ARGS=//p' "$wt/.ddev/.env")"
env_variants="$(sed -n 's/^HOSTSHIFT_VARIANTS=//p' "$wt/.ddev/.env")"
env_web="$(sed -n 's/^HOSTSHIFT_WEB_HOSTS=//p' "$wt/.ddev/.env")"
writefake
: > "$HS_FAKE_DIR/curl-hosts"; rm -f "$HS_FAKE_DIR/curl-redirected"
hs57n="$work/curl-n57"; echo 0 > "$hs57n"
# The first variant answers 200 and carries a leak; the sibling — the first call
# that asks for headers — is the one that signs up.
out="$(cd "$wt" && HS_CURL_BODY='<a href="https://www.acme.example/x">t</a>' \
  HS_CURL_CODES="200 200 200" HS_CURL_STATE="$hs57n" \
  HS_CURL_LOCATION="https://wt-a--shop.acme.ddev.site/wp-signup.php?new=shop.acme.example" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
case "$out" in
  *"refusing to call this healthy"*"redirects to wp-signup"*)
    fail "one absent subsite is a warning, not a refusal" "refused the whole deployment" ;;
  *"redirect to"*"wp-signup.php"*) pass "one absent subsite is a warning, not a refusal" ;;
  *) fail "one absent subsite is a warning, not a refusal" "said nothing about it: $out" ;;
esac
contains "and it names the hostname that signed up" "wt-a--shop.acme.ddev.site" "$out"
# The load-bearing half: the scans it used to pre-empt still ran.
contains "and the leak scan still ran" "canonical origin" "$out"

# ...and the same, with the signup page returning an *empty* body.
#
# That is what a real `wp_redirect()` to a hostname this map does not name
# produces: the proxy leaves the Location alone, hs_fetch_local declines to
# follow off the deployment, and the 302 carries nothing. hs_add_page returns
# early on an empty body, so the page that signed up was removed from the
# denominator at the same moment it was added to the numerator — "fewer than
# all" came out false and the refusal fired anyway, on the one state round 57
# was written to stop refusing. The off-map target is the ordinary case on the
# production-canonical multisite this tool exists for.
: > "$HS_FAKE_DIR/curl-hosts"; rm -f "$HS_FAKE_DIR/curl-redirected"
hs58n="$work/curl-n58"; echo 0 > "$hs58n"
out="$(cd "$wt" && HS_CURL_BODY='<a href="https://www.acme.example/x">t</a>' \
  HS_CURL_CODES="200 200 200" HS_CURL_STATE="$hs58n" \
  HS_CURL_EMPTY_ON_REDIRECT=1 \
  HS_CURL_LOCATION="https://network.elsewhere.example/wp-signup.php?new=shop" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
case "$out" in
  *"refusing to call this healthy"*"redirects to wp-signup"*)
    fail "an empty signup body still counts as a page probed" \
      "refused the whole deployment" ;;
  *) pass "an empty signup body still counts as a page probed" ;;
esac
contains "and the leak on the page that serves is still reported" \
  "canonical origin" "$out"

# ...and a chain is recorded under the page that set out, not the hop it was on.
#
# hs_fetch_local follows a redirect while it stays on the deployment, so a
# sibling can 302 to another variant and *that* can 302 to wp-signup. Recording
# the current hop names a page that is serving perfectly well — here the healthy
# first variant — as the one WordPress does not recognise, and sends the
# developer to inspect the wrong site. The entry URL is also the only one
# commensurable with the count of pages probed.
: > "$HS_FAKE_DIR/curl-hosts"
rm -f "$HS_FAKE_DIR/curl-redirected" "$HS_FAKE_DIR/curl-redirected2"
hs58c="$work/curl-n58c"; echo 0 > "$hs58c"
out="$(cd "$wt" && HS_CURL_BODY='<p>fine</p>' \
  HS_CURL_CODES="200 200 200 200" HS_CURL_STATE="$hs58c" \
  HS_CURL_LOCATION="https://wt-a--acme.ddev.site/fi/" \
  HS_CURL_LOCATION2="https://network.elsewhere.example/wp-signup.php?new=shop" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
case "$out" in
  *"wp-signup.php"*) ;;
  *) fail "a redirect chain is blamed on the page that set out" \
       "the chain was not followed to the signup at all" ;;
esac
case "$out" in
  *"/fi/"*)
    fail "a redirect chain is blamed on the page that set out" \
      "named the hop it was on, not the page probed" ;;
  *) pass "a redirect chain is blamed on the page that set out" ;;
esac

mv "$work/multi-hs.hold" "$wt/hostshift.yaml" 2>/dev/null || true
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || true
env_args="$(sed -n 's/^HOSTSHIFT_ARGS=//p' "$wt/.ddev/.env")"
env_variants="$(sed -n 's/^HOSTSHIFT_VARIANTS=//p' "$wt/.ddev/.env")"
env_web="$(sed -n 's/^HOSTSHIFT_WEB_HOSTS=//p' "$wt/.ddev/.env")"
writefake

# The `hostshift diff` a test-28 refusal sends the developer to has to be a
# command that runs.
#
# Round 55 wrote this remedy and never ran it: `diff` requires --slug — the
# README calls it "not optional here", and there is no `ddev hostshift diff`
# wrapper to supply it — so the line as printed exited 2 with "no variant". The
# assertion that was supposed to cover it sat on a fixture whose proxy runs with
# --dry-run, which takes a different arm and never prints the remedy at all.
writefake
leak='<link rel="canonical" href="https://www.acme.example/">
<a href="https://www.acme.example/x">t</a>'
out="$(cd "$wt" && HS_CURL_BODY="$leak" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
# Round 56 printed a `hostshift diff` command here. Round 57 ran it: under
# production-canonical DDEV does not register the canonical hostname, so
# --resolve reaches the router, gets a 404, and diff compares zero pages —
# "1 pages, 0 leaks" on the message printed *for* a leak. The remedy now names
# something that works here and says what diff would need.
contains "the leak refusal offers a remedy that runs here" "--explain" "$out"
# ...and it greps for a word the proxy actually writes. Round 57 pointed at
# `grep rewrote`, which appears nowhere in the proxy's output: `--explain` set a
# flag, filled a buffer nothing read, and printed nothing at all. A developer
# mid test-28 incident recreated every container in the project on the tool's own
# instruction and learned nothing.
contains "and the remedy greps a word the proxy writes" "grep census" "$out"
contains "and it says why the router cannot answer for the canonical" "returns 404" "$out"
# The replacement itself, not only the absence of the old advice: the whole
# value of this remedy is that it runs, and asserting `docker port` alone let
# the container and the port both be wrong.
contains "and it routes diff at web's published port" "docker port ddev-" "$out"
contains "and at web, not the database container" "-web 443/tcp" "$out"
contains "and resolves the canonical to it" "--resolve" "$out"
contains "and the diff command it prints passes --slug" "hostshift diff --slug" "$out"
case "$out" in
  *"Add that hostname to"*|*"additional_fqdns first"*)
    fail "and it does not send them to /etc/hosts" "advised additional_fqdns" ;;
  *) pass "and it does not send them to /etc/hosts" ;;
esac
writefake

# A response the proxy passed through untouched is a leak, and only the log
# knew.
#
# JSON and text/* are read whole and skipped over --max-body; HTML is streamed
# and never capped. Measured on one ordinary page-builder post: a 9.4 MB REST
# response carried 19,390 live canonical origins and zero rewrites, the proxy
# logged the skip, and check said "hostshift is serving" and exited 0. check
# already reads `docker logs` twice and never looked.
writefake
printf 'time=x level=WARN msg="JSON body exceeds the size cap, passing through untouched" cap=8388608 content-type=application/json\n' \
  >> "$HS_FAKE_DIR/logs"
out="$(cd "$wt" && PATH="$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" && rc=0 || rc=$?
contains "a body over the size cap is reported" "unrewritten" "$out"
contains "and the count is measured" "1 response(s)" "$out"
contains "and the remedy is the flag that fixes it" "--max-body" "$out"
# A refusal, because exiting 0 here told PLAN §3's unattended agent that
# everything was fine on the largest test-28 breach the tool can produce —
# measured at 120,001 live production origins in one 9.96 MB response. Refusing
# is safe because it clears on the fix: raising --max-body needs a `ddev
# restart`, and DDEV recreates the container, so the log starts empty.
[ "$rc" = 2 ] && pass "and it refuses rather than exiting 0" \
  || fail "and it refuses rather than exiting 0" "exit $rc"
contains "and it says the restart clears it" "log starts empty" "$out"

# A *request* over the cap is the opposite failure, and was reported as this one.
#
# Three proxy WARNs carry the same phrase and one is the request direction.
# Counted together, a 10 MB block-editor save was reported as "responses went to
# the browser unrewritten … live production links". Measured: under the cap the
# variant hostname is turned back into the canonical before the app sees it;
# over it the variant reaches the application verbatim and is written into the
# shared production database. §4.3, described as its inverse.
writefake
printf 'time=x level=WARN msg="request body exceeds the size cap, passing through untouched" cap=8388608 content-type=application/json\n' \
  >> "$HS_FAKE_DIR/logs"
out="$(cd "$wt" && PATH="$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" && rc=0 || rc=$?
contains "a request over the cap is reported as a request" "reached" "$out"
contains "and it names the direction's own harm" "shared database" "$out"
# ...and the sweep it prints looks where the write actually lands. The message
# names a media upload and a block-editor save, and neither writes to
# wp_options: a save lands in post content, an upload in post meta. Measured —
# a 33 KB REST save put the variant in wp_posts while the printed query returned
# nothing and exit 0, which a developer reads as "nothing was written".
contains "and the sweep looks in post content" "post_content', count(*) from wp_posts" "$out"
contains "and at the guid" "guid', count(*) from wp_posts" "$out"
contains "and in post meta" "postmeta', count(*) from wp_postmeta" "$out"
contains "and says a subsite keeps its own tables" "wp_2_posts" "$out"

# ...and it queries the database the *application* writes to.
#
# `ddev exec -s db` is this project's own container. In a worktree sharing the
# parent's — the configuration this whole tool exists for — that container is
# idle, so the sweep printed zeros about the one thing the message above says
# has no undo, while the rows sat in production's database. copy-db has detected
# this state since round 40 and the remedies never asked it.
writefake
printf 'time=x level=WARN msg="request body exceeds the size cap, passing through untouched" cap=8388608 content-type=application/json\n' \
  >> "$HS_FAKE_DIR/logs"
sharedbin="$work/sharedbin"; mkdir -p "$sharedbin"
cat > "$sharedbin/ddev" <<'SHAREDDEV'
#!/usr/bin/env bash
# A worktree whose web container reads the parent's database.
case "$*" in
  *printenv*) echo "DB_HOST=ddev-acme-db" ;;
esac
exit 0
SHAREDDEV
chmod +x "$sharedbin/ddev"
out="$(cd "$wt" && PATH="$sharedbin:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
case "$out" in
  *"ddev exec -s db"*)
    fail "the sweep names the database the application writes to" \
      "pointed at this project's own idle container" ;;
  *"docker exec ddev-acme-db"*)
    pass "the sweep names the database the application writes to" ;;
  *) fail "the sweep names the database the application writes to" \
       "named neither: $out" ;;
esac
writefake

# ...and it reads a dotenv the container cannot see, which is where Bedrock
# keeps DB_HOST — the branch whose comment names this fleet's shape, and which
# nothing exercised.
writefake
printf 'time=x level=WARN msg="request body exceeds the size cap, passing through untouched" cap=8388608 content-type=application/json\n' \
  >> "$HS_FAKE_DIR/logs"
nodbbin="$work/nodbbin"; mkdir -p "$nodbbin"
cat > "$nodbbin/ddev" <<'NODBDDEV'
#!/usr/bin/env bash
# web answers nothing: the application reads its own dotenv.
exit 0
NODBDDEV
chmod +x "$nodbbin/ddev"
printf 'DB_HOST=ddev-acme-db\n' > "$wt/.env"
out="$(cd "$wt" && PATH="$nodbbin:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
contains "the sweep reads a dotenv the container cannot see" "docker exec ddev-acme-db" "$out"

# ...and a *comment* is not an assignment. A stale note sent the sweep to a
# database the application does not write to, which is round 59's failure
# returning through the other door — and in the worst shape it named another
# client's container.
printf '# was: DB_HOST=ddev-otherclient-db  # disabled, we have our own now\n' > "$wt/.env"
out="$(cd "$wt" && PATH="$nodbbin:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
case "$out" in
  *"docker exec ddev-otherclient-db"*)
    fail "a commented-out DB_HOST is not an assignment" \
      "a stale note redirected the sweep to another database" ;;
  *"ddev exec -s db"*) pass "a commented-out DB_HOST is not an assignment" ;;
  *) fail "a commented-out DB_HOST is not an assignment" "named neither: $out" ;;
esac

# ...and a project does not mistake its own container for a rival.
printf 'DB_HOST=ddev-acme-wt-a-db\n' > "$wt/.env"
out="$(cd "$wt" && DDEV_SITENAME=acme-wt-a PATH="$nodbbin:$fakebin:$PATH" \
  "$cmd" check --slug wt-a 2>&1 || true)"
case "$out" in
  *"docker exec ddev-acme-wt-a-db"*)
    fail "its own container is not a shared one" "took itself for the parent" ;;
  *) pass "its own container is not a shared one" ;;
esac
rm -f "$wt/.env"
writefake

case "$out" in
  *"went to"*"the browser unrewritten"*)
    fail "and it is not described as a response" "$out" ;;
  *) pass "and it is not described as a response" ;;
esac
[ "$rc" = 2 ] && pass "and it refuses" || fail "and it refuses" "exit $rc"

# Both directions at once are two failures, and a developer needs to hear both.
#
# The request branch exited first, so a deployment over the cap in both
# directions was told only about the request — and the subtraction computing the
# response count was dead arithmetic, since it only ran when the request count
# was already zero.
writefake
printf 'time=x level=WARN msg="request body exceeds the size cap, passing through untouched" cap=8388608 content-type=application/json\n' \
  >> "$HS_FAKE_DIR/logs"
printf 'time=x level=WARN msg="JSON body exceeds the size cap, passing through untouched" cap=8388608 content-type=application/json\n' \
  >> "$HS_FAKE_DIR/logs"
out="$(cd "$wt" && PATH="$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" && rc=0 || rc=$?
contains "both cap directions are reported" "1 request(s) reached" "$out"
contains "and the response one too" "1 response(s) went to" "$out"
[ "$rc" = 2 ] && pass "and it still refuses" || fail "and it still refuses" "exit $rc"
writefake

# ...and a page that merely quotes the phrase is not a cap event.
#
# A straggler WARN carries raw page bytes as context. The map extractor forty
# lines above this one defends against exactly that ("a page that merely
# mentions `https://old -> https://new` within 64 bytes of a straggler injected
# a phantom pair, check exited 2 on every start"); the cap count did not, so a
# page quoting this message produced a refusal naming a cap that was never hit.
writefake
printf 'time=x level=WARN msg="straggler" context="...body exceeds the size cap, passing through untouched..."\n' \
  >> "$HS_FAKE_DIR/logs"
out="$(cd "$wt" && PATH="$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" && rc=0 || rc=$?
case "$out" in
  *"larger than the body cap"*)
    fail "a page quoting the message is not a cap event" "refused on page content" ;;
  *) pass "a page quoting the message is not a cap event" ;;
esac
writefake
writefake

# A redirect is followed only while it stays on this deployment.
#
# Round 54 added `curl -L` so a `/` that 302s to `/fi/` is still scanned, and
# `curl -L` has no same-origin restriction — so a `Location:` anywhere was
# followed, with `-k` and `--noproxy '*'`, on every `ddev start`. Under
# production-canonical that fetches the client's live site from the developer's
# laptop, and loopback containment cannot stop it because this curl runs on the
# host and not in `web`. The body then came back attributed to the variant, so
# `check` refused a preview by name over somebody else's links.
: > "$HS_FAKE_DIR/curl-hosts"; rm -f "$HS_FAKE_DIR/curl-redirected"
# A counter of our own, zeroed: the shared one is unset by this point, and
# HS_CURL_CODES is indexed by it — left running it is read past its list and
# yields the last entry forever, so the 302 never happens.
hs55n="$work/curl-n55"; echo 0 > "$hs55n"
out="$(cd "$wt" && HS_CURL_BODY="$third" HS_CURL_CODES="302 200 200" \
  HS_CURL_STATE="$hs55n" \
  HS_CURL_LOCATION="https://elsewhere.example/landing" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
if grep -q "elsewhere.example" "$HS_FAKE_DIR/curl-hosts" 2>/dev/null; then
  fail "a redirect off this deployment is not followed" \
    "fetched $(grep -c elsewhere.example "$HS_FAKE_DIR/curl-hosts") time(s)"
else
  pass "a redirect off this deployment is not followed"
fi

# ...and one that stays on a variant still is, which is what round 54 bought.
: > "$HS_FAKE_DIR/curl-hosts"; rm -f "$HS_FAKE_DIR/curl-redirected"
# A counter of our own, zeroed: the shared one is unset by this point, and
# HS_CURL_CODES is indexed by it — left running it is read past its list and
# yields the last entry forever, so the 302 never happens.
hs55n="$work/curl-n55"; echo 0 > "$hs55n"
out="$(cd "$wt" && HS_CURL_BODY="$third" HS_CURL_CODES="302 200 200" \
  HS_CURL_STATE="$hs55n" \
  HS_CURL_LOCATION="https://wt-a--acme.ddev.site/fi/" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
if grep -q "wt-a--acme.ddev.site/fi/" "$HS_FAKE_DIR/curl-hosts" 2>/dev/null; then
  pass "a redirect within the deployment is still followed"
else
  fail "a redirect within the deployment is still followed" \
    "probed: $(tr '\n' ' ' < "$HS_FAKE_DIR/curl-hosts")"
fi

# An apex canonical under a multi-label suffix is not a relative of every other
# site under it.
#
# Round 54 replaced a two-label registrable domain with the canonical's parent.
# The parent of `example.co.uk` is `co.uk`, so a healthy site was refused over
# ordinary `www.bbc.co.uk` links — and the remedy it printed, add the host as an
# alias, would have made the preview proxy the BBC. The apex is the canonical
# with a leading `www.` removed and only that: `www` is not a public suffix in
# any registry, so `www.x` and `x` are always one site while `x.co.uk` and
# `y.co.uk` never have to be.
mv "$wt/hostshift.yaml" "$work/couk-hs.hold" 2>/dev/null || true
printf 'sites:\n  - canonical: https://example.co.uk\n    variant: https://wt-a--acme.ddev.site\n' \
  > "$wt/hostshift.yaml"
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || true
writefake
couk='<a href="https://www.bbc.co.uk/news/1">a</a>
<a href="https://www.bbc.co.uk/sport">b</a>
<a href="https://www.bbc.co.uk/iplayer">c</a>
<a href="https://www.bbc.co.uk/weather">d</a>
<a href="https://www.bbc.co.uk/sounds">e</a>'
out="$(cd "$wt" && HS_CURL_BODY="$couk" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" && rc=0 || rc=$?
[ "$rc" = 0 ] && pass "an apex canonical does not make its public suffix a relative" \
  || fail "an apex canonical does not make its public suffix a relative" "exit $rc"
case "$out" in
  *"as an alias of the canonical it"*"belongs to"*)
    # Only the note's own wording may say this; the refusal must not have fired.
    case "$out" in
      *"refusing to call this healthy"*)
        fail "and it does not tell them to alias the BBC" "refused" ;;
      *) pass "and it does not tell them to alias the BBC" ;;
    esac ;;
  *) pass "and it does not tell them to alias the BBC" ;;
esac
# ...while a genuine sibling under that same apex is still caught.
sib='<a href="https://shop.example.co.uk/1">a</a>
<a href="https://shop.example.co.uk/2">b</a>
<a href="https://shop.example.co.uk/3">c</a>
<a href="https://shop.example.co.uk/4">d</a>
<a href="https://shop.example.co.uk/5">e</a>'
out="$(cd "$wt" && HS_CURL_BODY="$sib" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" && rc=0 || rc=$?
[ "$rc" = 2 ] && pass "a real sibling under the apex is still refused" \
  || fail "a real sibling under the apex is still refused" "exit $rc"
mv "$work/couk-hs.hold" "$wt/hostshift.yaml" 2>/dev/null || true
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || true
writefake

# --dry-run is the step that is supposed to catch a bad deployment before it is
# one, and it ran none of the static guardrails.
#
# With `base:` omitted — a natural first draft — the variant is derived from the
# canonical, so it is `wt-a--www.acme.example`: a name DDEV registers nowhere.
# On a client domain with wildcard DNS that resolves to *production*, and the
# developer is looking at the live site believing it is a worktree. The guard is
# pure inference over $variants, the project's hostnames, $DDEV_TLD and
# /etc/hosts — nothing to write and nothing to start — but it lived inside
# `check`, so `--dry-run` printed the hostname and no warning, and the refusal
# came only after the project had been deployed and restarted.
mv "$wt/hostshift.yaml" "$work/nobase-hs.hold" 2>/dev/null || true
printf 'sites:\n  - canonical: https://www.acme.example\n' > "$wt/hostshift.yaml"
out="$(cd "$wt" && "$cmd" init --slug wt-a --dry-run 2>&1)" && rc=0 || rc=$?
[ "$rc" = 2 ] && pass "init --dry-run refuses a variant nothing local answers to" \
  || fail "init --dry-run refuses a variant nothing local answers to" "exit $rc"
contains "and it says why that is dangerous" "resolve to *production*" "$out"
contains "and it names the remedy" "base:" "$out"
# ...and nothing was written, which is the other half of the flag's contract.
case "$(cat "$wt/.ddev/.env" 2>/dev/null || true)" in
  *"wt-a--www.acme.example"*)
    fail "and --dry-run still wrote nothing" "the variant reached .ddev/.env" ;;
  *) pass "and --dry-run still wrote nothing" ;;
esac
mv "$work/nobase-hs.hold" "$wt/hostshift.yaml" 2>/dev/null || true
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || true
writefake

# The message names the page that actually carries the finding.
#
# `probe_body` is up to nine documents concatenated — the first variant, the
# page its redirect leads to, and up to eight siblings — and every scan reported
# its finding as "the page served at https://$probe". The developer opened that
# URL, viewed source, found nothing, and had no way from the message to the page
# that is leaking. Here the first page is clean and the redirect target is not.
: > "$HS_FAKE_DIR/curl-hosts"; rm -f "$HS_FAKE_DIR/curl-redirected"
hs55n="$work/curl-n55b"; echo 0 > "$hs55n"
sibleak='=== a line that used to split the record ===
<a href="https://media.acme.example/1">a</a>
<a href="https://media.acme.example/2">b</a>
<a href="https://media.acme.example/3">c</a>
<a href="https://media.acme.example/4">d</a>
<a href="https://media.acme.example/5">e</a>'
out="$(cd "$wt" && HS_CURL_BODY='<p>clean</p>' HS_CURL_BODY2="$sibleak" \
  HS_CURL_CODES="302 200 200" HS_CURL_STATE="$hs55n" \
  HS_CURL_LOCATION="https://wt-a--acme.ddev.site/fi/" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" && rc=0 || rc=$?
[ "$rc" = 2 ] && pass "a leak on the page behind a redirect is found" \
  || fail "a leak on the page behind a redirect is found" "exit $rc"
contains "and the message names that page, not the first" "after its redirect" "$out"
# The body above opens with a line starting `=== `, which is what the record
# marker used to be — a changelog, a diff or a code sample splits the record,
# and because the URL then stays set it mis-attributes every later message for
# that page and not just one.
contains "and a page containing the marker still attributes" "after its redirect" "$out"

# ...and the escaped spellings, which is how WordPress writes URLs by default.
#
# The first version of this scan was a `//host` grep in shell — one spelling of
# an origin out of the dozen a browser resolves. `wp_json_encode` writes every
# URL in a block attribute or an inline script data island as `https:\/\/host`,
# and that was invisible: ten of them served, exit 0, `ddev restart` reporting
# success. It asks the binary now, which has the decoders.
for spelling in json pct refs; do
  case "$spelling" in
    json) esc='{"u":"https:\\/\\/acme.example\\/p"}' ;;
    pct)  esc='<a href="https%3A%2F%2Facme.example%2Fp">t</a>' ;;
    refs) esc='<a href="https:&#x2F;&#x2F;acme.example/p">t</a>' ;;
  esac
  body=""
  for i in 1 2 3 4 5; do body="$body
$esc"; done
  out="$(cd "$wt" && HS_CURL_BODY="$body" \
    PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" && rc=0 || rc=$?
  [ "$rc" = 2 ] && pass "a $spelling-escaped off-map origin is refused" \
    || fail "a $spelling-escaped off-map origin is refused" "exit $rc"
done

# ...and the healthy page says nothing at all. Every origin on it is the variant
# — that is what a working deployment looks like — so subtracting what this
# deployment legitimately names is what keeps this from firing on every start.
healthy='<link rel="canonical" href="https://wt-a--acme.ddev.site/">
<link rel="alternate" href="https://wt-a--acme.ddev.site/feed/">
<link rel="https://api.w.org/" href="https://wt-a--acme.ddev.site/wp-json/">
<link rel="shortlink" href="https://wt-a--acme.ddev.site/?p=1">
<a href="https://wt-a--acme.ddev.site/x">t</a>'
out="$(cd "$wt" && HS_CURL_BODY="$healthy" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" && rc=0 || rc=$?
[ "$rc" = 0 ] && pass "a page of nothing but variant origins is silent" \
  || fail "a page of nothing but variant origins is silent" "exit $rc"
case "$out" in
  *"links to wt-a--acme.ddev.site"*)
    fail "and the variant is not reported as a stranger" "$out" ;;
  *) pass "and the variant is not reported as a stranger" ;;
esac

# ...and a ccSLD sibling is not "the same site".
#
# The registrable domain was approximated as the last two labels, which for a
# canonical of `www.acme.co.uk` is `co.uk` — so six links to `www.bbc.co.uk` in
# an "as featured in" block were refused as the same site, on every `ddev start`,
# with no way to silence it. Following the printed remedy (add it as an alias)
# then proxied the BBC's links to the local container and turned the refusal
# green. The parent domain needs no public-suffix list: `www.bbc.co.uk` is not
# under `www.acme.co.uk`'s parent `acme.co.uk`.
cp "$wt/.ddev/.env" "$work/cc-env.hold" 2>/dev/null || true
mv "$wt/hostshift.yaml" "$work/cc-hs.hold" 2>/dev/null || true
printf 'sites:\n  - canonical: https://www.acme.co.uk\n    variant: https://wt-a--acme.ddev.site\n' \
  > "$wt/hostshift.yaml"
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || true
env_args="$(sed -n 's/^HOSTSHIFT_ARGS=//p' "$wt/.ddev/.env")"
env_variants="$(sed -n 's/^HOSTSHIFT_VARIANTS=//p' "$wt/.ddev/.env")"
env_web="$(sed -n 's/^HOSTSHIFT_WEB_HOSTS=//p' "$wt/.ddev/.env")"
writefake
ccbody='<a href="https://www.bbc.co.uk/news/1">a</a>
<a href="https://www.bbc.co.uk/news/2">b</a>
<a href="https://www.bbc.co.uk/news/3">c</a>
<a href="https://www.bbc.co.uk/news/4">d</a>
<a href="https://www.bbc.co.uk/news/5">e</a>'
out="$(cd "$wt" && HS_CURL_BODY="$ccbody" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" && rc=0 || rc=$?
[ "$rc" = 0 ] && pass "a ccSLD sibling is not refused as the same site" \
  || fail "a ccSLD sibling is not refused as the same site" "exit $rc"

# ...while a genuine subdomain of the canonical's parent still is.
ccown='<a href="https://shop.acme.co.uk/1">a</a>
<a href="https://shop.acme.co.uk/2">b</a>
<a href="https://shop.acme.co.uk/3">c</a>
<a href="https://shop.acme.co.uk/4">d</a>
<a href="https://shop.acme.co.uk/5">e</a>'
out="$(cd "$wt" && HS_CURL_BODY="$ccown" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" && rc=0 || rc=$?
[ "$rc" = 2 ] && pass "and a subdomain of the canonical's parent still is" \
  || fail "and a subdomain of the canonical's parent still is" "exit $rc"
rm -f "$wt/hostshift.yaml"
mv "$work/cc-hs.hold" "$wt/hostshift.yaml" 2>/dev/null || true
cp "$work/cc-env.hold" "$wt/.ddev/.env" 2>/dev/null || true
env_args="$(sed -n 's/^HOSTSHIFT_ARGS=//p' "$wt/.ddev/.env")"
env_variants="$(sed -n 's/^HOSTSHIFT_VARIANTS=//p' "$wt/.ddev/.env")"
env_web="$(sed -n 's/^HOSTSHIFT_WEB_HOSTS=//p' "$wt/.ddev/.env")"
writefake
rm -f "$wt/hostshift.yaml"
mv "$work/apex-hs.hold" "$wt/hostshift.yaml" 2>/dev/null || true
cp "$work/apex-env.hold" "$wt/.ddev/.env" 2>/dev/null || true
env_args="$(sed -n 's/^HOSTSHIFT_ARGS=//p' "$wt/.ddev/.env")"
env_variants="$(sed -n 's/^HOSTSHIFT_VARIANTS=//p' "$wt/.ddev/.env")"
env_web="$(sed -n 's/^HOSTSHIFT_WEB_HOSTS=//p' "$wt/.ddev/.env")"
writefake

# ...and every variant is probed, not only the first. `probe` is `cut -f1`, and
# probe_body was the sole evidence for all four scans — so on a multisite no
# state of any site but the first could fail check: site 1 clean and site 2
# carrying seventeen production origins exited 0.
: > "$HS_FAKE_DIR/curl-hosts"
out="$(cd "$wt" && HS_CURL_BODY="$healthy" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
n_probed="$(sort -u "$HS_FAKE_DIR/curl-hosts" 2>/dev/null | wc -l | tr -d ' ')"
[ "${n_probed:-0}" -ge 2 ] && pass "check probes every variant, not just the first" \
  || fail "check probes every variant, not just the first" "probed $n_probed host(s)"

# ...and one link is not that. A developer's note pointing at the client's site
# is an ordinary content link, and refusing on it would be the warning-on-every-
# start failure the stranger-hostname loop beside this one already learned.
out="$(cd "$wt" && HS_CURL_BODY='<a href="https://www.acme.example/x">t</a>' \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" && rc=0 || rc=$?
case "$out" in
  *"refusing to call this healthy — the page served at"*)
    fail "one incidental link to a declared hostname is not a refusal" "$out" ;;
  *) pass "one incidental link to a declared hostname is not a refusal" ;;
esac
rm -f "$main/hostshift.yaml"
mv "$work/wt-hs.hold" "$wt/hostshift.yaml" 2>/dev/null || true
cp "$work/wt-env.hold" "$wt/.ddev/.env" 2>/dev/null || true
env_args="$(sed -n 's/^HOSTSHIFT_ARGS=//p' "$wt/.ddev/.env")"
env_variants="$(sed -n 's/^HOSTSHIFT_VARIANTS=//p' "$wt/.ddev/.env")"
env_web="$(sed -n 's/^HOSTSHIFT_WEB_HOSTS=//p' "$wt/.ddev/.env")"
writefake

printf 'WP_HOME="https://acme-wt-a.ddev.site"\nWP_SITEURL="https://acme-wt-a.ddev.site/wp"\n' \
  > "$wt/.env"
out="$(cd "$wt" && DDEV_HOSTNAME=acme-wt-a.ddev.site \
  HS_CURL_BODY="$pinned" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
contains "a Bedrock project is told where DDEV actually pins it" \
  "the project-root .env, rewritten on" "$out"
contains "and how to stop it being rewritten" "disable_settings_management: true" "$out"

case "$out" in
  *"wp-config-ddev.php"*) fail "and is not told about a file it does not have" "$out" ;;
  *) pass "and is not told about a file it does not have" ;;
esac
rm -f "$wt/.env"
# ...and a .env that defines no WP_HOME is not a Bedrock pin. The probe tests
# both halves — the file existing and the variable being in it — and only the
# first half had a test, so a project with an unrelated .env got advice about a
# pin it does not have.
printf 'APP_ENV=development\nDB_NAME=db\n' > "$wt/.env"
out="$(cd "$wt" && DDEV_HOSTNAME=acme-wt-a.ddev.site \
  HS_CURL_BODY="$pinned" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
contains "a .env without WP_HOME is not a Bedrock pin" "wp-config-ddev.php" "$out"
rm -f "$wt/.env"

# But one incidental content link is not that, and warning on it meant warning
# on every `ddev start` of a healthy site. A developer's note pointing at the
# worktree's own mailpit is exactly one such link, and mailpit is deliberately
# not proxied.
out="$(cd "$wt" && DDEV_HOSTNAME=acme-wt-a.ddev.site \
  HS_CURL_BODY='<p>mail goes to <a href="https://acme-wt-a.ddev.site:8026/">mailpit</a></p>' \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
case "$out" in
  *"which is neither a canonical hostname nor a"*)
    fail "one incidental link is not a pinned WP_HOME" "$out" ;;
  *) pass "one incidental link is not a pinned WP_HOME" ;;
esac

# And a hostname that is only a prefix of a longer one is a different host.
long='<img src="https://acme-wt-a.ddev.site.cdn.example.net/1.png">
<img src="https://acme-wt-a.ddev.site.cdn.example.net/2.png">
<img src="https://acme-wt-a.ddev.site.cdn.example.net/3.png">
<img src="https://acme-wt-a.ddev.site.cdn.example.net/4.png">
<img src="https://acme-wt-a.ddev.site.cdn.example.net/5.png">
<img src="https://acme-wt-a.ddev.site.cdn.example.net/6.png">'
out="$(cd "$wt" && DDEV_HOSTNAME=acme-wt-a.ddev.site HS_CURL_BODY="$long" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
case "$out" in
  *"which is neither a canonical hostname nor a"*)
    fail "a longer hostname that merely starts the same is not a match" "$out" ;;
  *) pass "a longer hostname that merely starts the same is not a match" ;;
esac
# No subcommand prints usage rather than running init.
#
# `init` writes .ddev/.env and restarts every container, so a developer reaching
# for the usage text took their project down instead.
cp "$wt/.ddev/.env" "$work/env.before"
out="$(cd "$wt" && "$cmd" 2>&1 || true)"
contains "no subcommand prints usage" "usage: ddev hostshift" "$out"
# And it changed nothing — the usage text mentions restarting, so asserting on
# the words would pass whatever the command did.
if cmp -s "$work/env.before" "$wt/.ddev/.env"; then
  pass "no subcommand leaves .ddev/.env alone"
else
  fail "no subcommand leaves .ddev/.env alone" "the file was rewritten"
fi

# A body past the pipe buffer, which is every WordPress page.
#
# The check was `printf '%s' "$body" | grep -qF …` under `set -o pipefail`.
# grep -q exits on its first match, printf takes SIGPIPE while still writing,
# and the pipeline returns 141 — so the warning could not fire on any body over
# 64 KiB, which is to say on any deployment that had the problem. The fixture
# above is 41 bytes, three orders of magnitude under the threshold, which is why
# this passed while the real thing was inert. The threshold was measured exactly:
# 65536 bytes fires, 69374 does not.
# Newlines matter, and getting this wrong is how the first attempt at this test
# proved nothing: grep matches line by line, so a single enormous line forces it
# to read to the end and the pipeline never fails. A real page is many lines,
# grep exits on the first match, and everything still queued behind it dies.
big="$(cd "$wt" && awk 'BEGIN{for(i=0;i<4000;i++)print "<p>filler filler filler</p>"}')"
out="$(cd "$wt" && DDEV_HOSTNAME=acme-wt-a.ddev.site \
  HS_CURL_BODY="$pinned
$big" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
contains "a page larger than the pipe buffer is still inspected" \
  "which is neither a canonical hostname nor a" "$out"

# Every hostname DDEV registers, not just the first of the comma-separated list.
out="$(cd "$wt" && DDEV_HOSTNAME=acme-wt-a.ddev.site,extra-wt-a.ddev.site \
  HS_CURL_BODY="${pinned//acme-wt-a/extra-wt-a}" \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
contains "a hostname past the first in DDEV_HOSTNAME is inspected too" \
  "links to extra-wt-a.ddev.site" "$out"

# And a page that names only the variant says nothing.
out="$(cd "$wt" && DDEV_HOSTNAME=acme-wt-a.ddev.site \
  HS_CURL_BODY='<a href="https://wt-a--acme.ddev.site/x">t</a>' \
  PATH="$fakedb:$fakecurl:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
case "$out" in
  *"neither a canonical origin nor a"*)
    fail "a page naming only the variant is not called out" "$out" ;;
  *) pass "a page naming only the variant is not called out" ;;
esac

# The worktree's own hostshift.yaml, left by the running-map test above, would
# make the parent-declares gate correctly not engage at all — the whole gate is
# for a worktree whose branch predates the parent's file.
rm -f "$wt/hostshift.yaml"
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || true
# Re-capture before writefake: the map just changed, and a fake container still
# advertising the old one makes check refuse on a stale running map instead.
env_args="$(sed -n 's/^HOSTSHIFT_ARGS=//p' "$wt/.ddev/.env")"
env_variants="$(sed -n 's/^HOSTSHIFT_VARIANTS=//p' "$wt/.ddev/.env")"
env_web="$(sed -n 's/^HOSTSHIFT_WEB_HOSTS=//p' "$wt/.ddev/.env")"
writefake

# The database has the last word, and the verdict is a warning, not a refusal.
#
# The refusal exited *before* the database gate ever ran, so `check` asserted
# "the map does not name the hostnames the database holds" without having looked
# at the database — on a deployment where the row said otherwise: three
# hostnames serving 200, `hostshift diff` GREEN, and `wp_options.home` holding a
# hostname the map names. Seventh consecutive round this gate has been wrong in
# one direction or the other, and that time the printed remedy cost a working
# deployment.
#
# It now keeps the signal and gives up the authority, for the same reason the
# database gate did: a wrong answer costs a line of noise rather than every boot
# of a healthy site. The probe is the hard measurement and it still refuses.
printf 'version: 1\nsites:\n  - {name: main, canonical: https://www.acme.example, base: https://acme.ddev.site}\n' \
  > "$main/hostshift.yaml"
# `rc=$?` after a bare assignment never runs under `set -e` — the assignment's
# own status ends the script first.
rc=0
out="$(cd "$wt" && HS_FAKE_HOME="https://acme.ddev.site" \
  PATH="$fakedb:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1)" || rc=$?
check "a parent-declares mismatch no longer fails the start" "0" "$rc"
# The row qualifies the warning; it does not cancel it.
#
# This test used to assert suppression, and that expectation was the defect: the
# row is about *one* origin — whatever `wp_options.home` holds — while `unmapped`
# is a set difference over everything the parent declares. With `copy-db` having
# left `home` on a DDEV hostname, the row was satisfied, the subset test's
# finding was discarded, and a declared production canonical went to the browser
# unrewritten with check exit 0 — re-opening the leak the subset test was added
# in the same commit to close.
contains "a database row that names the map qualifies the warning" \
  "The database's own home is in the map" "$out"
contains "but the unmapped canonical is still named" \
  "Declared there and not in this map" "$out"
# But with no database answer, the warning still gets printed.
rc2=0
out="$(cd "$wt" && HS_FAKE_HOME="" PATH="$fakedb:$fakebin:$PATH" \
  "$cmd" check --slug wt-a 2>&1)" || rc2=$?
contains "with no database answer the warning still stands" \
  "declares the hostnames its database" "$out"
# And it is a warning: this is the half that must never become a refusal again.
check "and it is still only a warning" "0" "$rc2"
rm -f "$main/hostshift.yaml"

# `check` on a project with no branch and no .ddev/.env must say something.
#
# `sed` exits non-zero when the file is absent, and under `set -e` that ended the
# script with no output at all — exit 1, nothing on stdout or stderr, from the
# block whose own comment says a detached HEAD must not stop `check`. On a branch
# the same state prints "nothing deployed yet" and exits 2, so the failure was
# invisible except in the states the recovery block exists for.
mkdir -p "$work/bare/.ddev"
printf 'name: bare\n' > "$work/bare/.ddev/config.yaml"
rc=0
out="$(cd "$work/bare" && "$cmd" check 2>&1)" || rc=$?
case "$out" in
  "") fail "check says something when there is no branch and no .ddev/.env" \
        "exit $rc with no output" ;;
  *) pass "check says something when there is no branch and no .ddev/.env" ;;
esac

# A rival whose directory no longer exists under that name.
#
# `mv wt-a wt-a-renamed && ddev start` leaves the old project's containers up
# with the same VIRTUAL_HOST, and every scan above enumerates *directories* —
# so the rival's registered approot is gone and skipped, and its `.ddev/.env`
# travelled with the directory and reads as self. Measured: DDEV itself warned
# "Router count mismatch", traefik had four routers on one hostname, and check
# printed "hostshift is serving" and exited 0. A running container carries its
# own identity, so asking Docker needs no directory to exist.
printf 'ddev-acme-wt-renamed-hostshift\n' > "$HS_FAKE_DIR/ps"
out="$(cd "$wt" && PATH="$fakedb:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
contains "a renamed project's containers still claiming the variant are caught" \
  "no longer exists under that name" "$out"
# And the project must not find *itself*, or every healthy check refuses.
printf 'ddev-acme-wt-a-hostshift\n' > "$HS_FAKE_DIR/ps"
out="$(cd "$wt" && PATH="$fakedb:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
case "$out" in
  *"no longer exists under that name"*)
    fail "a project does not mistake its own container for a rival" "$out" ;;
  *) pass "a project does not mistake its own container for a rival" ;;
esac
# And with DDEV_SITENAME set, which is how the command is actually invoked.
#
# The first version of the self-check re-derived the name from
# .ddev/config.yaml. This suite passed, because with no DDEV_SITENAME it
# exercised the fallback — and the integration suite then found the project
# reporting *itself* as a rival and refusing every healthy worktree. DDEV merges
# config.*.yaml and a worktree names itself in config.*.local.yaml; two hundred
# lines down, this same file already carries a comment about making exactly that
# mistake. A test that only drives the fallback is not testing the real path.
printf 'ddev-acme-wt-real-hostshift\n' > "$HS_FAKE_DIR/ps"
out="$(cd "$wt" && DDEV_SITENAME=acme-wt-real PATH="$fakedb:$fakebin:$PATH" \
  "$cmd" check --slug wt-a 2>&1 || true)"
case "$out" in
  *"no longer exists under that name"*)
    fail "self-exclusion uses the name DDEV itself gives the project" "$out" ;;
  *) pass "self-exclusion uses the name DDEV itself gives the project" ;;
esac
# A container whose *name* no longer matches, because `name:` was edited and
# the project has not restarted — but whose approot is this very directory.
#
# The scan then reported the project's own live proxy as a rival whose directory
# no longer exists, and the remedy it printed would stop the proxy serving the
# page. DDEV refuses to complete such a rename, so the state is transient, but
# the advice was wrong while it lasted.
printf 'ddev-acme-wt-renamed-hostshift\n' > "$HS_FAKE_DIR/ps"
printf '%s\n' "$(cd "$wt" && pwd -P)" > "$HS_FAKE_DIR/approot"
out="$(cd "$wt" && PATH="$fakedb:$fakebin:$PATH" "$cmd" check --slug wt-a 2>&1 || true)"
case "$out" in
  *"no longer exists under that name"*)
    fail "a container sharing this project's approot is not a rival" "$out" ;;
  *) pass "a container sharing this project's approot is not a rival" ;;
esac
rm -f "$HS_FAKE_DIR/approot"
rm -f "$HS_FAKE_DIR/ps"
unset HS_FAKE_DIR

rm -f "$wt/hostshift.yaml"
unset HS_FAKE_DIR
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || true

# Every command this prints must be one DDEV actually accepts. `ddev start -p X`
# is not — it fails with "unknown shorthand flag".
if grep -n 'ddev start -p' "$repo/ddev/commands/host/hostshift"; then
  fail "no message suggests a flag ddev does not have" "ddev start -p"
else
  pass "no message suggests a flag ddev does not have"
fi

# The host command must carry #ddev-generated or `ddev add-on get` refuses to
# overwrite it — so an upgrade installs a new compose file beside the old
# command, and the two halves disagree about the variable names they pass
# between them. That is a fleet-wide 421 with init, restart and check all
# reporting success.
if head -5 "$repo/ddev/commands/host/hostshift" | grep -q '#ddev-generated'; then
  pass "the host command can be upgraded by ddev add-on get"
else
  fail "the host command can be upgraded by ddev add-on get" "no #ddev-generated marker"
fi

# And the compose file must still read what a .ddev/.env written before the
# rename says, because that file outlives the upgrade that renames it.
for v in HOSTSHIFT_ARGS HOSTSHIFT_MAP_ARGS HOSTSHIFT_SLUG; do
  grep -q "$v" "$repo/ddev/docker-compose.hostshift.yaml" \
    || fail "compose still reads $v" "not referenced"
done
pass "compose reads the pre-rename spellings too"

# Editing hostshift.yaml is caught by comparing the file against the running
# container's start time, in test/integration-proxy-ddev.sh where there is a
# container to ask. A checksum recorded at init time answered the wrong question:
# a plain `ddev restart` picks the file up correctly, and check called that stale.

# DDEV prints a four-line "Custom configuration detected" block naming every
# unmarked custom file, on every start. .ddev/.env is ours, so it carries the
# marker DDEV's own message tells you to add — and init must keep exactly one of
# them however many times it runs.
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || fail "init exited non-zero" ""
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || fail "init exited non-zero" ""
check "one silence marker in .ddev/.env, however often init runs" "1" \
  "$(grep -c '^#ddev-silent-no-warn' "$wt/.ddev/.env")"
contains "and the loopback file carries one too" "#ddev-silent-no-warn" \
  "$(head -1 "$repo/ddev/docker-compose.hostshift-loopback.yaml")"

# A temp file left in .ddev/ is read by DDEV as a per-service env file, and
# .ddev/.env is where DDEV documents putting credentials. One was found on this
# machine — a verbatim copy of a project's .env.
if ls "$wt"/.ddev/.env.* >/dev/null 2>&1; then
  fail "init leaves no copy of .ddev/.env behind" "$(ls "$wt"/.ddev/.env.*)"
else
  pass "init leaves no copy of .ddev/.env behind"
fi

echo "== wp-cli"

# A wp-cli.yml with no trailing newline glued url: onto its last line.
printf 'path: web/wp\nrequire: wp-cli.php' > "$wt/wp-cli.yml"
out="$(cd "$wt" && "$cmd" wp-cli --slug wt-a 2>/dev/null || true)"
contains "a file with no trailing newline stays valid YAML" \
  "require: wp-cli.php" "$out"
if printf '%s' "$out" | grep -q 'wp-cli.phpurl:'; then
  fail "a file with no trailing newline stays valid YAML" "$out"
fi

# An existing root url: was emitted twice, which yaml.v3 rejects as a duplicate.
printf 'path: web/wp\nurl: https://old.example\n' > "$wt/wp-cli.yml"
out="$(cd "$wt" && "$cmd" wp-cli --slug wt-a 2>/dev/null || true)"
check "an existing root url: is replaced, not duplicated" "1" \
  "$(printf '%s\n' "$out" | grep -c '^url:')"
contains "and the replacement is the canonical origin" "url: https://acme.ddev.site" "$out"
rm "$wt/wp-cli.yml"

echo "== arguments"

if (cd "$wt" && "$cmd" env --slugg oops >/dev/null 2>&1); then
  fail "an unknown argument is refused" "exited 0"
else
  pass "an unknown argument is refused"
fi
contains "--slug with no value says so" "--slug needs a value" \
  "$(cd "$wt" && "$cmd" env --slug 2>&1 || true)"
contains "an unknown subcommand prints usage" "usage: ddev hostshift" \
  "$(cd "$wt" && "$cmd" nonsense 2>&1 || true)"

# A slug reaches HOSTSHIFT_VARIANTS and the proxy's command line, and both go
# through compose's shell-word splitting: a `;` truncated the map silently, a `$`
# interpolated to nothing, a `"` killed the start with an error naming neither
# hostshift nor the file, and a `,` corrupted the comma-delimited variant list.
bad_ok=1
for bad in 'a;b' 'a$b' 'a,b' 'a b' '-lead' 'trail-'; do
  if (cd "$wt" && "$cmd" env --slug "$bad" >/dev/null 2>&1); then
    fail "a slug that is not a hostname label is refused" "accepted $bad"
    bad_ok=""
  fi
done
[ -n "$bad_ok" ] && pass "a slug that is not a hostname label is refused"
contains "and says why" "silently truncates or corrupts the map" \
  "$(cd "$wt" && "$cmd" env --slug 'a;b' 2>&1 || true)"

# `ddev` may be run from anywhere inside the project.
mkdir -p "$wt/web/app"
out="$(cd "$wt/web/app" && DDEV_APPROOT="$wt" "$cmd" env --slug wt-a 2>/dev/null || true)"
contains "it works from a subdirectory" "HOSTSHIFT_VARIANTS=wt-a--acme.ddev.site" "$out"

echo

echo "== copy-db --dry-run writes nothing"

# copy-db is the one destructive subcommand, and it exits before the file's only
# dry_run test — which sits at the very end, after the init path. So
# `copy-db --dry-run` replaced the database it was asked to describe, and the
# refusal it prints names --force, making `--dry-run --force` the natural second
# command and the one that destroys.
#
# A fake `ddev` records every exec, so "wrote nothing" is asserted on what was
# run rather than on what was printed.
cdb="$work/copydb"; mkdir -p "$cdb/bin"
export HS_FAKE_DIR="$cdb"
cat > "$cdb/bin/ddev" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${HS_FAKE_DIR}/ddev-calls"
case "$*" in
  # The shared-database probe: say the worktree has its own db container.
  *printenv*)                     echo "DB_HOST=ddev-$(basename "$PWD")-db" ;;
  *information_schema.tables*)    cat "${HS_FAKE_DIR}/tablecount" 2>/dev/null || echo 0 ;;
  *mysqldump*)                    echo copied > "${HS_FAKE_DIR}/DESTROYED" ;;
esac
exit 0
FAKE
chmod +x "$cdb/bin/ddev"
: > "$HS_FAKE_DIR/ddev-calls"
printf '7\n' > "$HS_FAKE_DIR/tablecount"

copydb() {
  rm -f "$HS_FAKE_DIR/DESTROYED"
  (cd "$wt" && PATH="$cdb/bin:$PATH" "$cmd" copy-db "$@" 2>&1 || true)
}

out="$(copydb --dry-run)"
if [ -f "$HS_FAKE_DIR/DESTROYED" ]; then
  fail "--dry-run does not copy the database" "mysqldump ran"
else
  pass "--dry-run does not copy the database"
fi
contains "--dry-run says it wrote nothing" "nothing written" "$out"
contains "--dry-run says what it would replace" "7 table(s)" "$out"

# The pair a developer reaches for after reading the refusal.
out="$(copydb --dry-run --force)"
if [ -f "$HS_FAKE_DIR/DESTROYED" ]; then
  fail "--dry-run --force does not copy either" "mysqldump ran"
else
  pass "--dry-run --force does not copy either"
fi

# And the guard has not disarmed the command itself.
out="$(copydb --force)"
if [ -f "$HS_FAKE_DIR/DESTROYED" ]; then
  pass "--force still copies"
else
  fail "--force still copies" "mysqldump did not run: $out"
fi

# A database belonging to a *third* project — neither this one nor the source.
#
# The guard used to ask "does web read `ddev-$from-db`?", which is the wrong
# question: the harm has nothing to do with which project the dump comes from.
# Two worktrees of one parent produce this the moment one of them has its own
# copy and the other points at it — twelve tables written into a container
# nothing reads, exit 0, and the success message, which is verbatim the failure
# the refusal describes.
cat > "$cdb/bin/ddev" <<'FAKE3'
#!/usr/bin/env bash
case "$*" in
  *printenv*)                     echo "DB_HOST=ddev-some-other-worktree-db" ;;
  *information_schema.tables*)    echo 0 ;;
  *mysqldump*)                    echo copied > "${HS_FAKE_DIR}/DESTROYED" ;;
esac
exit 0
FAKE3
chmod +x "$cdb/bin/ddev"
rm -f "$HS_FAKE_DIR/DESTROYED"
out="$(copydb || true)"
contains "copy-db refuses a database belonging to neither project" \
  "configured to *use*" "$out"

if [ -f "$HS_FAKE_DIR/DESTROYED" ]; then
  fail "and writes nothing" "mysqldump ran"
else
  pass "and writes nothing"
fi

# ...and a project that pins `name:` in a config override recognises its *own*
# database. `self` was derived from the directory basename, which is wrong on
# exactly the projects that pin a name — 62 of 66 in this fleet — so the
# project's own db looked foreign and a correct copy was refused, with --force
# unable to bypass it. The same file already derives this identity properly two
# hundred lines down, for the same reason, and this now uses that.
#
# The directory and the pinned name have to differ, or the test cannot tell the
# two derivations apart.
pin="$work/pinproj"; newproject "$pin"
git -C "$pin" worktree add -q "$work/pindir" -b wt-p
pinwt="$work/pindir"
printf 'name: pinnedname\n' > "$pinwt/.ddev/config.worktree.local.yaml"
(cd "$pinwt" && "$cmd" init --slug wt-p >/dev/null 2>&1) || fail "init for the pinned fixture" ""
cat > "$cdb/bin/ddev" <<'FAKE4'
#!/usr/bin/env bash
case "$*" in
  *printenv*)                     echo "DB_HOST=ddev-pinnedname-db" ;;
  *information_schema.tables*)    echo 0 ;;
  *mysqldump*)                    echo copied > "${HS_FAKE_DIR}/DESTROYED" ;;
esac
exit 0
FAKE4
chmod +x "$cdb/bin/ddev"
rm -f "$HS_FAKE_DIR/DESTROYED"
out="$(cd "$pinwt" && PATH="$cdb/bin:$PATH" "$cmd" copy-db 2>&1 || true)"
case "$out" in
  *"configured to *use*"*) fail "a pinned name recognises its own database" "$out" ;;
  *) pass "a pinned name recognises its own database" ;;
esac

# `wp-cli` redirected onto its own input.
#
# The documented form writes wp-cli.local.yml. One word away is `> wp-cli.yml`,
# which the shell truncates before the command reads it — so the committed
# config was replaced by a header and a `url:`, losing `path:` and every alias,
# with exit 0.
wc="$work/wpcliself"; newproject "$wc"
git -C "$wc" worktree add -q "$work/wpcliself-wt" -b wt-w
wcw="$work/wpcliself-wt"
printf 'path: web\n@prod:\n  ssh: deploy@prod.example.com/var/www/site\n' > "$wcw/wp-cli.yml"
git -C "$wcw" add wp-cli.yml
git -C "$wcw" -c user.email=t@t -c user.name=t -c commit.gpgsign=false commit -qm wpcli
( cd "$wcw" && "$cmd" wp-cli --slug wt-w > wp-cli.yml ) 2>/dev/null && rc=0 || rc=$?
[ "$rc" = 2 ] && pass "wp-cli refuses to write over its own input" \
  || fail "wp-cli refuses to write over its own input" "exit $rc"
err="$( ( cd "$wcw" && "$cmd" wp-cli --slug wt-w > wp-cli.yml ) 2>&1 >/dev/null || true )"
contains "and says the shell already truncated it" "already" "$err"
contains "and points at the way back" "git checkout -- wp-cli.yml" "$err"
contains "and names the file it belongs in" "wp-cli.local.yml" "$err"
# An empty wp-cli.yml is not evidence of anything, and must not refuse a
# redirect to somewhere else. The guard's signal is whether writing to stdout
# moves wp-cli.yml — not anything about the file's history.
ut="$work/wpcliuntracked"; newproject "$ut"
git -C "$ut" worktree add -q "$work/wpcliuntracked-wt" -b wt-u
utw="$work/wpcliuntracked-wt"
: > "$utw/wp-cli.yml"
( cd "$utw" && "$cmd" wp-cli --slug wt-u > wp-cli.local.yml ) 2>/dev/null && rc=0 || rc=$?
[ "$rc" = 0 ] && pass "an untracked empty wp-cli.yml does not block a real redirect" \
  || fail "an untracked empty wp-cli.yml does not block a real redirect" "exit $rc"

# ...and a wp-cli.yml that is *committed and empty on disk* must not refuse it
# either. Asking git instead of asking stdout refused the documented command
# here, on a tree where nothing was being overwritten.
git -C "$utw" add wp-cli.yml
git -C "$utw" -c user.email=t@t -c user.name=t -c commit.gpgsign=false commit -qm empty
printf 'path: web\n' > "$utw/wp-cli.yml"
git -C "$utw" add wp-cli.yml
git -C "$utw" -c user.email=t@t -c user.name=t -c commit.gpgsign=false commit -qm filled
: > "$utw/wp-cli.yml"
( cd "$utw" && "$cmd" wp-cli --slug wt-u > wp-cli.local.yml ) 2>/dev/null && rc=0 || rc=$?
[ "$rc" = 0 ] && pass "a committed-but-empty wp-cli.yml does not block one either" \
  || fail "a committed-but-empty wp-cli.yml does not block one either" "exit $rc"

# ...and without git at all, the real accident is still caught. The question is
# about this process's stdout, which git cannot see.
git -C "$wcw" checkout -- wp-cli.yml
out="$( cd "$wcw" && PATH="/usr/bin:/bin" "$cmd" wp-cli --slug wt-w > wp-cli.yml 2>&1 || true )"
err="$( cd "$wcw" && PATH="/usr/bin:/bin" "$cmd" wp-cli --slug wt-w 2>&1 >/dev/null || true )"
case "$err" in
  *"writing wp-cli.yml from itself"*) fail "no false positive without git" "$err" ;;
  *) pass "no false positive without git" ;;
esac

# The documented form still works — on the file as committed.
git -C "$wcw" checkout -- wp-cli.yml
( cd "$wcw" && "$cmd" wp-cli --slug wt-w > wp-cli.local.yml ) 2>/dev/null
contains "and the documented form still emits path:" "path: web" "$(cat "$wcw/wp-cli.local.yml")"

# init answers with the restart's status, not with the negation's.
#
# `if ! ddev restart; then rc=$?` captures the status of the *negation*, which
# is always 0 — so a failed restart exited 0 with `.ddev/.env` written and the
# containers still on the old map. That is exactly what the comment block above
# that call exists to prevent: "an agent that checks $? was told everything was
# fine". Second time this class has bitten, so it gets a test.
ex="$work/exitcode"; newproject "$ex"
git -C "$ex" worktree add -q "$work/exitcode-wt" -b wt-x
exw="$work/exitcode-wt"
mkdir -p "$work/failddev"
cat > "$work/failddev/ddev" <<'FAILDDEV'
#!/usr/bin/env bash
case "$1" in
  restart) echo "Failed to restart: router did not come up" >&2; exit 7 ;;
esac
exit 0
FAILDDEV
chmod +x "$work/failddev/ddev"
(cd "$exw" && DDEV_APPROOT="$exw" PATH="$work/failddev:$PATH" \
  "$cmd" init --slug wt-x >/dev/null 2>&1) && rc=0 || rc=$?
[ "$rc" = 7 ] && pass "init exits with the failed restart's status" \
  || fail "init exits with the failed restart's status" "exit $rc, want 7"
out="$(cd "$exw" && DDEV_APPROOT="$exw" PATH="$work/failddev:$PATH" \
  "$cmd" init --slug wt-x 2>&1 || true)"
contains "and says the env is written and the restart is what failed" \
  "the restart is what failed" "$out"
rm -rf "$work/failddev"

echo "== a proxy flag in HOSTSHIFT_ARGS survives check and init"

# --max-body, --strict-origins, --compress and --no-sweep are real proxy flags
# with no hostshift.yaml key, so .ddev/.env is the only place to set them. init
# wrote the line wholesale and check compared it for equality, so adding one put
# the project permanently "out of date" — and following the advice that printed,
# `ddev hostshift init`, deleted the flag silently. Both directions at once,
# with no message either way.
d="$work/keepargs"; newproject "$d"
git -C "$d" worktree add -q "$work/keepargs-wt" -b wt-k
wtk="$work/keepargs-wt"
(cd "$wtk" && "$cmd" init --slug wt-k >/dev/null 2>&1) || fail "init in the worktree" ""
# The developer adds a flag by hand, which is the documented way.
perl -pi -e 's/^(HOSTSHIFT_ARGS=.*)$/$1 --max-body 20000000/' "$wtk/.ddev/.env"

out="$(cd "$wtk" && "$cmd" check --from https://a.test --to https://wt-k--a.test 2>&1 || true)"
case "$out" in
  *"out of date"*) fail "check accepts a flag it did not write" "$out" ;;
  *) pass "check accepts a flag it did not write" ;;
esac

(cd "$wtk" && "$cmd" init --slug wt-k >/dev/null 2>&1) || fail "init runs again" ""
args="$(sed -n 's/^HOSTSHIFT_ARGS=//p' "$wtk/.ddev/.env")"
contains "init keeps it" "--max-body 20000000" "$args"
# Its own part, in whatever form this project resolves to — here a map, because
# the parent declares additional_hostnames, so `--slug` never appears.
contains "and still writes its own" "--to https://wt-k--keepargs.ddev.site" "$args"
# And it is kept once, not appended on every run.
(cd "$wtk" && "$cmd" init --slug wt-k >/dev/null 2>&1) || true
args="$(sed -n 's/^HOSTSHIFT_ARGS=//p' "$wtk/.ddev/.env")"
n="$(printf '%s\n' "$args" | grep -o -- '--max-body' | wc -l | tr -d ' ')"
check "a second init does not duplicate it" "1" "$n"

echo "== the proxy dials its own web container, not whichever answers"

# The service sits on ddev_default as well as its own network — it must, so the
# router can reach it — and every DDEV web container carries the alias `web`
# there. With this project's own web down, the bare name resolves through the
# shared network to another project: the developer's variant hostname serves a
# different client's site, and the request is delivered into it carrying this
# project's Host header. The proxy logs nothing at serve time, so nothing says so.
compose="$repo/ddev/docker-compose.hostshift.yaml"
up="$(tr '\n' ' ' < "$compose" | sed -n 's/.*--upstream \([^ ]*\).*/\1/p')"
check "the upstream names the project's own container" \
  'http://ddev-${DDEV_SITENAME}-web:80' "$up"
case "$up" in
  *//web:*) fail "the upstream is not the bare alias" "got $up" ;;
  *) pass "the upstream is not the bare alias" ;;
esac

echo "== the post-start marker check in config.hostshift.yaml"

# The guard that tells a developer their command file was not upgraded.
#
# Nothing exercised the yaml at all, so its `grep -q '#ddev-generated'` searched
# a 2,700-line file whose own prose names the marker three times — and a
# developer who removed line 2, which is exactly what DDEV's "Remove unexpected
# #ddev-generated comments" advice says to do, silenced the warning about having
# done it. The command is extracted from the yaml rather than restated here,
# because a copy of it in this file would be the same defect one layer over.
# The condition only: the line in the yaml ends `|| { …`, which opens the
# warning block and cannot be evaluated on its own.
hook="$(awk '/^        head -5/{sub(/ *\|\| *\{.*/, ""); print; exit}' \
  "$repo/ddev/config.hostshift.yaml")"
[ -n "$hook" ] && pass "the yaml still has a marker check to test" \
  || fail "the yaml still has a marker check to test" "no head -5 line found"
markerdir="$work/marker/.ddev/commands/host"
mkdir -p "$markerdir"
# A file whose *prose* mentions the marker, with no marker on its own line — the
# shape a hand-edit leaves behind.
{ printf '#!/usr/bin/env bash\n'
  # Padding, because the prose that fooled the old check lives at lines 13, 120
  # and 2033 of the real file — well past the header a marker may occupy.
  for i in 1 2 3 4 5 6 7 8; do printf '# filler\n'; done
  printf '# This file carries #ddev-generated so the add-on replaces it.\n'
  printf 'echo hi\n'; } > "$markerdir/hostshift"
if (cd "$work/marker" && eval "$hook" >/dev/null 2>&1); then
  fail "a stripped marker is detected" "the prose satisfied the check"
else
  pass "a stripped marker is detected"
fi
# ...and a real marker on line 1 is not a false alarm.
{ printf '#ddev-generated\n'; printf 'echo hi\n'; } > "$markerdir/hostshift"
if (cd "$work/marker" && eval "$hook" >/dev/null 2>&1); then
  pass "a real marker is not a false alarm"
else
  fail "a real marker is not a false alarm" "the check fired on a generated file"
fi

if [ "$fails" -gt 0 ]; then echo "$fails failure(s)"; exit 1; fi
echo "all passed"
