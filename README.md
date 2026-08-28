# hostshift

Serve a CMS site from a hostname other than the one baked into its database,
without rewriting the database.

A reverse proxy that maps origins in both directions: the browser talks to a
variant hostname, the application sees the hostname its database was written
for. Nothing in the database is ever rewritten.

[`PLAN.md`](PLAN.md) is the authoritative design and is not re-decided here.
[`spike/`](spike/) is the working evidence behind the Go decision. Progress notes
for completed milestones live in [`docs/`](docs/).

**Status: M6, packaging done.** The engine, the host map, both directions, HTML,
JSON, transport, the binaries, the image, the DDEV add-on and the corpus diff are
in place. What remains is the live pilot — a DDEV project running against an
unrewritten production database — which is also what tests 29a, 29b and 29d wait
on. `PLAN.md` §8 has the detail.

## Using it

```
hostshift rewrite < in.html > out.html   the whole engine as a Unix filter
hostshift proxy   --upstream http://web:80 --listen 0.0.0.0:8080 --slug wt-a
hostshift map     print the resolved host map
hostshift check   validate the config; exit 2 if invalid
hostshift wp-cli  print wp-cli.local.yml for this project
```

### The map

Resolved from three layers, each overriding the last (§5.3). Discovery by probing
is impossible and would be a silent no-op, so the map is always declared.

1. **DDEV defaults.** `.ddev/config.yaml`'s `name` and `additional_hostnames`
   give the ordered list of local hosts for free. For a single-environment site
   with no extra aliases this is enough on its own and no config file is needed.
2. **`hostshift.yaml`.** Adds the production hostnames, alias hosts from other
   environments, the variant pattern and the upstream.
3. **Flags.** `--slug`, `--upstream`, `--from`/`--to`, `--map C=V`.

```yaml
version: 1
upstream: http://web:80
sites:
  - name: main
    canonical: https://www.herrfors.fi
    base:      https://herrfors.ddev.site
    aliases:   [https://herrfors.genero-dev.com]
  - name: nat
    canonical: https://www.herrforsnat.fi
    base:      https://nat.herrfors.ddev.site
```

Variants are derived, not written out: `--slug wt-a` prefixes the leftmost label
of each site's base host, giving `wt-a--herrfors.ddev.site` and
`wt-a--nat.herrfors.ddev.site`. An explicit `variant:` overrides that.

Listing the other environments as `aliases` is what lets a residual `@production`
or `@staging` URL left behind by an imperfect `db:pull` be corrected too.

### Flags worth knowing

- `--dry-run` counts every rewrite it would make and changes nothing, so it is
  safe to point at a live canonical checkout.
- `--explain` traces every candidate that did *not* result in a rewrite, with the
  reason — `not-a-url`, `host-not-in-map`, `unanchored`, `identity-map`,
  `self-redirect`. Given how many silent-failure modes this design has, that
  trace is the difference between a five-minute diagnosis and an afternoon.
- `--strict-origins` turns off the self-redirect carve-out (see below).
- `--compress` re-encodes responses per the client's `Accept-Encoding`. Off by
  default: over loopback compression buys nothing, and it exists for performance
  work where transfer size and `Content-Encoding` must resemble production.
- stdout is data, stderr is diagnostics, in every subcommand. Exit codes are
  0 success, 1 runtime error, 2 invalid configuration.

`--type` selects the surface: `text/html` (the default) streams; `application/json`
and `application/*+json` are buffered and get an RFC 6901 path in `--explain`:

```
$ hostshift rewrite --type application/json --explain --map https://c.example=https://v.example < rest.json
explain (3 events):
      16  rewrote  -   json-string /link                   https:\/\/c.example
      95  rewrote  -   json-string /content/rendered       https:\/\/c.example
     164  rewrote  -   json-string /_links/self/0/href     https:\/\/c.example
```

Everything else passes through byte-identical and never enters a rewriter.

The corpus diff (§7) collapses to one line:

```bash
curl -s https://canonical/page | hostshift rewrite --quiet -C . --slug wt-a \
  | diff - <(curl -s https://variant/page)
```

### The self-redirect carve-out

53 of the fleet's 63 DDEV repos ship an nginx snippet that 302-redirects a
missing `/app/uploads/` request to a hardcoded production origin. Rewriting that
`Location` would send the browser back to the request it just made — an infinite
redirect loop. hostshift detects it and passes the `Location` through unmodified,
counted as `self-redirect`. That is the single enumerated exception to "no
production origin reaches the browser"; `--strict-origins` returns 404 instead.

## The DDEV add-on

The whole add-on is two compose files and an `install.yaml`. No `lib.sh`, no
hooks, no guard — §3 measured what happens when per-repo footprint is not held
to, and the answer was 42 repos carrying 14 different pinned SHAs of the same
submodule.

```
ddev add-on get generoi/hostshift
```

Then, per project:

1. **Declare the map** — `.ddev/config.yaml` alone is enough for a
   single-environment site; `hostshift.yaml` for production-canonical.
2. **Set the variant hostnames** in `.ddev/.env` (`HOSTSHIFT_SLUG`,
   `HOSTSHIFT_VARIANTS`) *and* in `additional_hostnames`, or mkcert issues no SAN
   for them and the browser gets a TLS interstitial instead of a site. A
   three-label variant host is not covered by the `*.ddev.site` wildcard, so it
   needs registering regardless. `hostshift map --slug wt-a` prints the list.
3. **`hostshift wp-cli > wp-cli.local.yml`** if the database holds production
   hostnames — without it every `ddev wp` on a multisite fails to resolve a site.
4. **List this site's production hostnames** in
   `.ddev/docker-compose.hostshift-loopback.yaml`, or WordPress's internal
   requests (wp-cron, Site Health) leave the machine for live production.

Routing is DDEV's router's job and it already does it: the add-on names the
variant hostnames in `VIRTUAL_HOST` and exposes 80/443 → 8080, the same
mechanism the phpmyadmin add-on uses. Everything not named there keeps going to
`web`, so the canonical site stays reachable alongside the variant.

## The corpus diff

The only test that validates against reality. Fixtures would not have caught the
double-port bug; this would.

```
hostshift diff -n 20
hostshift diff --canonical-base http://127.0.0.1:8091 --variant-base http://127.0.0.1:8090
```

It crawls N pages on the canonical site, fetches the same N through the proxy,
runs the canonical bytes through the same engine the proxy uses, and compares.
Byte equality is reported, but the assertions that fail the run are the ones that
cannot be innocent: **a canonical origin reaching the browser**, and **a page
whose line count changed** — splicing never rebuilds whitespace, so a line-count
change means something re-serialised.

## Building

```
go build ./cmd/hostshift
go test ./...
```

Note for this machine: there is no `go` on `PATH`. Go 1.26.5 is in the nix store
at `/nix/store/4im44k446822ixjai0mdaizqskc90qxr-go-1.26.5/bin/go`; put it on
`PATH` (nix profile or devShell) before the commands above work from a plain
shell.

## The invariant to keep

Rewriting with an **identity map must produce byte-identical output**, over
`spike/corpus` and `spike/adv`. It is asserted by
`internal/rewrite.TestIdentityMapByteIdentity` and it holds by construction — the
matcher never splices when a pair maps to itself. It catches every splice and
offset defect, and it runs in milliseconds over 5.9 MB. Do not let it go red.
