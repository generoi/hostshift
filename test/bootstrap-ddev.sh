#!/usr/bin/env bash
#
# Build a stock DDEV WordPress multisite whose database holds *production*
# hostnames, install the hostshift add-on into it, and run the e2e suite.
#
# The point is reproducibility. internal/e2e can be pointed at any project, but
# until now the only project it had ever run against was one developer's copy of
# acmecorp. This creates the whole thing from nothing, so the acceptance tests
# can be run on any machine and in CI — and it exercises the add-on's install
# path, which is where two bugs were found by hand during M6.
#
#   test/bootstrap-ddev.sh up      create the project and run the suite
#   test/bootstrap-ddev.sh down    delete it
#
# The canonical hostnames use the reserved .example TLD, so nothing here can
# reach a real site even if the loopback containment were broken.
#
# `.example` and not `.test`, which this used until round 60. `check` skips
# `*.test` along with localhost and `*.ddev.site` — right for a real deployment,
# where those never name production — so on the only reproducible WordPress
# fixture this project has, every sibling of the canonical was skipped and the
# whole related-unmapped refusal was unreachable. Measured: six links to
# `media.hostshift-a.test` on the page and `check` exited 0 printing nothing;
# the same fixture under `.example` refused immediately. `.example` is reserved
# by RFC 2606 exactly as `.test` is, does not resolve, and is not in that skip
# list.
set -euo pipefail

PROJECT=${HOSTSHIFT_TEST_PROJECT:-hostshift-e2e}
ROOT=${HOSTSHIFT_TEST_ROOT:-$HOME/Projects/Genero/$PROJECT}
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SLUG=wt-a

CANON_A=www.hostshift-a.example
CANON_B=www.hostshift-b.example
VARIANT_A=$SLUG--$PROJECT.ddev.site
VARIANT_B=$SLUG--b.$PROJECT.ddev.site

say() { printf '\n=== %s\n' "$*"; }

down() {
  if [ -d "$ROOT" ]; then
    (cd "$ROOT" && ddev delete -Oy >/dev/null 2>&1) || true
    rm -rf "$ROOT"
  fi
  echo "removed $ROOT"
}

