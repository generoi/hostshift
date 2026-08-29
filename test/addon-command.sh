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
contains "branch becomes a hostname label" "HOSTSHIFT_SLUG=feature-abc-123" "$out"

# BSD sed under a UTF-8 locale aborted with "RE error: illegal byte sequence",
# and en_US.UTF-8 is the macOS default.
(cd "$d" && git checkout -q -b 'feature/käyttöliittymä')
if out="$(cd "$d" && LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8 "$cmd" env 2>&1)"; then
  contains "a non-ASCII branch still derives a slug" "HOSTSHIFT_SLUG=" "$out"
else
  fail "a non-ASCII branch still derives a slug" "exited non-zero: $out"
fi

# The truncation used to happen after the trim, so a slug cut at 30 characters
# could end in a hyphen and derive "…abc---site.ddev.site".
(cd "$d" && git checkout -q -b abcdefghijklmnopqrstuvwxyzabc/x)
out="$(cd "$d" && "$cmd" env 2>/dev/null | sed -n 's/^HOSTSHIFT_SLUG=//p' || true)"
case "$out" in *-) fail "a truncated slug never ends in a hyphen" "slug=$out" ;;
                *) pass "a truncated slug never ends in a hyphen" ;; esac

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
check "hostshift.yaml wins over the parent's hostnames" \
  "HOSTSHIFT_VARIANTS=wt-a--acme.ddev.site" "$(printf '%s' "$out" | sed -n '2p')"
# ...and then the container reads that file itself, since it is mounted and
# carries aliases a canonical=variant list cannot express.
contains "no map is handed over when hostshift.yaml is mounted" \
  "HOSTSHIFT_MAP_ARG=" "$out"
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
  "HOSTSHIFT_MAP_ARG=--map https://acme.ddev.site=https://wt-a--acme.ddev.site" "$out"

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
if [ -e "$wt/.ddev/.env" ] || [ -e "$wt/.ddev/config.hostshift.local.yaml" ]; then
  fail "--dry-run writes nothing" "files appeared"
else
  pass "--dry-run writes nothing"
fi
contains "--dry-run shows what it would write" "HOSTSHIFT_SLUG=wt-a" "$out"

(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || fail "init exited non-zero" ""
if [ -e "$wt/.ddev/config.hostshift.local.yaml" ]; then
  fail "init writes no DDEV config file" "it wrote one"
else
  pass "init writes no DDEV config file"
fi

# An earlier version wrote .ddev/config.hostshift.local.yaml to register the
# variants. It was never needed — DDEV takes the cert SANs from VIRTUAL_HOST too
# — and left behind it is still read as DDEV config, so it feeds its own variants
# back in as canonical hosts.
printf '# generated by `ddev hostshift init`\nadditional_hostnames:\n  - wt-a--acme\n' \
  > "$wt/.ddev/config.hostshift.local.yaml"
out="$(cd "$wt" && "$cmd" init --slug wt-a 2>&1 || true)"
if [ -e "$wt/.ddev/config.hostshift.local.yaml" ]; then
  fail "a stale generated file is removed" "still there"
else
  pass "a stale generated file is removed"
fi
contains "and removing it is said out loud" "removed .ddev/config.hostshift.local.yaml" "$out"

# A config.*.yaml this command did not write is none of its business.
printf 'additional_hostnames:\n  - mine\n' > "$wt/.ddev/config.hostshift.local.yaml"
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || fail "init exited non-zero" ""
if [ -e "$wt/.ddev/config.hostshift.local.yaml" ]; then
  pass "a file without the marker is left alone"
else
  fail "a file without the marker is left alone" "deleted it"
fi
rm -f "$wt/.ddev/config.hostshift.local.yaml"

(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || fail "init exited non-zero" ""
first="$(cat "$wt/.ddev/.env")"
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || fail "init exited non-zero" ""
check "init is idempotent" "$first" "$(cat "$wt/.ddev/.env")"

out="$(cd "$wt" && "$cmd" env --slug other 2>/dev/null || true)"
contains "a second slug does not compound with the first" \
  "HOSTSHIFT_VARIANTS=other--acme.ddev.site,other--nat.acme.ddev.site" "$out"

printf 'UNRELATED=keep\n' > "$wt/.ddev/.env"
(cd "$wt" && "$cmd" init --slug wt-a >/dev/null 2>&1) || fail "init exited non-zero" ""
contains ".env keeps keys that are not ours" "UNRELATED=keep" "$(cat "$wt/.ddev/.env")"
perms="$(ls -l "$wt/.ddev/.env" | cut -c1-10)"
check ".env stays world-readable" "-rw-r--r--" "$perms"

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

# `ddev` may be run from anywhere inside the project.
mkdir -p "$wt/web/app"
out="$(cd "$wt/web/app" && DDEV_APPROOT="$wt" "$cmd" env --slug wt-a 2>/dev/null || true)"
contains "it works from a subdirectory" "HOSTSHIFT_SLUG=wt-a" "$out"

echo
if [ "$fails" -gt 0 ]; then echo "$fails failure(s)"; exit 1; fi
echo "all passed"
