# hostshift

Rewrite origins in HTTP traffic, in both directions.

A site's content refers to one hostname; you want to reach it at another.
hostshift maps between them: responses get the hostname the browser is on,
requests get the hostname the content was written for. Nothing is rewritten at
rest — hostshift never writes to the database itself.

What the *application* writes does pass through it, though, and one thing
changes there: **the scheme**. A canonical is declared with a scheme, and the
matcher accepts either — so `http://www.acme.fi/legacy/` in a post becomes the
variant on the way out, and comes back as `https://www.acme.fi/legacy/`, because
nothing in the variant spelling records which scheme was written originally.
Save a post you did not otherwise edit and every plain URL in it takes the
canonical's declared scheme. That is an upgrade when the canonical is `https`
and a downgrade when it is `http`, which some staging environments are; PLAN §M0
measured one fleet host appearing 165 times over `http` and never over `https`.
The data stays valid and serialized lengths are recomputed correctly — this is
the scheme and nothing else — but it is a real change to rows you did not
intend to touch, and `hostshift diff` cannot see it, because it only exercises
the response direction.

It is a filter and a reverse proxy, it knows nothing about any CMS, and it
scaffolds nothing: no config files written, no slugs guessed, no directories
created. An optional DDEV add-on sits on top and does the opinionated part.

**You have this problem if** one database has to be browsed at more than one
hostname — a git worktree previewing a branch beside the main checkout, or a
production dump you want to open locally without search-replacing it first.
**The one precondition hostshift cannot supply** is that the application answer
to a hostname the map names. Deriving `WP_HOME` from the request satisfies that
for free; so does pinning it, as long as it is pinned to the canonical or the
variant. What cannot work is a site pinned to a *third* hostname — stock DDEV
WordPress does this, setting `WP_HOME` to `DDEV_PRIMARY_URL`, which is the
project's own name and neither side of the map. `ddev hostshift check` says so
when it sees it.

(Stock Bedrock does not derive its host from the request either: its
`config/application.php` *requires* `WP_HOME` and defines it as a constant. The
`env('WP_HOME') ?: 'https://'.$host` form PLAN §4.1 quotes is a local edit, not
upstream. It works fine either way.)

## Install

