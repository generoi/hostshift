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

"$GO" build -o "$work/hostshift" "$repo/cmd/hostshift"
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

main="$work/acme"; newproject "$main" 'name: acme\nadditional_hostnames:\n  - nat.acme'
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
out="$(cd "$wt" && "$cmd" env --slug wt-a 2>&1 || true)"
contains "an inherited hostname the parent also serves is called out" \
  "also registered nat.acme.ddev.site" "$out"

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
hook="$(sed -n 's/^ *- *exec-host: *//p' "$repo/ddev/config.hostshift.yaml")"
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
    *"unknown argument"*|*"usage:"*)
      fail "the post-start hook parses under $tag's command" "$out" ;;
    *) pass "the post-start hook parses under $tag's command" ;;
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

# Nothing shared: copy-db must get past the guard. A guard that refuses
# everything passes both tests above and is useless.
out="$(cd "$wt" && "$cmd" copy-db 2>&1 || true)"
case "$out" in
  *"configured to *use*"*) fail "and does not refuse when nothing is shared" "$out" ;;
  *) pass "and does not refuse when nothing is shared" ;;
esac

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
if [ "$fails" -gt 0 ]; then echo "$fails failure(s)"; exit 1; fi
echo "all passed"
