# M6 — pilot against a live site

Run 2026-08-27 against the **running** `herrfors` DDEV project. Read-only: it
makes GETs against a local dev site and imports nothing, so the database is left
exactly as it was.

That bounds what it proves. It proves the proxy against real WordPress output —
real HTML, real multisite, real headers — with the map `canonical = the ddev
hosts`, which is what `.ddev/config.yaml` alone produces (test 10e). It does
**not** prove production-canonical; see "What this does not cover".

## Setup

```yaml
# hostshift.yaml
upstream: http://127.0.0.1:32839      # the web container's port 80, bypassing the router
sites:
  - {name: main, canonical: https://herrfors.ddev.site,     variant: http://localhost:18090}
  - {name: nat,  canonical: https://nat.herrfors.ddev.site, variant: http://127.0.0.1:18090}
```

`localhost` and `127.0.0.1` are two distinct hostnames that both resolve to
loopback, which is what lets one listener serve two blogs and exercise the
multisite inverse mapping for real without touching `/etc/hosts`.

## Result

**Corpus diff: 20 pages, 0 leaks, 0 errors, GREEN.**

Every page's line count is identical between the rewritten canonical bytes and
the bytes the proxy served — 5977/5977, 8803/8803, and so on. Splicing never
rebuilds whitespace, so a line-count change is how re-serialisation would show,
and there is none.

One page came back byte-identical. The other nineteen differ, and **every
difference is a CSP nonce**:

```
rewritten canonical: 339186 bytes
through the proxy  : 339186 bytes

38c38
<     <style nonce='YEmuraTlmlBdmBFWJmpTKYnVoKFyrQLK' >/* vietnamese */
>     <style nonce='pk6IfiwesJsJ8qQ6uTrQ2VTC5NluTVXM' >/* vietnamese */

what kinds of token differ?
    158 nonce
      2 <timestamp>
```

The byte counts are equal. Two fetches of the same WordPress page necessarily
differ by their per-request nonces; nothing else did.

Also verified live:

| | |
|---|---|
| Multisite blog 2 (`127.0.0.1` → `nat.herrfors.ddev.site`) | 200, 0 canonical origins remaining |
| `Content-Length` / `ETag` on a rewritten page | dropped, `Transfer-Encoding: chunked` |
| `Vary` | `Host` |
| Unmapped host | 421, never proxied |

## What the pilot changed

It found 25 stragglers per crawl, and neither kind was a bug in the sweep — both
were surfaces the structured pass was not scanning:

1. **HTML comments.** `sage-cachetags` emits
   `<!-- sage-cachetags Url: https://herrfors.ddev.site/… -->` on every cached
   page — roughly 20 per crawl.
2. **URLs in visible prose.** A privacy-policy paragraph quoting its own URL:
   `https://herrfors.ddev.site/fi/tietosuoja/gdpr/ (“…`.

§4.4 is explicit that every straggler is "a gap in the structured pass and a bug
to fix", so both are now scanned there. The second matters more than it looks:
under production-canonical, a visible prose URL pointing at production is exactly
the hazard §4.4 opens with — a developer copy-pastes it and lands on live
production. §4.4 already accepts the consequence ("a page that intentionally
links to production, as a URL, is rewritten too"), and anchoring keeps test 28's
exclusion intact, because a bare hostname has no scheme and cannot match.

**After the change: 0 stragglers.** The sweep is a silent backstop on real pages,
which is what §4.4 wants it to be.

## What this does not cover

- **The REST API is auth-gated on this site** — every `/wp-json/` endpoint
  returns 401. The 401 bodies pass through correctly with no canonical origins,
  but live JSON rewriting was not exercised. It stays covered by unit tests
  against realistic REST shapes.
- **Tests 29a, 29b, 29d and test 28 over a full crawl** need a
  production-canonical database — `wp_blogs.domain` holding `www.herrfors.fi`
  rather than `herrfors.ddev.site`. That means importing the 524 MB dump over an
  existing local database, which is not reversible by hostshift and is not
  hostshift's call. `ddev snapshot` first, and prefer the existing
  `herrfors-wt-pilot` worktree (its own DDEV project,
  `herrfors-wt-3477594550`, currently stopped) over canonical `herrfors`, which
  is running.
- **The DDEV add-on's router wiring** is written to the mechanism the phpmyadmin
  add-on already uses and its YAML is asserted valid, but it has not been
  installed into a project and started.
