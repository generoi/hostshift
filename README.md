# hostshift

Rewrite origins in HTTP traffic, in both directions.

A site's content refers to one hostname; you want to reach it at another.
hostshift maps between them: responses get the hostname the browser is on,
requests get the hostname the content was written for. Nothing is rewritten at
rest — the database is never touched.

It is a filter and a reverse proxy, it knows nothing about any CMS, and it
scaffolds nothing: no config files written, no slugs guessed, no directories
created. An optional DDEV add-on sits on top and does the opinionated part.

**You have this problem if** one database has to be browsed at more than one
hostname — a git worktree previewing a branch beside the main checkout, or a
production dump you want to open locally without search-replacing it first.
**The one precondition hostshift cannot supply** is that the application derive
its host from the request. Bedrock does; a site that pins `WP_HOME` to a
constant cannot be proxied by hostshift or by anything else.

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

Install the add-on once per project:

```
ddev add-on get generoi/hostshift
```

Then, in the worktree, one command:

```
ddev hostshift init
```

It writes exactly one file, `.ddev/.env`, and nothing else — then restarts the
project to pick it up, and prints the URLs it is serving. That is the whole
required path.

### Worked example

One site, one hostname, and `.ddev/config.yaml` deliberately has no `name:` —
so DDEV names each project after its own directory, and a worktree becomes a
DDEV project of its own with nothing configured.

```console
$ git worktree add ../acme-wt-a -b wt-a
$ cd ../acme-wt-a
$ ddev add-on get generoi/hostshift
$ ddev hostshift init
hostshift: slug "wt-a", from the git branch wt-a
hostshift: canonical hostnames from /Users/you/Projects/acme, whose database this shares
hostshift: wrote .ddev/.env
map from --from/--to
site1  https://acme.ddev.site  ->  https://wt-a--acme.ddev.site
hostshift: restarting to pick it up
```

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
HOSTSHIFT_ARGS=--from https://acme.ddev.site --to https://wt-a--acme.ddev.site
HOSTSHIFT_VARIANTS=wt-a--acme.ddev.site
HOSTSHIFT_WEB_HOSTS=acme-wt-a.ddev.site
```

Ignore it in the project's own `.gitignore` — DDEV's generated
`.ddev/.gitignore` does not cover `.env`. `init` merges into the file rather
than truncating it, so anything else already in there survives.

After `ddev restart`, `https://wt-a--acme.ddev.site` serves the worktree and
`https://acme.ddev.site` goes on serving the parent.

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
  wins them from its first `ddev start` until the next restart. `init` says so
  when it sees the overlap. Upstream: [ddev/ddev#5486][].
- **Staleness.** A `post-start` hook runs `ddev hostshift check` on every
  `ddev start`, prints what is being served, and says so when `.ddev/.env` no
  longer matches what the project resolves to — which happens on its own, since
  the project name follows the directory while the file does not.
- **The self-redirect carve-out.** 55 of the fleet's 63 DDEV repos ship an
  nginx snippet that 302-redirects a missing `/app/uploads/` request to a
  hardcoded production origin. Rewriting that `Location` would send the browser
  back to the request it just made, so hostshift passes it through unmodified
  and counts it as `self-redirect`. That is the single enumerated exception to
  "no canonical origin reaches the browser"; `--strict-origins` returns 404
  instead.

[ddev/ddev#5486]: https://github.com/ddev/ddev/issues/5486

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

Every DDEV container is already on `ddev_default`, so the parent's database
container resolves by name with nothing else configured. Note that a shared
database is shared: previewing is safe, but activating a plugin, running a
migration or uploading media writes to the real thing.

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

**`ddev restart` after editing it.** The proxy reads the file once, at startup,
and nothing detects that you have changed it since — `ddev hostshift check`
compares `.ddev/.env` and the running container's command line, and neither moves
when the file's contents do. Two attempts at detecting this were worse than the
gap: a checksum recorded at `init` time called a correctly-restarted project
stale, and comparing the file's timestamp against the container's proved
unreliable across platforms.

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
locally. Opt-in per repo, and it is where the hazards live — see
[`PLAN.md`](PLAN.md) §4.4. Two things become required with it:

- **Loopback containment.** List the site's production hostnames in
  `.ddev/docker-compose.hostshift-loopback.yaml`, or WordPress's internal
  requests (wp-cron, Site Health) leave the machine for live production. That
  file carries no generated-file marker, so your edit survives the next
  `ddev add-on get`.
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
ddev hostshift init      write .ddev/.env         (required, per worktree)
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

Exit codes: 0 success, 1 runtime error, 2 invalid configuration.

`hostshift diff -n 20` is the check that validates a deployment against
reality: it crawls N pages canonically, fetches the same N through the proxy,
runs the canonical bytes through the same engine, and compares. The assertions
that fail a run are the ones that cannot be innocent — a canonical origin
reaching the browser, and a page whose line count changed.

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

[`PLAN.md`](PLAN.md) is the authoritative design and is not re-decided here.
[`docs/`](docs/) has the pilot notes and the performance numbers;
[`spike/`](spike/) is the evidence behind the Go decision. MIT licensed.
