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
# ...and a project whose parent declares nothing extra stays silent.
cp "$main/.ddev/config.yaml" "$work/pcfg.hold"
printf 'name: acme\n' > "$main/.ddev/config.yaml"
out="$(cd "$wt" && HOSTSHIFT_HOOK=1 "$cmd" check 2>&1 || true)"
case "$out" in
  "") pass "and one whose parent declares nothing extra stays silent" ;;
  *) fail "and one whose parent declares nothing extra stays silent" "$out" ;;
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
  *"the proxy image is"*) pass "and one whose banner predates the version" ;;
  *) fail "and one whose banner predates the version" "$out" ;;
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
f="${HS_CURL_STATE:-/tmp/hs-curl-n}"
n=$(( $(cat "$f" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$f"
# The body too, since check now reads what the page says about itself.
[ -n "${HS_CURL_BODY:-}" ] && printf '%s\n' "$HS_CURL_BODY"
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
contains "and the cause is named" "--dry-run" "$out"
if (cd "$wt" && PATH="$fakebin:$PATH" "$cmd" check --slug wt-a >/dev/null 2>&1); then
  fail "check fails when the page carries one" "it exited 0"
else
  pass "check fails when the page carries one"
fi

# And a page with none of them is still healthy, so this is not a check that is
# red on everything — the failure mode its own predecessor had.
printf '<a href="https://wt-a--acme.ddev.site/a">a</a>\n' > "$HS_FAKE_DIR/page"
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
  *printenv*)                     echo "DB_HOST=ddev-wt-db" ;;
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

if [ "$fails" -gt 0 ]; then echo "$fails failure(s)"; exit 1; fi
echo "all passed"