Binaries for linux and darwin, amd64 and arm64, are attached to each
[release](https://github.com/generoi/hostshift/releases). Put one on `PATH` as
`hostshift`. With Go:

```
go install github.com/generoi/hostshift/cmd/hostshift@latest
```

The DDEV add-on runs the proxy from `ghcr.io/generoi/hostshift`, so the
container half needs nothing installed. The binary on `PATH` is what
`ddev hostshift` and `hostshift map|check|diff` use.

## The smallest thing that works

No DDEV, no config file, no project. Give it a map and it runs anywhere:

```console
$ printf '<a href="https://acme.ddev.site/x">x</a>\n' \
    | hostshift rewrite --map https://acme.ddev.site=https://wt-a--acme.ddev.site
<a href="https://wt-a--acme.ddev.site/x">x</a>
rewrites by surface:
  html-attr                1
candidates by surface:
  html-attr                1
```

The rewritten bytes are on stdout, the counters on stderr — in every
subcommand, so a rewrite can always be piped. `--quiet` drops the counters.

The same engine in front of an upstream:

```console
$ hostshift proxy --upstream http://127.0.0.1:8080 --listen 127.0.0.1:8081 \
    --map https://acme.ddev.site=https://wt-a--acme.ddev.site
hostshift: map from --from/--to
site1  https://acme.ddev.site  ->  https://wt-a--acme.ddev.site
hostshift: listening on 127.0.0.1:8081, upstream http://127.0.0.1:8080
```

Everything that is not an origin in the map passes through byte-identical and
never enters a rewriter.

## Worktrees under DDEV

The case the add-on exists for: one project, one database, and a second
hostname, so a branch can be previewed without a database of its own.
`acme.ddev.site` is served *also* at `wt-a--acme.ddev.site`, and the parent
checkout goes on serving `acme.ddev.site` untouched. Whatever pulls the
database keeps its search-replace; nothing about a normal pull changes.

Install the add-on once per project, from the release:

```
ddev add-on get https://github.com/generoi/hostshift/releases/latest/download/hostshift-ddev.tar.gz
```

**Per project means per worktree, and a worktree does not inherit the parent's
add-ons.** There is no way around this and it is not an oversight of ours: DDEV
assembles a project's services by globbing `docker-compose.*.yaml` in *that
project's* `.ddev/` (`ComposeFiles()`), and it has no global compose file and no
global hooks. A `git worktree` is a separate working directory, so it gets its
own empty `.ddev/`. Only the host command could be shared, via
`~/.ddev/commands/`, and that is one file of the three.

**The parent checkout usually should not have it.** The service takes its routing
from `VIRTUAL_HOST: ${HOSTSHIFT_VARIANTS:-}`, which is empty where nothing was
configured — so an add-on installed in the parent starts a proxy the router sends
nothing to. It does no harm; it is a container for no reason. The pre-start hook
does not fire there either, because it gates on a linked worktree.

The exception is a parent that needs a map of its own — a production-canonical
database, where `wp_blogs` holds the live hostnames and even the main checkout
has to be mapped to be browsable at `.ddev.site`. Then it is an ordinary
hostshift project and `ddev hostshift init` configures it like any other.

Installing anywhere in the checkout is enough for the *ignore*: `install.yaml`
writes to `$GIT_COMMON_DIR/info/exclude`, which linked worktrees share.

`ddev add-on get generoi/hostshift` does **not** work, and the reason is worth
stating rather than leaving you to discover: DDEV expects `install.yaml` at the
repository root, and hostshift keeps its add-on under `ddev/` because the root is
a Go module. The download fails with `Unable to read … install.yaml`. The release
carries a tarball in the shape DDEV wants instead, which also pins the add-on to
a version rather than to whatever `master` happens to be — the add-on and the
published image have to agree about flags, and that skew has broken every project
twice.

A local clone still works if you are developing the add-on itself:
`ddev add-on get ~/src/hostshift/ddev`.

Then `ddev start`. A worktree configures itself: a pre-start hook derives
`.ddev/.env` before compose reads it, so there is nothing to run and nothing to
remember. It writes exactly one file and prints the map it resolved.

`ddev hostshift init` still exists and is still the way to *change* that —
`--slug`, or re-deriving after a branch rename. It writes the same file and then
restarts to pick it up. `--no-restart` writes and stops, which is what the hook
runs.

The hook fires only in a linked worktree, and only when `.ddev/.env` carries no
`HOSTSHIFT_` line, so it is a no-op on every later start. One thing it cannot do
that `init` can: set the exit status. `ddev start` exits 0 whatever `check`
finds, so a script that wants a status must run `ddev hostshift check` itself.

### Worked example

One site, one hostname, and `.ddev/config.yaml` deliberately has no `name:` —
so DDEV names each project after its own directory, and a worktree becomes a
DDEV project of its own with nothing configured.

```console
$ git worktree add ../acme-wt-a -b wt-a
$ cd ../acme-wt-a
$ ddev add-on get https://github.com/generoi/hostshift/releases/latest/download/hostshift-ddev.tar.gz
$ ddev start
hostshift: slug "wt-a", from the git branch wt-a
hostshift: canonical hostnames from /Users/you/Projects/acme, the checkout this was made from
hostshift: wrote .ddev/.env
map from --from/--to
site1  https://acme.ddev.site  ->  https://wt-a--acme.ddev.site
```

The hostshift lines come from a pre-start hook, which runs before compose reads
`.ddev/.env` — so the project comes up already serving the variant. There is no
second start.

Nothing was committed and no map was declared. The hostnames the database holds
are the **parent checkout's** — whatever pulled it search-replaced to
`acme.ddev.site`, not to the worktree's hostname — so the command reads them
from there rather than from the project it is configuring. Getting this wrong
is silent: a map built from the worktree's own config would name
`acme-wt-a.ddev.site`, which appears nowhere in the database, so every page
loads and every link is still the parent's.

The one file that comes out of it:

```sh
# .ddev/.env
#ddev-silent-no-warn
HOSTSHIFT_ARGS=--from https://acme.ddev.site --to https://wt-a--acme.ddev.site
HOSTSHIFT_VARIANTS=wt-a--acme.ddev.site
HOSTSHIFT_WEB_HOSTS=acme-wt-a.ddev.site
```

The first line is not a comment DDEV ignores: without it every `ddev start`
prints a four-line "Custom configuration detected" block. `init` merges into
the file rather than truncating it, so anything else already in there survives.

You do not need to gitignore it. Installing the add-on adds its files to
`.git/info/exclude`, which is per checkout and shared with linked worktrees, so
the ignore travels with the machine rather than with the branch. Removing the
add-on takes the entry back out.

That first `ddev start` is the only one needed: `https://wt-a--acme.ddev.site`
serves the worktree and `https://acme.ddev.site` goes on serving the parent.

### What happens by itself

- **Routing and TLS.** Nothing registers the variant hostnames and nothing
  needs to. The compose service puts `HOSTSHIFT_VARIANTS` into its
  `VIRTUAL_HOST`, which is what traefik routes on, and DDEV feeds every
  service's `VIRTUAL_HOST` into the mkcert SAN list too — verified on v1.25.2
  with no hostshift config file on disk. Everything not named there keeps going
  to `web`, so the canonical site stays reachable alongside the variant.
- **Multiple hostnames.** A multisite needs nothing extra. If the parent
  declares `additional_hostnames`, `init` derives a variant for each one.
- **Keeping the two projects apart.** `HOSTSHIFT_WEB_HOSTS` narrows `web` back
  to the worktree's own hostnames. DDEV derives `name` from the directory but
  not `additional_hostnames`, so a worktree inherits the parent's extra
  hostnames verbatim and — traefik breaking the tie by rule length — silently
  wins them from its first `ddev start` until the next restart. `check` says so
  on every start while the overlap lasts — the pre-start hook deliberately does
  not, because its remedy is "run `ddev restart`" and the hook is inside one.
  Upstream: [ddev/ddev#5486][].
- **Staleness.** A `post-start` hook runs `ddev hostshift check` on every
  `ddev start`, prints what is being served, and says so when `.ddev/.env` no
  longer matches what the project resolves to — which happens on its own, since
  the project name follows the directory while the file does not.
- **The self-redirect carve-out.** 55 of the fleet's 63 DDEV repos ship an
  nginx snippet that 302-redirects a missing `/app/uploads/` request to a
  hardcoded production origin. Rewriting that `Location` would send the browser
  back to the request it just made, so hostshift passes it through unmodified
  and counts it as `self-redirect`. `--strict-origins` returns 404 instead.

  A JSON body over the 8 MB cap is another: it streams through untouched with
  only a `WARN` in `ddev logs -s hostshift`, and under production-canonical every
  origin in it reaches the browser. PLAN §5.8 decides the cap deliberately —
  buffering an arbitrarily large body is the thing it exists to prevent — but
  `/wp-json/wp/v2/posts?per_page=100` on a content-heavy site gets there.

  It is not the only exception. Tier 2 content types — `text/css` and
  JavaScript — are excluded by design (PLAN §5.2), so an absolute canonical URL
  inside a stylesheet reaches the browser unrewritten. That is a deliberate
  decision on evidence from theme sources, and generated CSS under `uploads/`
  (Elementor, WPBakery, the Customizer) is the case that evidence did not
  survey; those files do carry absolute upload URLs. `hostshift diff` reports
  them on its `Tier 2` line rather than failing the run, which is the trigger
  PLAN's fast path names for rewriting them.

  **`--rewrite-type` is the override.** Repeatable, and it names a bare media
  type:

  ```
  --rewrite-type text/css --rewrite-type text/javascript
  ```

  Through the add-on, add it to `HOSTSHIFT_ARGS` in `.ddev/.env` — `init`
  preserves flags it did not write — then `ddev restart`.

  It stays off by default because the cost is real and falls on every response
  of that type. A type in the set is buffered to `--max-body` rather than
  streamed, and stylesheets and script bundles are exactly the large static
  responses the `Content-Type` fast path exists to leave alone. Turn it on when
  `diff`'s `Tier 2` line is non-zero, not before.

  It moves both directions at once, deliberately: a type rewritten on the way
  out and not on the way back in puts variant hostnames into the shared
  database, which is the one failure with no undo.

  The JavaScript case is worth naming separately, because it is a write rather
  than a broken image. With an aggregation plugin that inlines scripts,
  a REST root can land in a `text/javascript` bundle — and then a form on the
  variant submits to production: mail sent, entry stored, on the live site.
  `image/svg+xml` and the XML family are already rewritten by default and need
  no flag.

[ddev/ddev#5486]: https://github.com/ddev/ddev/issues/5486


### If your repo pins `name:`

Most do. A worktree inherits the tracked `.ddev/config.yaml`, so it inherits the
name, and DDEV refuses before hostshift is involved:

```console
$ ddev add-on get https://github.com/generoi/hostshift/releases/latest/download/hostshift-ddev.tar.gz
Unable to get project : a project (web container) in running state already
exists for acme that was created at /Users/you/Projects/acme
```

Give the worktree a name of its own. Every command here runs **inside the
worktree** — running the `printf` in the parent renames the parent, and its
canonical hostname, the one the database holds, stops resolving.

```console
$ git worktree add ../acme-wt-a -b wt-a
$ cd ../acme-wt-a
$ printf '#ddev-silent-no-warn\nname: acme-wt-a\n' > .ddev/config.hostshift-name.yaml
$ ddev add-on get https://github.com/generoi/hostshift/releases/latest/download/hostshift-ddev.tar.gz
$ ddev start
```

The name only has to be unique; hostshift derives the preview hostnames from the
parent's config and the slug, not from it. Between the `printf` and the add-on
install `git status` will list the file — that is expected, because the install
is what writes the `.git/info/exclude` rule that hides it.


## The map

Resolved from three layers, each overriding the last. Discovery by probing is
impossible and would be a silent no-op, so the map is always declared.

1. **DDEV config** — `.ddev/config.yaml`'s `name` and `additional_hostnames`
   give the ordered list of local hosts for free. Read, never written. With a
   `--slug`, which says *which* variant to derive, this is enough on its own
   for a single-environment site.
2. **`hostshift.yaml`** — optional; see below. It *replaces* layer 1 rather
   than merging with it, so a hostname it leaves out has no variant.
3. **Flags** — `--slug`, `--upstream`, `--map C=V`, `--from`/`--to`.

Variants are derived, not written out: `--slug wt-a` prefixes the leftmost
label of each site's base host, giving `wt-a--acme.ddev.site` and
`wt-a--shop.acme.ddev.site`. An explicit `variant:` overrides that.

`hostshift check` validates the result and exits 2 if it is not usable:

```console
$ hostshift check --map https://acme.ddev.site=https://wt-a--acme.ddev.site
hostshift: 1 site(s) from --from/--to — map is injective and anchored
```

**Injective** means no two sites share a variant hostname, so no rewrite is
ambiguous. **Anchored** means running the matcher over each variant hostname
yields no match, so a variant can never be rewritten a second time. Both are
asserted at startup; the proxy refuses to start otherwise.

In a worktree, run `ddev hostshift check` rather than the bare one. The bare
command resolves *this* checkout's hostnames — not the ones the database holds
— and reports that map as valid.

## The database — required, and hostshift does not choose it

**DDEV gives a new worktree an empty database of its own**, so the first thing
you see after `ddev restart` is the CMS installer. hostshift maps hostnames and
takes no part in this; pick one of two:

**Share the parent's.** Right for previewing a branch. One gitignored file in
the worktree, then `ddev restart`:

```yaml
# .ddev/config.worktree.local.yaml
web_environment:
  - DATABASE_URL=mysql://db:db@ddev-acme-db:3306/db
```

**Uploads are separate, and optional.** A shared database references files the
worktree does not have. A browser is usually fine — many repos ship an nginx
rule that 302s a missing `/app/uploads/` to production, and hostshift passes
that through. What it cannot do is serve a file to *PHP*: a template calling
`file_get_contents()` on an upload fatals, which is a 500 rather than a missing
image. Mount the parent's when you need that:

```yaml
# .ddev/docker-compose.uploads.yaml
services:
  web:
    volumes:
      # Relative to THIS file's directory, `.ddev/` — so two levels up to reach
      # a sibling checkout, not one. `../acme/…` silently mounts an empty
      # directory and the failure looks like missing uploads, not a wrong path.
      - "../../acme/web/app/uploads:/var/www/html/web/app/uploads:ro"
```

Read-only is deliberate: an upload made in the worktree then writes its row to
the shared database while the file goes nowhere.

**The worktree also needs any gitignored config the parent has** — `.env` most
often, since a plugin reading a missing key fatals the same way. `cp
../acme/.env .` if there is one; not every site has one.

Every DDEV container is already on `ddev_default`, so the parent's database
container resolves by name with nothing else configured. Note that a shared
database is shared: previewing is safe, but activating a plugin, running a
migration or uploading media writes to the real thing.

Uploads split in a way worth knowing about. The row goes to the shared database
and the *file* goes to the worktree's own `uploads/`, so the parent gets a row
pointing at an image that is not there. On a stock DDEV WordPress it is worse:
`wp-config-ddev.php` pins `WP_HOME` to this project's own hostname, so the
attachment's `guid` is computed from a name that is neither canonical nor
variant, and nothing will ever map it back. `ddev hostshift check` warns when
the served page carries that hostname; the fix is to remove the pin, not to
rewrite the row afterwards.

**Or give it one of its own**, which a branch that has to write needs. The
fastest source is the parent, and it is the state you are already working
against:

```console
$ ddev hostshift copy-db
```

That streams the parent's database straight in — no dump on disk, no production
round trip. It refuses to overwrite a database that already has tables unless
you pass `--force`. It does not touch hostnames: the copy still holds the
parent's, which is exactly why hostshift is needed afterwards just as much as
before.

## What is optional

### `hostshift.yaml` — only for aliases, or for production-canonical

**`ddev restart` after editing it.** The proxy reads the file once, at startup.
`ddev hostshift check` catches it if you forget: the proxy prints its resolved
map to stderr when it starts, so `check` compares that against what this
checkout resolves to now and refuses to call a stale proxy healthy.

Not needed because a site is a multisite. Needed for the two things a DDEV
config genuinely cannot say: **alias hostnames**, so a residual `@staging` URL
left behind by an imperfect `db:pull` is corrected too, and
**production-canonical**, below. Commit it.

```yaml
version: 1
upstream: http://web:80
sites:
  - name: main
    canonical: https://www.example.com            # what the database holds
    base:      https://acme.ddev.site             # what the variant derives from
    aliases:   [https://acme.staging.example.net]
  - name: shop
    canonical: https://shop.example.com
    base:      https://shop.acme.ddev.site
```

`base` is folded in as an alias automatically, so the canonical hostname, the
base and every alias all rewrite to that site's one variant. `.ddev/.env` then
carries `HOSTSHIFT_ARGS=--slug wt-a` and nothing more: the file is mounted into
the container and the proxy reads it directly, which it must, since a flat
canonical=variant list cannot carry aliases.

### Production-canonical — only for a pristine dump

The same engine pointed further: set `canonical` to the live production
hostname and a database that was never search-replaced at all can be browsed
locally. Opt-in per repo, and it is where the hazards live — see PLAN §4.4,
summarised below. Two things become required with it:

- **Loopback containment.** `ddev hostshift loopback > .ddev/docker-compose.hostshift-loopback.yaml`
  writes the site's production hostnames into web's `extra_hosts`, pointed at
  127.0.0.1. Without it WordPress's internal requests (wp-cron, Site Health)
  leave the machine for live production — with `sslverify => false`, against a
  database that believes it is production. The file ships with `www.example.com`
  in it as a placeholder, so generate it rather than assuming its presence means
  anything; `ddev hostshift check` warns when a canonical hostname is not pinned
  to the loopback in web's `extra_hosts` — a comparison against the container's
  configuration, not a reachability probe. It carries no generated-file marker,
  so a hand edit survives the next `ddev add-on get`.

  Note the redirect **replaces** the file. If you have added hosts of your own —
  a CDN origin, a legacy domain, an apex sibling that is not in `hostshift.yaml`
  — regenerating discards them along with the file's own explanatory header.
  Keep those additions somewhere you can re-apply, or edit the file by hand
  instead and use the generated output as a reference.
- **WP-CLI.** `ddev hostshift wp-cli > wp-cli.local.yml`, gitignored. WP-CLI
  resolves a site by URL, so without it every `ddev wp` on a multisite fails
  with "Site not found". It emits the project's existing `wp-cli.yml` back
  verbatim with a root `url:` appended — replacing rather than merging, because
  WP-CLI's own precedence does. Aliases are left exactly as written; some of
  them SSH into production, so for a sibling blog pass
  `wp --url=https://shop.acme.ddev.site` rather than expecting `wp @shop` to
  have changed meaning.

Neither is needed when the database already holds DDEV hostnames.

## Reference

```
hostshift rewrite   a filter — bytes on stdin, rewritten bytes on stdout
hostshift proxy     the same rewriting in front of an upstream
hostshift map       print the resolved map
hostshift hosts     print the hostnames a project declares, one per line
hostshift check     validate the map; exit 2 if it is not usable
hostshift diff      crawl a site two ways and compare, to verify a deployment
```

```
ddev hostshift init      write .ddev/.env         (automatic on start; run it
                                                   to change --slug, or after a
                                                   branch rename)
  --no-restart           write it and stop          (what the pre-start hook runs)
ddev hostshift check     is the deployed map still current  (also the post-start hook)
ddev hostshift copy-db   copy the parent checkout's database into this worktree
                         (refuses to overwrite a non-empty one without --force)
ddev hostshift env       print what init would write, without writing
ddev hostshift wp-cli    emit wp-cli.local.yml on stdout
```

`--slug NAME` overrides the slug derived from the git branch. `--dry-run` shows
what `init` would write without writing it.

Flags worth knowing:

- `--dry-run` counts every rewrite it would make and changes nothing, so it is
  safe to point at a live canonical checkout.
- `--explain` traces every candidate that did *not* result in a rewrite, with
  the reason: `not-a-url`, `host-not-in-map`, `unanchored`, `identity-map`,
  `self-redirect`. This design has many silent-failure modes; that trace is the
  difference between a five-minute diagnosis and an afternoon.
- `--type` selects the surface. `text/html` (the default) streams;
  `application/json` and `application/*+json` are buffered and get an RFC 6901
  path in `--explain`.
- `--reverse` maps variant origins back to canonical, as the request direction
  does.
- `--compress` (proxy) re-encodes per the client's `Accept-Encoding`. Off by
  default; it exists for performance work where transfer size must resemble
  production.

Exit codes: 0 success, 1 runtime error, 2 invalid configuration. These are the
binary's. DDEV collapses a host command's status to 1, so a script testing
`ddev hostshift check` for `-eq 2` will never match — test for non-zero, or call
`hostshift` directly.

`hostshift diff -n 20` is the check that validates a deployment against
reality: it crawls N pages canonically, fetches the same N through the proxy,
runs the canonical bytes through the same engine, and compares. In a worktree
it needs `--canonical-base` to say what the canonical side is, since the
production hostname is not routed locally:

```
hostshift diff -n 20 --slug <slug> --canonical-base https://<project>.ddev.site
```

`--slug` is not optional here. Without it the worktree's map has no variant side
to derive — `hostshift diff` exits 2 with *no variant — pass --slug, or declare
`variant:` on the site* — and there is no `ddev hostshift diff` wrapper to supply
it for you.

**On a worktree whose map comes from DDEV config alone**, add `--variant-base`
too. The variant is derived from the project the command is run in, so in
`acme-wt-a` it comes out `wt-a--acme-wt-a.ddev.site` — a hostname nothing serves,
and every row a 404:

```
hostshift diff -n 20 --slug wt-a \
  --canonical-base https://acme.ddev.site \
  --variant-base   https://wt-a--acme.ddev.site
```

Under production-canonical `--variant-base` is not needed — the map names the
variant — but `--slug` still is, unless the site declares `variant:` outright
rather than deriving it from `base:`. The README's own `hostshift.yaml` derives
it, so `--slug` applies there too.

When you pass one base, `diff` warns if it is not a hostname its map knows and
says what it fell back to. When you pass **both** and the map knows neither —
the worktree case above — the two bases *are* the comparison, and `diff` says so
and uses them as its map. That is what makes the worktree form mean anything:
the bases used to move only the crawl, while the rewriting map still came from
`--slug`, so the leak scan looked for an origin that could not occur and printed
`0 leaks` over pages that were full of them.

The assertions that fail a run are the ones that cannot be innocent — a canonical origin
reaching the browser, a serialized value served with a length that does not
describe its data, which PHP will refuse or silently truncate, a page whose byte
count moved by more than a quarter — which is how an upstream that answers 200
with an empty body, or dies mid-stream, is caught — and a page whose
line count moved by more than a Host-dependent line could explain (over eight
lines, or over a quarter of the page). A one-line difference is reported and
does not fail the run: the two fetches carry different `Host` headers, so
WordPress emits one extra `<link rel="dns-prefetch">` on every page of a
healthy production-canonical site.

That one-line expectation assumes `WP_HOME` is **pinned to the canonical**. If
you derive it from the request — which this README recommends above, and which
is right for serving — then under production-canonical the only baseline you can
fetch locally, `<project>.ddev.site`, emits *that* hostname throughout instead of
the database's. Every page then differs by a tenth of its lines, reported as
`N lines differ (dynamic content?)`, and the run is still GREEN. Measured: 20–29
lines a page against 0–1 with `WP_HOME` pinned. A real re-serialisation sits
inside that noise indistinguishably, so pin `WP_HOME` for the run you intend to
read, or compare against a checkout that has it pinned.

That last one is asserted on the served bytes alone, not by comparing the proxy
against the engine. Every other check here compares the two, so when both are
wrong in the same way the run is green — which is how five consecutive rounds of
silent `wp_options` destruction went unreported by the one check that validates
against reality.

## Building and testing

```
make build                     go build ./cmd/hostshift
make test                      go test ./... plus the add-on command
make test-integration          worktrees through real DDEV and a real router
```

Set `GO=` if `go` is not on `PATH`. `go test ./...` is hermetic: no Docker, no
network. `internal/e2e` drives hostshift against a real DDEV project and is
skipped unless `HOSTSHIFT_E2E_VARIANT` is set; `test/bootstrap-ddev.sh up`
builds a WordPress multisite from nothing to run it against, `down` deletes it.

**The invariant to keep:** rewriting with an identity map must produce
byte-identical output. `internal/rewrite.TestIdentityMapByteIdentity` asserts
it over 5.9 MB in milliseconds and catches every splice and offset defect. Do
not let it go red.

## Design

`PLAN.md` is the authoritative design and is not re-decided here. It is written
against named client deployments and so is not published, which is why comments
throughout the code cite sections of a document this repo does not contain —
`PLAN §4.3` is the shared-database invariant, `§4.4` the production-canonical
hazards, `§5.2` the identity map. The citation still names the decision.

[`docs/performance.md`](docs/performance.md) has the numbers;
[`spike/`](spike/) is the evidence behind the Go decision. MIT licensed.
