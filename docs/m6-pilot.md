# M6 — pilot

Run 2026-08-27. Two pilots: `herrfors` against a genuinely **unrewritten
production database**, and `pellervo` — five blogs — in ddev-canonical mode.

Both corpus diffs are green, which is §8's "done when" criterion for the diff.

`ddev snapshot pre-hostshift-pilot` was taken before anything was imported.
Rollback is `ddev snapshot restore pre-hostshift-pilot`.

---

## 1. herrfors, production-canonical

### The database is production's, untouched

```
$ ddev mysql -N -e "select blog_id, domain, path from wp_blogs" db
1  www.herrfors.fi     /
2  www.herrforsnat.fi  /
```

Before the import it held `herrfors.ddev.site` / `nat.herrfors.ddev.site` — the
result of `db:pull`'s search-replace. Nothing rewrote it back.

### Test 29d — WP-CLI

The regression §4.3 predicts, reproduced exactly:

```
$ ddev wp site list
Error: Site 'herrfors.ddev.site/' not found. Verify DOMAIN_CURRENT_SITE
matches an existing site or use `--url=<url>` to override.
```

After `ddev hostshift wp-cli > wp-cli.local.yml`:

```
$ ddev wp option get home
https://www.herrfors.fi

$ ddev wp site list --fields=blog_id,url
blog_id  url
1        https://www.herrfors.fi/
2        https://www.herrforsnat.fi/
```

**Green — and it corrected two claims in §4.3.** See "Corrections" below.

### Test 29a — loopback containment

Control, before the override reaches the running container:

```
$ ddev exec getent hosts www.herrfors.fi
151.101.1.91   n.sni.global.fastly.net  www.herrfors.fi
```

That is live production via Fastly. After `ddev restart` applies
`.ddev/docker-compose.hostshift.yaml`:

```
$ ddev exec getent hosts www.herrfors.fi
127.0.0.1      www.herrfors.fi
$ ddev exec getent hosts www.herrforsnat.fi
127.0.0.1      www.herrforsnat.fi

http  code=301 ip=127.0.0.1:80
https code=301 ip=127.0.0.1:443
```

Both schemes stay on the box. **Green.**

### Test 29b — TLS, and both halves of the limitation

```
verified   exit 60 (SSL certificate problem)
unverified code=301

SAN: herrfors.ddev.site, nat.herrfors.ddev.site, localhost, web,
     ddev-herrfors-web, ddev-herrfors-web.ddev, 127.0.0.1
```

No production name in the certificate, exactly as §4.4 measured. At the
WordPress level:

```
home_url() = https://www.herrfors.fi/
  wp_remote_post(home, sslverify=false)  [cron]     ok    HTTP 200
  wp_remote_get(home, sslverify=false)   [health]   FAIL  Too many redirects
  wp_safe_remote_get(home)               [oEmbed]   FAIL  cURL error 60
  wp_safe_remote_get(sibling blog)                  FAIL  cURL error 60

  gethostbyname(www.herrfors.fi)    = 127.0.0.1
  gethostbyname(www.herrforsnat.fi) = 127.0.0.1
```

Cron works and nothing leaves the machine. Two results contradict §4.4 and are
corrected below.

### The corpus diff — test 28 over a crawl

```
$ hostshift diff --canonical-base https://www.herrfors.fi \
    --variant-base http://localhost:18095 \
    --resolve www.herrfors.fi:443:127.0.0.1:32851 \
    --canonical-header "X-Forwarded-Proto: https" -n 20

20 pages, 1 byte-identical, 0 leaks, 0 errors
corpus diff GREEN: no canonical origin reached the browser, no page re-serialised
stragglers: 0
```

Every page's line count is identical — 5977/5977, 8803/8803 — and every
difference is a CSP nonce (158 per page, with equal byte counts). Two fetches of
a WordPress page necessarily differ by their nonces; nothing else did.

`--resolve` is not a convenience. Under production-canonical the canonical base
*is* the production hostname, so without it the crawl would hit the client's live
site — the one thing this design exists to keep developers away from.