up() {
  command -v ddev >/dev/null || { echo "ddev is not installed"; exit 1; }
  down

  say "creating a stock DDEV WordPress project in $ROOT"
  mkdir -p "$ROOT/web"
  (
    cd "$ROOT"
    ddev config --project-name="$PROJECT" --project-type=wordpress --docroot=web \
      --additional-hostnames="b.$PROJECT,$SLUG--$PROJECT,$SLUG--b.$PROJECT" >/dev/null
    ddev start >/dev/null
    ddev wp core download --force >/dev/null

    say "installing multisite"
    ddev wp core multisite-install \
      --url="https://$PROJECT.ddev.site" \
      --title="hostshift e2e" \
      --admin_user=admin --admin_password=admin \
      --admin_email=admin@example.test \
      --subdomains --skip-email >/dev/null
    ddev wp site create --slug=b --title="blog b" >/dev/null 2>&1 || true
  )

  say "taking wp-config.php out of DDEV's management"
  # DDEV regenerates any file carrying #ddev-generated on every restart, so the
  # prelude below would be silently clobbered. Removing the marker is what DDEV
  # documents for exactly this case.
  sed -i.bak '/#ddev-generated/d' "$ROOT/web/wp-config.php"
  rm -f "$ROOT/web/wp-config.php.bak"

  say "making the application derive its host from the request"
  # This is the precondition §4.1 states as a fact about Bedrock —
  # config/application.php:49-60 sets WP_HOME from HTTP_HOST — and it is what
  # lets one database be served at more than one hostname.
  #
  # A *stock* DDEV WordPress does not do it: wp-config-ddev.php:26 pins WP_HOME
  # to DDEV_PRIMARY_URL, which defeats production-canonical entirely. So the
  # equivalent is written here, ahead of DDEV's include, which uses
  # `defined(...) ||` and therefore yields.
  python3 - "$ROOT/web/wp-config.php" "$CANON_A" <<'PY'
import sys
path, fallback = sys.argv[1], sys.argv[2]
src = open(path).read()
if 'hostshift-prelude' in src:
    sys.exit(0)
prelude = f"""<?php
// hostshift-prelude: derive the host from the request, as Bedrock's
// config/application.php does (PLAN §4.1). Without this the application is
// pinned to one hostname and no proxy can help it.
$hs_host = $_SERVER['HTTP_HOST'] ?? '{fallback}';
$hs_host = preg_replace('/:\\\\d+$/', '', $hs_host);
define('WP_HOME', 'https://' . $hs_host);
define('WP_SITEURL', 'https://' . $hs_host);
// application.php:196's equivalent: without it is_ssl() is false behind a
// TLS-terminating router and wp-login.php redirects forever.
if (!empty($_SERVER['HTTP_X_FORWARDED_PROTO']) && $_SERVER['HTTP_X_FORWARDED_PROTO'] === 'https') {{
    $_SERVER['HTTPS'] = 'on';
}}
"""
if src.startswith('<?php'):
    src = src[len('<?php'):]

# `wp core multisite-install` has already written the multisite constants,
# pinned to the ddev hostname. Point DOMAIN_CURRENT_SITE at the request host
# instead of defining it twice — the same thing application.php:100 does.
import re
src = re.sub(r"define\(\s*'DOMAIN_CURRENT_SITE'\s*,\s*'[^']*'\s*\)",
             "define( 'DOMAIN_CURRENT_SITE', $hs_host )", src)
open(path, 'w').write(prelude + src)
PY

  say "moving the database to production hostnames"
  # This is the whole premise: the database holds hostnames the local
  # environment has never heard of, and nothing rewrites it afterwards.
  docker exec -i "ddev-${PROJECT}-db" mysql -uroot -proot db <<SQL
UPDATE wp_blogs SET domain='$CANON_A' WHERE blog_id=1;
UPDATE wp_blogs SET domain='$CANON_B' WHERE blog_id=2;
UPDATE wp_site  SET domain='$CANON_A' WHERE id=1;
UPDATE wp_options      SET option_value='https://$CANON_A' WHERE option_name IN ('home','siteurl');
UPDATE wp_2_options    SET option_value='https://$CANON_B' WHERE option_name IN ('home','siteurl');
UPDATE wp_sitemeta     SET meta_value='https://$CANON_A/' WHERE meta_key='siteurl';
SQL

  say "building the image the add-on references"
  # Always built from this checkout, never pulled. The add-on's compose service
  # names ghcr.io/generoi/hostshift:latest, so without this the suite would
  # exercise whatever was last published rather than the code under test — which
  # is the opposite of what a CI run is for. It also means the script works
  # before anything has ever been published.
  docker build -q -t ghcr.io/generoi/hostshift:latest "$REPO" >/dev/null

  say "installing the hostshift add-on"
  # Through `ddev add-on get`, not by copying the files. Exercising install.yaml
  # is the reason this script exists — both add-on bugs the M6 audit found were
  # in the install path, and copying the compose files by hand walks straight
  # past it.
  (cd "$ROOT" && ddev add-on get "$REPO/ddev" >/dev/null)
  sed -i.bak -e "s|www.example.com|$CANON_A|" -e "s|www.example-blog2.com|$CANON_B|" \
    "$ROOT/.ddev/docker-compose.hostshift-loopback.yaml"
  rm -f "$ROOT/.ddev/docker-compose.hostshift-loopback.yaml.bak"

  cat > "$ROOT/hostshift.yaml" <<YAML
version: 1
upstream: http://web:80
sites:
  - name: main
    canonical: https://$CANON_A
    base:      https://$PROJECT.ddev.site
  - name: b
    canonical: https://$CANON_B
    base:      https://b.$PROJECT.ddev.site
YAML

  # Every Bedrock repo in the fleet has a wp-cli.yml carrying at least `path`.
  # A stock download does not, and without it WP-CLI searches upward from
  # /var/www/html and never finds WordPress under web/. `ddev hostshift wp-cli`
  # preserves whatever is here and adds the root url:.
  cat > "$ROOT/wp-cli.yml" <<'YAML'
path: web
YAML
  printf 'wp-cli.local.yml\n' > "$ROOT/.gitignore"

  # Through the add-on's own command, which is the thing that scaffolds a DDEV
  # project — the binary deliberately does not. It needs hostshift on PATH.
  export PATH="$REPO:$PATH"
  (cd "$ROOT" && ddev hostshift init --slug "$SLUG" >/dev/null) \
    || { echo "build the binary first: (cd $REPO && make build)"; exit 1; }

  say "generating wp-cli.local.yml"
  (cd "$ROOT" && ddev hostshift wp-cli --slug "$SLUG" > wp-cli.local.yml)

  say "restarting with hostshift in place"
  (cd "$ROOT" && ddev restart >/dev/null)

  say "creating an admin and an application password for the REST checks"
  APP_PASSWORD=$(cd "$ROOT" && ddev wp user application-password create admin hostshift-e2e --porcelain 2>/dev/null | tail -1)

  say "running the e2e suite"
  cd "$REPO"
  HOSTSHIFT_E2E_USER=admin \
  HOSTSHIFT_E2E_APP_PASSWORD="$APP_PASSWORD" \
  HOSTSHIFT_E2E_VARIANT="https://$VARIANT_A" \
  HOSTSHIFT_E2E_CANONICAL="https://$CANON_A" \
  HOSTSHIFT_E2E_SIBLING_VARIANT="https://$VARIANT_B" \
  HOSTSHIFT_E2E_SIBLING_CANONICAL="https://$CANON_B" \
  HOSTSHIFT_E2E_DDEV_HOST="https://$PROJECT.ddev.site" \
  HOSTSHIFT_E2E_PROJECT="$ROOT" \
    ${GO:-go} test ./internal/e2e/ -v
}

case "${1:-up}" in
  up)   up ;;
  down) down ;;
  *)    echo "usage: $0 [up|down]"; exit 2 ;;
esac