## 2. pellervo, five blogs

No production dump exists on this box, so this is ddev-canonical: it proves the
five-blog map and the per-blog inverse routing, not production-canonical.

```
  localhost    status=200 bytes=282188   canonical origins remaining=0
  127.0.0.1    status=200 bytes=262518   canonical origins remaining=0
  127.0.0.2    status=200 bytes=305263   canonical origins remaining=0
  127.0.0.3    status=200 bytes=284305   canonical origins remaining=0
  127.0.0.4    status=410 bytes=2572     canonical origins remaining=0

12 pages, 1 byte-identical, 0 leaks, 0 errors
corpus diff GREEN
stragglers: 0
```

Five distinct loopback hostnames serve as the five variants, so one listener
routes all five blogs by `Host`. The 410 on blog 5 is the site's own state —
`ddev wp site list` shows `otlehti` with `archived=1`, and it returns 410 with or
without the proxy.

`/app/uploads/2022/12/mountains.jpeg` came back **byte-identical** through the
proxy: test 12 on a real image rather than a fixture.

---

## Corrections the pilot forced

Applied to `PLAN.md`.

| Claim | Measured |
|---|---|
| §4.3: "WP-CLI merges [`wp-cli.local.yml`] over `wp-cli.yml` with local taking precedence" | **It replaces.** With WP-CLI 2.12.0, a local file containing only `url:` loses `path:`, `require:` and every alias, and WP-CLI can no longer find the installation |
| §4.3: "sibling blogs keep working through the existing aliases — `wp @nat …`" | `@nat` is an **SSH alias into production**. Following that advice runs the command against the live site. The local sibling alias is `@herrforsnat.ddev`, whose `url:` production-canonical breaks |
| §4.4: "**Site Health** loopback probes (same) — work" | **They loop.** DDEV's nginx derives `$fcgi_https` from `$http_x_forwarded_proto` alone, never `$scheme`, so a request on the container's own 443 listener is reported to PHP as plain HTTP and WordPress redirects to the https URL it is already on |
| §4.4: a sibling blog "resolving to `127.0.0.1`, is rejected as unsafe" by `wp_http_validate_url` | It gets as far as TLS and fails with the same `cURL error 60`. Same outcome, different mechanism |

`ddev hostshift wp-cli` now emits the existing `wp-cli.yml` back with a root `url:`
added, and warns for every alias whose `url:` the database no longer holds rather
than rewriting it — silently changing what `wp @ddev` means is worse than saying
so, when some of those aliases are SSH into production.

## An unrelated bug, worth knowing

**Every retained `db:pull` dump on this box begins with seven lines of PHP
warnings** from `config/wp-cli/pre-ssh.php`, before the MariaDB header:

```
Warning: Undefined array key "ssh" in …/config/wp-cli/pre-ssh.php on line 28
Deprecated: preg_match(): Passing null to parameter #2 …
…
/*M!999999\- enable the sandbox mode */
-- MariaDB dump 10.19-11.4.7-MariaDB
```

`ddev import-db` fails on them — **and drops the database before it fails**. The
import here only worked as `tail -n +9 dump.sql | mysql`. That is a `db:pull` bug
in the Genero repos rather than anything to do with hostshift, but it is why the
snapshot mattered, and it means the dumps sitting on developer machines are not
usable as-is.

## Still not covered

- **The DDEV add-on has not been installed into a project and started.** The
  pilot drove `hostshift proxy` directly. Its YAML is asserted valid and its
  router wiring follows the mechanism the phpmyadmin add-on already uses, but
  that is not the same as having run it.
- **The REST API is auth-gated on herrfors** — every `/wp-json/` endpoint returns
  401, so live JSON rewriting was not exercised. The 401 bodies pass through
  correctly with no canonical origins; JSON stays covered by unit tests against
  realistic REST shapes.
- **The database halves of tests 30 and 31** — asserting against real `wp_posts`
  rows after an editor save — need an authenticated wp-admin session.
