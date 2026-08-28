# hostshift — plan

Serve a CMS site from a hostname other than the one baked into its database,
without rewriting the database.

**Status: design, audited twice, decided. Not yet ready to build — §4.5 lists
checks to run first, and §8 M0 is deliberately no-code.**

An earlier version of this document (`wp-host-proxy/PLAN.md`) was audited on
2026-08-27. Four of its central claims were false. This revision corrects them
and, as a result, recommends a different approach. The history is kept in full
because most decisions here are reactions to specific failures.

---

## 1. The problem

CMSes store absolute URLs in the database. For WordPress:

- `wp_options.home` / `wp_options.siteurl`
- Absolute URLs inside `post_content`
- Serialized data — ACF, menus, Polylang, widgets
- Multisite: `wp_blogs.domain` / `wp_site.domain`, exact-match, **no port column**

So a database only renders correctly on the hostname it was made for. Browsing
it from a second hostname means `wp search-replace` over the whole DB, or
something at runtime.

This bites in two places. **Every `db:pull`** runs a multisite `--precise`
search-replace across a 548 MB dump. And **git worktrees with agents** — parallel
agents on parallel branches, each needing a browser-visible environment, each
currently needing its own DB copy plus another search-replace.

The design in §4 removes both: the database is never rewritten, and is instead a
pristine artifact shared by canonical, every worktree, and CI.

Measured on `herrfors`, 2026-08-27:

| Per worktree | Size |
|---|---|
| `node_modules` | 2.2 GB (the live `herrfors-wt-pilot` worktree is 1.1 GB) |
| `vendor` | 232 MB |
| `web/wp` | 77 MB |
| database | ~665 MB |

The existing `herrfors-wt-pilot` worktree totals **1.6 GB**. Five parallel
worktrees is roughly 8 GB, not the 16 GB claimed in the previous revision.

---

## 2. History

### 2.1 The Cursor session, 2026-08-21

85 turns, *"Leaner WordPress Dev"*, 10:27–14:32, cwd `~/Projects/Genero`. Opened
with *"what would a leaner alternative to ddev be"*, narrowed immediately to the
real blocker:

> hmm problem is mostly that wordpress hardcodes urls in database so they need
> to be rewritten with db:pull (that we do now or other means)

**Rejected as DDEV replacements:** thin shared `docker-compose.yml` (abandoned
after *"sounds we are awfully close to ddev in this plan already :D"*), native
PHP + Homebrew MariaDB, Lando, wp-env, Trellis/Vagrant, OrbStack,
`@e0ipso/ddev-worktree`, a custom Rust proxy (*"should we build our own in
rust?"* — waved off toward Caddy), nginx `sub_filter` (*"sounds brittle"*),
OpenResty.

**Five ideas for the URL problem:**

1. Shared DB, port per worktree, fixed `Host` header — **chosen**, became v0.1
2. `/etc/hosts` with production domains — *"hard to beat"* for single-checkout
   human dev, dies on parallel worktrees. **Never adopted, never revisited**
3. Runtime URL-rewriting mu-plugin — tried, failed (§2.3)
4. Golden DB artifact + copy-on-write clone — raised once, **never pursued**
5. Stop isolating DBs, isolate code only — partly folded into 1

### 2.2 What was built: generoi/ddev-worktree

**v0.1** — canonical DDEV authoritative; each worktree gets a thin sidecar web
container on the shared MariaDB, a Caddy proxy on `:808x`, and a `wt-guard`
hook that **blocks `ddev start`** in worktrees.

**v0.2** (`c093297`) reversed the premise: full DDEV per worktree, unique project
name, DB cloned from canonical, Caddy `:808x` proxy. `Co-authored-by: Cursor`.

After that reversal the **only** remaining differentiator versus plain DDEV is
the Caddy proxy that avoids search-replace.

### 2.3 What failed

**The mu-plugin.** Killed by the user: *"this mu-plugin is kind of proving this
isnt working :D"*. Correct — multisite builds blog URLs from `wp_blogs.domain`,
which has no port column, so `WP_HOME` can never fix it.

**Implementation failures:** `ERR_SSL_PROTOCOL_ERROR` on 8081; missing
`web/wp/wp-blog-header.php` after symlink removal; `sunrise.php` running too
early for `add_action('init')`; `.ddev/nginx/` rewrites not applying so uploads
404'd; and:

    https://herrfors.ddev.site:8081:8081/wp/wp-includes/blocks/gallery/style.css

**The double-port bug.** Diagnosed as Caddy's bare-host rule re-matching URLs
that already carried the port. User: `how did you not catch that?`, after
already having said `please test things thoroughly`.

**Correction from the audit:** v0.2 was *not* uniformly naive about this.
`lib.sh:225-228` emits `header_down Location` **regex** rewrites with `(?::\d+)?`
— already port-idempotent. Only the body `replace` block (`lib.sh:230-234`, four
rules per host, not two) was byte-level. A `(?::\d+)?` guard on the body rules
would have fixed the bug cheaply. This weakens the case for a ground-up rewrite
and must not be misrepresented.

**Rejected on taste:** `ddev wt-e2e` wrappers (*"not a fan of these.... things
should just work"*); unfocused churn (*"stop just changing everything, actually
think about what makes sense...."*); direct pushes to `generoi/github-actions`
(*"in future always open PRs"*).

**The security issue.** This was in `config/application.php`, running in
production:

```php
$_SERVER['HTTP_HOST'] = preg_replace('/:\d+$/', '', $_SERVER['HTTP_HOST']);
if (! empty($_SERVER['HTTP_X_FORWARDED_PORT'])) {
    $_SERVER['SERVER_PORT'] = $_SERVER['HTTP_X_FORWARDED_PORT'];
}
```

> not happy with this in application.php tbh and that it's used for production.
> is it safe/secure?

**Correction from the audit:** the previous revision claimed this was gated
behind `GENEROI_WT` in `config/environments/wt.php`. **That file does not exist
and never did**; `GENEROI_WT` appears nowhere. The user asked for options, was
given three, and chose *"3. fix at caddy"*. Both lines were simply reverted.

The lesson is narrow and must not be over-generalised: **do not make production
trust `X-Forwarded-Port`.** It is *not* a prohibition on forwarded headers —
`application.php:196` already sets `$_SERVER['HTTPS']` from
`HTTP_X_FORWARDED_PROTO`, and the app depends on it.

### 2.4 What the previous attempt got wrong

1. Opened by recommending DDEV be replaced; abandoned 45 minutes later
2. Conflated npm's cache with pnpm's store — *"I was parallelizing them loosely;
   the mechanics differ"*
3. Disk goal never met — *"Disk is basically the same … We save container
   overhead, not disk"*
4. Its own comparison concluded DDEV native + `omit-project-name-by-default`
   would be *"less custom code for the same outcome"*
5. **pnpm was done but never merged.** Branch
   `origin/feat/generoi-worktree-pnpm` exists with `pnpm-lock.yaml` and
   `pnpm-workspace.yaml`. `master` still has `package-lock.json` and 2.2 GB of
   `node_modules`. (The previous revision said it "never landed", implying it
   was never attempted. It was attempted and left unmerged.)
6. All code went into transport; the DB and disk problems went untouched

---

## 3. Evidence

Measured 2026-08-27.

**Fleet:** 63 repos with `.ddev/config.yaml`. PHP 8.3×34, 8.4×21, 8.2×7, 8.0×1.
Webserver `nginx-fpm`×53, `apache-fpm`×10.

**Agent behaviour** (5,808 Claude Code transcripts, 2026-06-20 → 08-26):

- 1,413 subagent spawns; **1,373 (97%) run with no isolation**, same directory
- 40 worktree-isolated spawns, **all in `kokoomus`** — which shows **0.0%**
  parallel-session time
- 352 `git worktree` invocations (Genero 129, kaskipuu 74, suomentyokalu 26)

Two distinct modes: when a human hands out tickets, agents share one directory
and it works (disjoint tasks, plus `Bash(git add -A*)` in the global deny list).
When an agent drives itself unattended, it starts isolating.

Worktrees are created **reactively**, on collision with an in-flight WIP branch:

> okay open a PR, note that another agent is working so use a worktree

**Per-repo installation drifts.** 42 repos carry `.claude/agency` (41 with a
`skills/` dir, 1 uninitialised). The newest skill `facetwp` is in **0 of 42**;
**9** repos sit at 6 of 18 skills; ~14 distinct submodule SHAs are pinned across
42 repos. `install.sh` documents this exact failure mode for *rules* and fixed it
with a single directory symlink; skills and agents were never migrated.

**Design constraint:** anything shipped here must have near-zero per-repo
footprint.

---

## 4. THE DECISION

Revision 1 framed this as an unresolved go/no-go between a proxy and one-time DB
normalisation. **Decided 2026-08-27: build the proxy, with canonical = the
production hostnames.** The reasoning below records why, including what changed.

### 4.1 What the first audit established

On Bedrock, `config/application.php:49-60`:

```php
$host = match (true) {
    ! empty($_SERVER['HTTP_HOST']) => $_SERVER['HTTP_HOST'],
    ! empty(env('DDEV_PRIMARY_URL')) => parse_url(env('DDEV_PRIMARY_URL'), PHP_URL_HOST),
    ...
};
Config::define('WP_HOME', env('WP_HOME') ?: ('https://'.$host));
Config::define('WP_SITEURL', env('WP_SITEURL') ?: Config::get('WP_HOME').'/wp');
```

**`WP_HOME` already follows the request host.** Two consequences:

1. **Zero-config discovery is impossible.** Probing `GET /wp-json/` with
   `Host: X` returns `home = https://X` (`class-wp-rest-server.php:1365-1366`;
   `functions.php:4727` `_config_wp_home`). The proxy would conclude canonical
   == variant and rewrite nothing — a **silent no-op**. The map must be declared
   (§5.3), never discovered.
2. **`home` / `siteurl` need no help.** They were never the problem.

The residue is: absolute URLs in `post_content` and serialized options, and
multisite blog resolution via `wp_blogs.domain` / `wp_site.domain`.

### 4.2 Multisite is N→N with unrelated domains — and Genero already has the data

`herrfors` production `wp_blogs`:

    (1, 'www.herrfors.fi',    '/')
    (2, 'www.herrforsnat.fi', '/')

`SUBDOMAIN_INSTALL` is true, but the blogs sit on **unrelated registrable
domains**. That is the norm, not the exception:

| repo | blogs | production domains |
|---|---|---|
| fsi | 9 | `fsi.idrott.fi`, `idrott.fi`, `esboif.fi`, `ngf.fi`, `gamlakarlebyif.fi`, `raseborgsskyttar.fi`, `piffotboll.idrott.fi`, … |
| pellervo | 5 | `kodinpellervo.fi`, `maatilanpellervo.fi`, `omapiha.info`, … |
| snellmanecom | 5 | `snellmanpetfood.com`, `shop.snellman.fi`, … |
| steripolarnew | 4 | `steripolar.dk`, `*.steripolarnew.kinsta.cloud` |
| spfpension | 4 | |
| fluo | 3 | `flpipe.fi`, `kesrec.fi`, `fluosites.kinsta.cloud` |
| steripolar | 3 | `steripolar.fi`, `www.steripolarvet.fi`, `www.steripolar.se` |
| beamex, mutti, panini, suomentyokalu, herrfors | 2 each | |

12 multisite repos, N from 2 to **9**. Any suffix-derived mapping is dead — and not
only on the production side. `snellmanecom`'s *local* hosts are
`snellmanecom.ddev.site`, `shop.snellman.ddev.site` and
`tilaus.figen.ddev.site`: three different bases.

**However, Genero repos already declare the full index-aligned bijection**
in `robo.yml`, which is what `robo db:pull` uses to search-replace today:

```yaml
multisite: true
env:
  '@ddev':
    url: ['https://herrfors.ddev.site', 'https://nat.herrfors.ddev.site']
  '@staging':
    url: ['https://herrfors.genero-dev.com', 'https://herrforsnat.genero-dev.com']
  '@production':
    url: ['https://www.herrfors.fi', 'https://www.herrforsnat.fi']
```

Position *i* is blog *i* in every environment. This matters as *evidence*, not as an interface: the mapping problem is solvable
because the data exists, is per-repo, and stays maintained because `db:pull`
depends on it. It resolves what the previous revision could not:

- **Discovery (§4.1)** — the application self-reports and cannot be interrogated.
  The map must be declared, and something already declares it.
- **N→N mapping** — already written down, for all 12 multisite repos.

**hostshift must not read `robo.yml`.** That is Genero deployment-tool config,
and a standalone tool that parses it is coupled to one agency's stack. hostshift
defines its own config format (§5.3); Genero writes a small adapter that emits it
from `robo.yml`. The Genero-specific knowledge stays on the Genero side of the
boundary — in the DDEV add-on or a robo task — where it can change without
touching the tool.

It also enables something search-replace does not achieve: because *all*
environments are listed, the map for blog *i* can be
`{production_i, staging_i, ddev_i} → variant_i`, so residual production or
staging URLs left in content by an imperfect `db:pull` are also corrected.

This was the user's own instinct during the 2026-08-21 session — *"hmm could we
make it 'speak' .ddev.config.yml? and anything else our projects have"* → *"or
robo.yml"* — and it was not pursued.

**Edge cases found in the fleet** — re-measured across all 12 multisite repos in
M0, which corrected three of the five claims this paragraph previously made:

- `spfpension` — `@staging` has **1 URL** against 4 elsewhere, as revision 2 said.
  It is written as a *scalar* — `url: 'https://stg-spfpension-staging.kinsta.cloud'`
  — not a one-item list, which is the more useful fact: **the adapter must accept
  both scalar and list forms** of `url:`, and this is the only instance of the
  scalar form in the fleet. An M0 audit script counted list items, got 0, and
  briefly "corrected" this paragraph into being wrong.
- `snellmanecom` — `@kinsta` has 2 against 5. Confirmed.
- `steripolarnew` — extra `@legacy-staging` / `@legacy-production` environments,
  all with equal 4-entry lists. Confirmed.
- `fsi` — **the previous claim that it has no `url` lists at all is false.** It
  declares complete, index-aligned **9**-entry lists for `@ddev`, `@staging` and
  `@production`, and is the largest multisite in the fleet across seven
  registrable domains. Its `@staging` list is `http://`, not `https://` — direct
  confirmation that the map must be origin→origin and carry both schemes (§5.3).
- `suomentyokalu` — **the previous claim that its `@staging` is empty is false.**
  It is aligned at 2. What it does carry, like `beamex`, is an extra `@vagrant`
  environment; `mutti` carries `@dev`.

Unequal lists must fail loudly rather than mis-align by index.

Three further shapes the adapter has to survive, all present in these same files:

- **The environment set is not fixed.** Fleet-wide the keys are `@ddev`, `@dev`,
  `@kinsta`, `@legacy`, `@legacy-production`, `@legacy-staging`, `@netvisor`,
  `@production`, `@staging`, `@vagrant` — and some files quote the key. Enumerate
  them; do not assume a set. (An earlier draft of this paragraph listed five of
  the ten, which rather made the point.)
- **A URL can repeat inside one environment's list**, so the map derived from
  `robo.yml` is **not injective**. `spfpension`'s `@ddev` lists
  `https://osterbotten.spfpension.ddev.site` twice, against two *different*
  canonical origins; `mutti` and `panini` repeat hosts too. §5.4 requires
  injectivity in both directions, so the adapter must reject or resolve this
  rather than emit a map hostshift will refuse at startup — which is test 29c
  firing on real fleet data.
- **Values interpolate** — `mutti`'s URLs are `http://${machine_name}.ddev.site`.
  The adapter must expand `robo.yml`'s placeholders, not read them literally.

### 4.3 Why the proxy, and why canonical = production

The alternative — normalise the database once at `db:pull` time, rewriting
content URLs to root-relative — was seriously considered and rejected.

The decisive reframing: **if canonical is the production hostname, the database
is never rewritten at all.**

| | `db:pull` rewrites to ddev hosts (status quo) | Normalise to relative once | **Proxy, canonical = production** |
|---|---|---|---|
| Full-DB search-replace on every pull | yes | yes, once per pull | **never** |
| Per-worktree search-replace | yes | no | **no** |
| Serialized ACF/Polylang/GF rewritten | yes | yes — unverified risk | **never touched** |
| DB is a reusable artifact | no, environment baked in | partly | **yes — byte-identical to production, shared by canonical, every worktree, and CI** |
| Content matches production | approximately | approximately | **exactly** |
| Runtime component | none | none | proxy |
| Application changes | none | shared `db:pull` task + multisite handling | **none committed** — one generated, already-gitignored file |

`robo db:pull` today runs a multisite `--precise` search-replace across a 548 MB
dump. Production-canonical would let that step disappear — not merely for
worktrees, but for every pull, for everyone — and it removes the one risk that
made normalisation uncertain: nothing ever rewrites serialized data, so nothing
can corrupt it.

**That is the ceiling, not the plan.** Decided 2026-08-28: `db:pull` keeps its
search-replace, the main DDEV site keeps a database written for
`<project>.ddev.site`, and hostshift's job is **worktrees** — serving that same
database at a second hostname so an agent or a branch can be previewed without a
database of its own. Canonical is then the ddev host, not the production one,
and the map is `<project>.ddev.site → wt-a--<project>.ddev.site`.

Everything below still holds, because the engine does not care which hostname is
canonical: it maps origin to origin. What changes is which risks are live.
Production-canonical is what makes §4.4's loopback containment load-bearing —
under ddev-canonical `home_url()` is already a local hostname, so WordPress
talking to itself never leaves the box — and it is what makes a leak reach the
client's live site rather than a neighbouring dev host. Those hazards do not
disappear; they become opt-in, per repo, via a `hostshift.yaml` that declares
production hostnames. herrfors and pellervo are piloted that way and stay that
way.

The map is unchanged in structure — the same index-aligned bijection (§4.2) —
only paired `@production ↔ variant` instead of `@ddev ↔ variant`.

**The "no application changes" claim is not free, and the exception is WP-CLI.**
Under `ddev wp` there is no `HTTP_HOST`, so `application.php:48-53` falls through
to the ddev host and `DOMAIN_CURRENT_SITE` (line 100) is set to it — while the
pristine dump's `wp_blogs.domain` is `www.herrfors.fi`, matched exactly by
`get_site_by_path()`. **Every WP-CLI invocation on all 12 multisite repos would
fail to resolve a site.** Today this works only because `db:pull` rewrote the
hosts — the step this design deletes.

**The fix is `wp-cli.local.yml`, and the fleet is already set up for it.**

WP-CLI's `--url` sets `$_SERVER['HTTP_HOST']` before WordPress bootstraps, so it
satisfies the *first* branch of the `match` above — no environment variables, no
code change. It only needs the right default.

`wp-cli.yml` already carries a per-alias `url:` for every environment and every
blog:

```yaml
'@ddev':              url: herrfors.ddev.site
'@herrforsnat.ddev':  url: nat.herrfors.ddev.site
'@production':        url: www.herrfors.fi
'@nat':               url: www.herrforsnat.fi
```

What is missing is a **root-level** `url:`, which is what plain `ddev wp` (no
alias) falls back on — no repo in the fleet has one, which is why it currently
relies on `DDEV_PRIMARY_URL`.

So: **the add-on generates `wp-cli.local.yml`** with a root-level `url:` set to
blog 1's canonical origin.

```yaml
# generated by hostshift — do not commit
url: https://www.herrfors.fi
```

`wp-cli.local.yml` is **already gitignored in 53 of 60 repos** that carry a
`wp-cli.yml`, so it is an established fleet-standard local override. This means:

- **no committed change in any repo**, so nothing to drift (§3)
- it disappears the moment hostshift is not in use

**Two claims in this section were false, and the M6 pilot found both.**

**`wp-cli.local.yml` does not merge — it replaces.** Measured with WP-CLI 2.12.0:
a local file containing only `url:` loses `path:`, `require:` and *every* alias,
leaving WP-CLI unable to find the installation at all ("This does not seem to be
a WordPress installation"). So `ddev hostshift wp-cli` emits the existing
`wp-cli.yml` back with a root `url:` added, rather than a bare two-line file
written over it — an existing root `url:` replaced rather than duplicated, and a
newline forced between the two, since yaml.v3 rejects a duplicate key and a file
with no trailing newline glues `url:` onto its last line.

It lives in the add-on and not the binary: `hostshift` does not know what a CMS
is, and a proxy with a `wp-cli` subcommand is not a Unix tool.

**"Sibling blogs keep working through the existing aliases" was wrong twice.**
In herrfors' `wp-cli.yml`, `@nat` is an **SSH alias into production** — following
that advice would have run the command against the live site. The *local* sibling
alias is `@herrforsnat.ddev`, and its `url:` is the ddev host, which
production-canonical breaks. The honest instruction is `wp --url=https://www.herrforsnat.fi`,
and `ddev hostshift wp-cli` leaves every alias exactly as written rather than
silently rewriting it — silently changing what `wp @ddev` means is worse than
leaving it alone, especially when some of these aliases are SSH into production.
The M6 pilot's alias warning went with the Go implementation and has not been
rebuilt in shell; `wp-cli.yml` is read-only to this command now.

With the full generated override, test 29d passes on an unrewritten production
database: `ddev wp option get home` returns `https://www.herrfors.fi` and
`ddev wp site list` returns both blogs on their production hostnames.

Note the simplification this produces: under production-canonical the `@ddev` and
`@production` aliases converge on the same `url:`, differing only in transport.
The per-environment alias duplication in `wp-cli.yml` becomes redundant over time.

The seven repos with a `wp-cli.yml` but no `wp-cli.local.yml` gitignore entry —
`ekorosk`, `fsi`, `kokoomus`, `niva`, `panini`, `snellman-group`, `vendoprint` —
need that line added; `hostshift check` should warn when it is missing rather
than silently generating a file that would be committed.

### 4.4 Three hazards this introduces

**A missed rewrite reaches live production.** This is the serious one and it is
new. Under the status quo an unrewritten URL points at a host that does not
resolve locally and fails visibly. Here, an unrewritten
`https://www.herrfors.fi/…` **works** — the browser leaves for the real site, and
an agent could issue writes against production.

**The fix is a post-condition inside the proxy, not setup on the machine.**
Editing `/etc/hosts` was considered and **rejected**: it is a manual, machine-wide
mutation required on every developer box and CI runner, and it is precisely the
per-environment footprint §3 forbids. The tool must just work.

(Correcting the record: it would *not* have broken `db:pull` — every
`@production.host` in the fleet is a bare IP, `79.76.40.134` for herrfors, so
pulls go over SSH to addresses. The real objections are sudo on every box and CI
runner, hundreds of accumulating client domains, losing the ability to browse
real production, and — fatally — landing the browser on DDEV's router holding a
mkcert cert for the wrong SAN, i.e. a TLS interstitial rather than "back on the
proxy".)

Instead, exploit an asymmetry already in the design. The Aho–Corasick automaton
finds occurrences the structured parser does not understand. But it must be built
correctly, and the obvious construction is wrong:

**A bare-hostname automaton reintroduces the double-port bug.** The first draft of
this section argued blunt replacement was safe because "canonical hosts are never
equal to the variant". Substring replacement requires *never contained in*, which
is false by construction: §5.3 puts ddev hosts in the canonical set as aliases,
and §5.4 derives variants by prefixing, so `herrfors.ddev.site` is canonical and
`wt-a--herrfors.ddev.site` contains it. A second pass yields
`wt-a--wt-a--herrfors.ddev.site` — precisely the failure documented in §2.3.

**Build the automaton from left-anchored origin tokens, not hosts.** For each
canonical origin emit `https://H`, `http://H`, `//H`, plus the encoded forms
`https:\/\/H` (JSON) and `https%3A%2F%2FH` / `%2F%2FH` (percent-encoded). Accept
a match only when the byte following `H` is one of `/ : ? # " ' < > \ &`,
whitespace, or end of input. `//herrfors.ddev.site/` then matches and
`//wt-a--herrfors.ddev.site/` does not, because the left anchor fails.

**A trailing root dot terminates the host but is not always consumed.**
`https://www.herrfors.fi.` is the same origin and a browser dereferences it
identically, so treating the dot as "host cut short" rejects the match and
leaks — M0 counted five in herrfors' database. It is absorbed into the span
only when real URL structure follows (`/ : ? #`, or end of value), because the
variant is written in its root-less form and the dot is then dropped. In prose
— "Read more at https://www.herrfors.fi. Thanks" — the dot is a full stop: the
origin is still rewritten, and the dot stays where it is.

**There is a fourth encoding, and it is unbounded: HTML character references.**
A browser decodes them in an attribute value *before* it resolves the URL, so
`href="https:&#47;&#47;www.herrfors.fi/x"` navigates to production — test 28,
and §7 marks that safety-critical. Patterns cannot cover it: `&#47;`, `&#047;`,
`&#x2f;` and `&sol;` are one family of infinitely many spellings. So attribute
values are decoded and re-matched, and a value whose decoded form carries an
origin the raw form did not is replaced with the decoded, rewritten text. That
re-serialises a value, which §5.2 otherwise forbids — it is confined to values
that would *otherwise leak*, so it never runs on a page that is already correct.
The decode is deliberately narrow: whole-value unescaping would also apply the
legacy no-semicolon forms and turn a query string's `&copy=1` into `©=1`, so
only numeric references and the handful of named ones that spell URL structure
are decoded, in place. Counted as its own surface, `html-entity`, because a
non-zero count means content is storing origins in a form this section does not
model. Inside `<script>` and `<style>` the browser does not decode references,
so nothing is decoded there.

With that construction:

1. **Re-scan the rewritten output.** Any surviving canonical origin is a missed
   rewrite.
2. **Replace it**, safely — the anchoring makes the operation idempotent, and it
   cannot touch a bare hostname appearing as prose.
3. **Report every straggler** — offset, context, inferred surface. Each is a gap
   in the structured pass and a bug to fix.

**It runs in-stream, not on a buffer.** The sweep retains a sliding window of
`max_pattern_len - 1` bytes across chunk boundaries; a match is replaced before
that window is emitted, so no already-written bytes need re-alignment and the
body is never buffered whole.

**The window has a left half too, and it is one byte.** The sliding window above
protects a match from being decided on bytes that have not arrived on the right.
A protocol-relative `//H` is decided just as much by the byte on its *left* —
after a letter it is a path segment, after a separator it is an origin — and
that byte has usually already been emitted by the time the rest of the match
arrives. Carrying it forward is not optional: without it a compaction that
happens to leave the match at offset 0 reads as "start of stream", which
anchors, so `.../cache//www.herrfors.fi/…` becomes the variant host **depending
on where the 32 KiB read boundary fell**. That also breaks tests 7 and 29, since
pass 1 changes the token lengths and pass 2's boundary lands elsewhere.

**`--dry-run` does not sweep, and cannot.** The sweep re-scans *rewritten*
output; §5.8's dry run deliberately emits the input unchanged, so a sweep behind
it re-scans the original document and reports every origin on the page as a
straggler — about a thousand false WARNs on a corpus page, each naming a bug
that does not exist. Making the census meaningful under dry run would mean
feeding the sweep rewritten bytes while emitting the original ones, i.e.
buffering the whole body. The census is dropped instead, and the report says so
rather than printing a zero that reads like proof of coverage.

The replacement **changes length** —
`https://www.herrfors.fi` is 23 bytes, `https://wt-a--herrfors.ddev.site` is 32 —
so downstream framing is chunked per §5.2. `--explain` offsets are cumulative
**input**-stream offsets, so they stay stable across a length-changing rewrite.

That requirement binds the *sweep* hardest, and it is the one place it is not
free. The sweep runs downstream of the structured pass and can only count the
stream it actually scans, so its offsets drift by the total length change so far
— on a page with a thousand rewrites of a nine-byte-longer variant, by nine
thousand bytes, silently, in the same event list as the structured pass's
genuine input offsets. The structured pass therefore hands the sweep a map from
its output offsets back to its input ones. The map costs one entry per
*length-changing token* rather than per token, and entries are dropped once the
sweep has passed them, since it only ever asks about increasing offsets.

Accepted limitation: a page that *intentionally* links to production, as a URL,
is rewritten too. On a development clone that is almost always what you want.
Bare hostnames in prose are **not** rewritten and must not be — see test 28.

**Server-side loopback is a separate problem, and the least certain part of this
design.** WordPress makes internal HTTP requests to `home_url()` — cron, Site
Health, some block-editor and REST paths. `WP_HOME` derives from `HTTP_HOST`,
which the proxy sets to the production host, so those calls would leave the
container for the real site. Browser-side rewriting cannot touch this.

The resolution is **container-scoped, not machine-scoped**, which is what keeps it
inside "just works": the container resolves the production hostnames to something
local, and the request never leaves the box. Note the pleasant property — loopback
then stays entirely in canonical space and needs *no* rewriting, because
WordPress is talking to itself using exactly the host its database expects. The
proxy is only ever in the browser's path.

**Delivery mechanism: a `docker-compose.hostshift.yaml` override in `.ddev/`**,
setting `extra_hosts` on the web service. DDEV merges `docker-compose.*.yaml`
overrides by design and this is the standard way add-ons contribute services, so
it is guaranteed to work. Do **not** build on a `web_extra_hosts` config key —
its existence in DDEV v1.25.2 was not confirmed, and the override needs no such
key. Do **not** use `additional_fqdns`: DDEV manages host-level hosts entries for
those, which would point the developer's own machine at the router for the real
production domain — worse than the `/etc/hosts` approach already rejected.

**The concrete leak, confirmed — and measured.** `wp-includes/cron.php:985` calls
`wp_remote_post( site_url('wp-cron.php') … )`, which under production-canonical is
`https://www.herrfors.fi/wp/wp-cron.php`, fired from inside the container on
front-end hits. herrfors has `DISABLE_WP_CRON` defaulting to **false**
(`application.php:126`), and `cron.php:991` passes `'sslverify' => false`, so TLS
is not even a speed bump.

**Control, measured 2026-08-27** — from an unmodified DDEV container
(`ddev-kokoomus-web`, no override):

```
getent hosts www.herrfors.fi
  151.101.193.91  n.sni.global.fastly.net  www.herrfors.fi
```

That is live production, via Fastly. Without the override, a dev box POSTs to
production's cron queue.

**The fix is verified working, end to end.** With
`.ddev/docker-compose.hostshift.yaml` adding `extra_hosts` to the web service:

```yaml
services:
  web:
    extra_hosts:
      - "www.herrfors.fi:127.0.0.1"
      - "www.herrforsnat.fi:127.0.0.1"
```

measured on herrfors:

| check | result |
|---|---|
| `/etc/hosts` inside web container | both hosts → `127.0.0.1` |
| web container listeners | `0.0.0.0:80` **and** `0.0.0.0:443` — it terminates TLS itself, the router is not required |
| `curl http://www.herrfors.fi/` from inside | `code=302 ip=127.0.0.1:80` |
| `curl https://www.herrfors.fi/` from inside | `code=302 ip=127.0.0.1:443` |

Both schemes stay on the box. **The mechanism works and needs no host-level
change.** Stock DDEV emits no `extra_hosts` on `web` at all (checked on kokoomus,
snellmanecom, suomentyokalu, steripolarnew), so the override has nothing to
clobber. (Note the web container listening on 443 was not assumed — it was
checked, and it is what makes the HTTPS loopback resolvable at all.)

**TLS verification fails, exactly as predicted, and that is acceptable.** The
presented certificate is mkcert with
`SAN: herrfors.ddev.site, nat.herrfors.ddev.site, localhost, web, ddev-herrfors-web…`
— no production names — so unverified `curl` succeeds and verified `curl` returns
`000`. Consequences:

- **cron** (`sslverify => false`) — works. Confirmed live in M6: HTTP 200.
- **`wp_safe_remote_get` paths** — the block editor's link-preview endpoint
  (`class-wp-rest-url-details-controller.php:254`) and internal oEmbed
  (`class-wp-oembed.php:454`) — **fail cert validation**. Confirmed live:
  `cURL error 60: SSL certificate problem`.

Accept that. The alternative is issuing a locally-trusted certificate bearing a
real production domain; even scoped to one container's trust store that is a bad
trade for two non-critical features.

**Site Health loopback probes do *not* work, and the reason is not TLS.** This
paragraph previously listed them alongside cron as working. Measured in M6 they
fail with "Too many redirects", and the cause is one line of DDEV's nginx config:

```nginx
map $http_x_forwarded_proto $fcgi_https { … }   # /etc/nginx/nginx.conf
```

`$fcgi_https` is derived from the forwarded header **only**, never from
`$scheme`. A request arriving directly on the container's own 443 listener — which
is exactly what the `extra_hosts` loopback creates — carries no such header, so
PHP is told the request is plain HTTP and WordPress canonical-redirects to the
`https` URL it is already on. Forever. Supplying `X-Forwarded-Proto: https`
turns the same request into a clean 200, which is how it was diagnosed.

It is not hostshift's to fix: hostshift is not in the loopback path at all, and
the file is `#ddev-generated`. §4.4's own fallback already covers it —
"disabling cron loopback and accepting a failing Site Health check is a
legitimate fallback for a development environment" — and cron, the one that
matters, works.

**One more caveat, with its mechanism corrected.** A *sibling* blog's host is not
`$same_host`, so cross-blog `wp_safe_remote_*` fails. The prediction was that
`wp_http_validate_url` would reject it as a private IP; measured, it gets as far
as TLS and fails with the same `cURL error 60`. Same outcome, different
mechanism — worth knowing when reading the error.

Exposure is smaller than it first appears: `DISABLE_WP_CRON` (already referenced
in `herrfors/config/application.php`) removes the most frequent loopback, leaving
Site Health and occasional REST paths. If TLS proves intractable, disabling cron
loopback and accepting a failing Site Health check is a legitimate fallback for a
development environment.

**Writes must be rewritten too.** Response-only rewriting is insufficient:
Gutenberg receives variant URLs and will save variant URLs back into the DB,
polluting the clone and breaking edit round trips. The transformation is
**bidirectional on bodies** — variant → canonical on POST/PUT form data and REST
JSON writes. See §5.1.

**The fleet's uploads redirect becomes an infinite redirect loop. Found in M0.**
This is the third hazard and it was not anticipated anywhere above.

**55 of the 63 DDEV repos** (87%) ship a committed uploads redirect —
`.ddev/nginx/redirect-uploads.conf` in 49, `uploads-redirect.conf` in three,
folded into `nginx_full/nginx-site.conf` in three more:

```nginx
location ^~ /app/uploads/ {
    absolute_redirect off;
    try_files $uri @external;
}
location @external {
    rewrite ^ https://www.herrfors.fi$request_uri redirect;
}
```

Every instance targets a hardcoded remote origin — the site's public production
domain in most, a `*.kinsta.cloud` hosting hostname in about a third, but always
a host the repo's own `robo.yml` declares. It fires only on a miss, and it is why
nobody has noticed that uploads are barely synced (below).
Under hostshift:

1. browser requests `https://wt-a--herrfors.ddev.site/app/uploads/2025/07/x.jpg`
2. hostshift maps `Host` → `www.herrfors.fi`, forwards to `web:80`
3. nginx misses, `@external` fires: `302 Location: https://www.herrfors.fi/app/uploads/2025/07/x.jpg`
4. `Location` is Tier 1 (§5.2), so hostshift rewrites it back to
   `https://wt-a--herrfors.ddev.site/app/uploads/2025/07/x.jpg`
5. → step 1. `ERR_TOO_MANY_REDIRECTS`.

Today this works, because the browser is on the ddev host, no proxy is in the
path, and the 302 simply leaves for production. **hostshift converts a working
production fallback into a redirect loop on 87% of the fleet.**

**Decision: a self-redirect guard, in the proxy, counted.** On a 3xx, if
rewriting `Location` canonical→variant would yield a URL equal to the incoming
request URL, emit the `Location` **unmodified** and count it as
`self-redirect-passthrough`. It is loop-free, needs no per-repo config, and is
narrow in the right way: it fires on the exact shape that loops.

**The guard compares whole URLs, query included, and must keep doing so.** The
fleet writes the snippet as `rewrite ^ https://host$request_uri redirect`, and
nginx appends the query string a *second* time unless the replacement ends in
`?`. So the `Location` comes back one query longer than the request that
produced it, the equality test never matches, and each hop appends another copy
until the browser gives up at 414 — measured live on herrfors. M6 made the guard
ignore the query to absorb that, and it was reverted: hostshift would then be
carrying a workaround for a one-character bug in someone else's nginx config,
and would silently pass through every redirect that changes only the query on
the same path. The fix is `$request_uri?` in the repo, and `hostshift check`
names the file and prints the offending line — 52 of the 53 fleet repos with
this snippet still need it.

Two further limits worth stating rather than discovering. It defuses **period-1**
loops only — a period-2 cycle, where blog A's origin redirects to blog B's canonical
host and back, is constructible in a multisite map and the equality test never
fires. And because every `redirect-uploads.conf` hardcodes **one** origin, a miss
on a *non-primary* blog redirects to blog 1's canonical host, which is not the
request's own origin: the guard does not fire on the first hop, hostshift
rewrites it to blog 1's variant, and the browser lands on the wrong blog before
the guard catches it on the second. For `pellervo` that is four blogs of five. So
test 32 must assert *terminates in a bounded number of hops and never loops*,
not "exactly one 302", and must cover a non-primary blog.

The cost is honest and must be stated: that response carries a production origin
to the browser, so **test 28 gains an explicit, enumerated carve-out** for 3xx
self-redirects. It is a read-only asset `GET`, not the write hazard §4.4 opens
with, and the alternative — rewriting it into a loop, or 404ing — is a
regression against what developers have today. `--strict-origins` turns the
carve-out off and returns 404 instead, for the corpus diff and for test 28's
full-crawl run in M6.

Do **not** solve this by having hostshift fetch the upstream asset itself: that
reintroduces production traffic on the server side, which is exactly what the
`extra_hosts` override above exists to prevent.

### 4.5 Confirmed in M0

Both checks ran on 2026-08-27. Neither blocks the decision; both change what the
project is justified by. Full method and numbers in `docs/m0-preflight.md`.

**Usage survey — both halves hold.** **113 of the 184** top-level Claude Code
sessions on this box are in DDEV repos — 61%, not the rounding error an earlier
draft reported by dividing sessions by *all* transcripts including 5,648 subagent
ones. And there are **19 worktree checkouts on disk** across the fleet, 18 of them
`kokoomus/.claude/worktrees/agent-*`, created on 2026-08-20 in bursts of three to
four inside a minute, with **57 worktrees registered** across 30 repos.

That is parallel agents in worktrees, in a DDEV repo, at the concurrency this
design serves. Note that kokoomus is the project §3 cites for "0.0%
parallel-session time": its parallel agents are *subagents*, which a
session-transcript metric cannot see, so §3's figure measures something narrower
than it sounds.

§4.3's other claim holds too, smaller than first stated: **8.6 GB across 19
retained dump files**, of which 13 are production pulls and 11 belong to
single-site repos, spanning 2025-08 → 2026-08. Both are one developer's box, so
both generalise to the fleet by inference rather than measurement. §9's staging
plan is unchanged.

**Uploads — the sync does not cover content, and an nginx redirect has been
hiding it.** On herrfors, content references **2,661 distinct** upload URLs
(122,179 occurrences). **2,534 of them — 95.2% — are absent locally.** The local
tree is 159 files and 1.2 MB, mostly hand-added SVGs. `web/app/uploads/*` is fully gitignored and
`robo files:pull` has evidently never completed here.

This is **not** a regression hostshift introduces — under the status quo those
same 2,499 files are missing too. It matters for two other reasons:

1. It is what makes the `redirect-uploads.conf` hazard above load-bearing rather
   than theoretical: that redirect fires for the 95.2% of distinct upload URLs
   that are absent locally.
2. It quantifies §9 — with care about *what* it quantifies. **95.2% of the
   distinct upload URLs** content references are absent locally, so with
   hostshift not running they resolve against live production. That is a
   distinct-URL rate, not a request rate: the files that *are* present are theme
   SVGs, icons and fonts, which is what loads on every page view, so the share of
   actual requests reaching production is lower and was not measured.

Do not treat "sync the uploads properly" as a hostshift deliverable. It is
`robo files:pull`'s job, it is orthogonal, and the self-redirect guard makes
hostshift correct whether or not anyone does it.

§5 is the design.

---

## 5. Proxy design (corrected)

### 5.1 Request direction — NEW WORK, not inherited

The previous revision said this was "already solved in v0.2". **False.**
`lib.sh:252` is `header_up Host {http.request.host}` — it forwards the *request*
host. That worked only because v0.2's variant differed from canonical by **port
only**, so the hostname was already canonical. Changing the hostname makes
variant→canonical request mapping new, unimplemented, and the hard part.

- **Canonical is the production hostname** (§4.2). The browser's variant host is
  mapped to the production host of the *same blog* before the request is
  forwarded. WordPress sees exactly the host its database was written for.
- **Request bodies are rewritten variant → canonical** on POST/PUT/PATCH, for
  `application/x-www-form-urlencoded`, `multipart/form-data` non-file parts, and
  `application/json`. Without this, content saved through wp-admin stores dev
  hostnames. Mirror of §5.2, sharing its machinery.
- **The request line and query string are rewritten too**, including
  percent-encoded and JSON-escaped origin forms. This is not optional:
  `wp-login.php?redirect_to=…` is validated by `wp_validate_redirect()`
  (`pluggable.php:1665`) against `home_url()`'s host, so a variant origin is
  silently discarded and login returns to the wrong place. The same applies to
  `/wp-json/wp-block-editor/v1/url-details?url=` and the oEmbed proxy. §5.5
  previously left this open; it is decided here. Rewriting query origins is only
  wrong when done response-side alone — done symmetrically it is correct.
- **Multipart:** rewrite only parts whose `Content-Disposition` carries no
  `filename=` and whose type is `text/*`; file parts pass through byte-identical.
  Apply only when the whole body is under the size cap (§5.8); above it, stream
  through untouched and log the skip.
- **Multisite:** apply the **inverse** of the host bijection per request.
  `nat.wt-a.local` must arrive as `Host: nat.herrfors.ddev.site` — the sibling
  blog's host, not the network's main host. `ms-settings.php:62-68` lowercases
  and strips `:80`/`:443`, then `get_site_by_path()` (`ms-load.php:163,313`)
  matches `wp_blogs.domain` **exactly**.
- **`Referer` must be rewritten variant→canonical.** `functions.php:1986` runs
  it through `wp_validate_redirect($ref, false)`, which rejects any host ≠
  `home_url()` host, so without this `wp_get_referer()` is `false` throughout
  wp-admin and bulk actions and back-links degrade.
- **Send `X-Forwarded-Proto: https`.** `load.php:1659-1673` makes `is_ssl()`
  true only via `$_SERVER['HTTPS']` or `SERVER_PORT === '443'`;
  `application.php:196` already reads `HTTP_X_FORWARDED_PROTO`. Omit it and
  `wp-login.php:14-22` (`force_ssl_admin() && ! is_ssl()`) redirects forever.
- **Never send `X-Forwarded-Port`** (§2.3).

### 5.2 Response direction — locate spans, splice, never re-serialise

**The core property: output is byte-identical to input everywhere a rewrite did
not occur — universally, not merely outside modified tags.**

`golang.org/x/net/html`'s `Tokenizer` provides the framing, and its `Raw()`
carries a documented contract:

> "The token stream's raw bytes partition the byte stream (up until an
> ErrorToken). There are no overlaps or gaps between two consecutive token's raw
> bytes. One implication is that the byte offset of the current token is the sum
> of the lengths of all previous tokens' raw bytes."

So untouched tokens are emitted verbatim and byte offsets come for free — which
is also what `--explain` (§5.8) needs. Verified over **15 real pages (5,940,172 bytes)** and 36 adversarial fixtures,
including with a one-byte-at-a-time reader.

Use the **`Tokenizer`, never `html.Parse` + `html.Render`.** The lossy-round-trip
reputation belongs entirely to the parser/renderer, which alphabetises
attributes, lowercases names, decodes entities and inserts `<tbody>`. `goquery`
and `cascadia` build on that tree and are therefore also unsuitable — and
unnecessary, since §5.2 scans every attribute rather than selecting by CSS.

The tokenizer does not report attribute-value offsets, so a **~60-line intra-tag
span scanner** supplies them: HTML5 tag-open / attribute-name / before-value /
quoted-and-unquoted-value states. Validated against the tokenizer's own output — though note the committed
validator compares only attribute **counts and names**, never `ValueStart`/
`ValueEnd`. Land the value-span assertion before M1 (it passes: 37,280 values,
zero real mismatches; two apparent ones are comparison artifacts —
`html.UnescapeString` being more aggressive than the tokenizer's attribute mode
on legacy entities like `&not`, and CRLF normalisation). Counts:
**19,953 start tags, 37,280 attributes, 9 divergences across 6 files** — all
duplicate attribute names (`rel`, `media`, `defer`, `class`,
`data-wp-on-window--resize`), where it reports every physical position; dropping
duplicates would lose a splice site.
Copy `Raw()` before calling `TagName()`/`TagAttr()`. The docs make **no** lifetime
promise about `Raw()` — the partition guarantee is all they state; safety today
rests on `TagAttr` happening to allocate via `bytes.ReplaceAll` before its
in-place unescape, which is an implementation detail. Separately, the slices from
`Text()`/`TagName()`/`TagAttr()` *are* documented to change on the next `Next()` —
never retain them. (`spike/go/full/main.go:100–105` aliases `raw` across a
`TagName()` call and must be fixed before M1.)

There is **no text-fragmentation problem**: a 700 KB inline `<script>` arrives as
a single text token, and token counts are identical at chunk sizes 1, 7, 4096 and
whole-file.

The single exception to splicing is an HTML fragment nested inside a JSON string
(`content.rendered`): that value is decoded, rewritten, re-encoded and spliced
back. Recursion stops at depth 2.

**Known gap — closed in M3, and it was wider than described.** Without a tree
builder the tokenizer does not track foreign content, so `<svg><title>` is
treated as raw text and a URL in an `<a href>` inside it is missed. The same is
true of `<noscript>`, `<textarea>`, `<iframe>` and `<title>`: the tokenizer hands
back the *markup* inside every raw-text element as one opaque text token, so an
attribute in there never reaches the attribute scan. The corpus turned up a real
`<noscript>` case, not an SVG one.

Scanning the text of **every** raw-text element — not just `<script>` and
`<style>` — closes all of them in the structured pass, which is where §4.4 says
such gaps belong. Anchoring is what makes it safe on prose-bearing elements like
`<title>`: it can only match a real origin, never a bare hostname.

Measured after the change: the structured pass makes 1,112 rewrites across the
corpus and the straggler sweep catches **zero**. It is a backstop again rather
than a load-bearing part. Byte preservation is unaffected either way.

#### Length, validators, and framing

Every rewrite changes body length. Therefore:

- **Responses:** drop `Content-Length` and emit chunked whenever any handler
  fired; never forward a stale length. Drop or weaken `ETag` and `Last-Modified`
  on any modified response — otherwise a conditional request returns 304 and the
  browser serves a **cached canonical-bearing body**, defeating test 28 silently.
- **Requests:** body rewriting changes request length; recompute `Content-Length`
  or send chunked upstream.
- `Vary` and `Accept-Ranges` need the same care.

#### The surface, ranked by measured value

**Tier 1 — required.**

| Surface | Notes |
|---|---|
| Response headers | `Location`, `Content-Location`, `Refresh`, `Link`, `Content-Security-Policy`(`-Report-Only`), `Access-Control-Allow-Origin`, `Set-Cookie` `Domain=` |
| HTML | **Every attribute value on every element** — see below |
| Inline `<style>` and `<script>` | Each arrives as a **single** `TextToken` (measured: a 700 KB inline script is one token), so scan it directly — no accumulation. **This is where the CSS and JS URLs actually are.** `type="application/ld+json"` and `type="application/json"` route to the JSON rewriter |
| JSON | URL-valued strings, **plus the HTML rewriter over string values containing HTML** — `content.rendered` is a full HTML blob a URL-only rule skips |

**Do not use an attribute allowlist.** An allowlist guarantees a long tail of
leaks; the fleet already supplies three — `style="…url(https://…)"` on cover and
hero blocks, `data-src`/`data-srcset`/`data-large_image` from lazyload and the
WooCommerce gallery, and Yoast's JSON-LD graph on every page. Instead, following
pywb (§10), **run the origin automaton over every attribute value** and rewrite
any value whose origin is in the canonical set. Keep a named list only where
*structured* parsing is required:

- `srcset` / `imagesrcset` — comma-separated with descriptors
- `<meta http-equiv=refresh>` — `N;url=…`
- `<iframe srcdoc>` — nested HTML
- `ping` — space-separated
- `<base href>` — highest-severity single omission: one tag re-points every
  relative URL at canonical and the browser leaves the proxy entirely

This is cheaper than an allowlist, strictly more complete, and removes most of
the work the §4.4 sweep exists to catch.

**M3 found that none of those five needs structured parsing after all.**
Anchoring locates origins wherever they sit, so the *grammar* of the value never
has to be understood: `srcset`'s commas and descriptors, `refresh`'s `N;url=`,
`ping`'s spaces, and `srcdoc`'s entity-encoded HTML all fall out of running the
automaton over the whole value, because the origins appear literally in every
one. `<base href>` was never a parsing problem at all — it is one more attribute
value, and the every-attribute scan already covers it.

The named list is kept in the tests as the cases most likely to regress, not in
the code as five parsers. If a future surface genuinely needs its grammar
parsed, the evidence for it should be a failing test, not this list.

`Set-Cookie` `Domain=` is **Tier 1 and load-bearing** — the previous revision
wrongly called it optional on the strength of herrfors alone.
`ms_cookie_constants()` defines `COOKIE_DOMAIN` from the network domain on any
subdomain multisite that does not set it explicitly, so such a site would emit
`Domain=.www.herrfors.fi` while the browser is on a variant host — the cookie is
discarded and **login fails outright**. Prefer *dropping* the attribute
(host-scoped is always safe) over substituting it.

**Tier 2 — measured to be near-worthless; do not build initially.**

Standalone `.css` and `.js` files served from disk. Surveyed herrfors:
**88 CSS and 185 JS files in `web/app/themes/`, zero containing an absolute site
URL** — the only matches are `bud.config.js`, a build config that is never
served. Built CSS elsewhere in the fleet contains no `url(https://…)` at all.
Themes avoid absolute URLs precisely because they would break across
environments. The absolute URLs live in PHP patterns, i.e. in rendered HTML,
which Tier 1 already covers.

**Tier 3 — drop for now.** XML sitemaps and RSS. Real in production SEO terms,
irrelevant to an agent rendering pages in a worktree. Revisit only on evidence.

(herrfors happens to set `COOKIE_DOMAIN` to `''` at `application.php:106`, so it
emits no `Domain=` at all — which is why an earlier revision wrongly generalised
this surface as optional. Other subdomain multisites do not set it.)

Everything else is byte-identical passthrough.

#### Fast path

**The gate is `Content-Type`, not a body scan.** An earlier revision proposed
running Aho–Corasick over every raw body and skipping parse on no match. That is
backwards: learning that a 4 MB JPEG contains no canonical host means reading 4 MB
through the automaton — buffering exactly the bodies most worth streaming — when
the header answered it for free.

Anything outside the rewritable set — `text/html`, `application/json`,
`application/*+json` — streams through untouched, never entering a rewriter.
`text/css` and `application/javascript`/`text/javascript` are **deliberately
excluded** per Tier 2, and added only if the corpus diff shows a leak (if ever
added, include `text/javascript`: it is the IANA-preferred type and what nginx
serves). That is where "most responses are never
parsed" comes from, at zero buffering cost.

Within the rewritable set the automaton is used only on already-bounded values:
per attribute value and per header value; over accumulated `<script>`/`<style>`
text (which must be accumulated anyway); and over the JSON buffer. **HTML is never
whole-body prefiltered** — the tokenizer already is the fast path: with no rewrite
firing, concatenating `Raw()` reproduces the input exactly, measured at ~300 MB/s.

#### Compression

Send `Accept-Encoding: identity` **upstream**. The fleet has no compression
config of its own, so DDEV's stock nginx applies — gzip for text types, no
brotli module, no zstd — and asking for identity means the common path needs no
decoder at all. Keep a gzip decoder as fallback for upstreams that compress
regardless.

Serve **identity downstream by default**: over loopback and the Docker bridge
compression buys nothing, and v0.2's mistake was forcing `identity` on the
*browser* side, silently changing behaviour under test. Provide `--compress` to
re-encode per the client's `Accept-Encoding` for performance work, where transfer
size and `Content-Encoding` must resemble production.

**Never rewrite what cannot be decoded.** An unsupported encoding is passed
through byte-identical and logged as skipped. `deflate` is not worth
implementing; `bzip2` is not an HTTP content encoding.

### 5.3 Configuration

Discovery by probing is unusable (§4.1). The map is **declared**, in three
layers, each overriding the last.

**1. DDEV defaults (optional, used when present).** `.ddev/config.yaml` gives
`name` plus `additional_hostnames`, which yields the ordered list of local hosts
for free — `herrfors` + `[nat.herrfors]` → `herrfors.ddev.site`,
`nat.herrfors.ddev.site`. For a single-environment site with no extra aliases
this is sufficient on its own and **no hostshift config file is needed at all**.
DDEV is a widely used tool and this is real value; it is the only third-party
format hostshift reads.

**2. `hostshift.yaml`.** The tool's own format. Adds alias hosts (other
environments' domains that may still appear in content), the variant naming
pattern, and the upstream.

```yaml
version: 1
upstream: http://web:80

variant:
  pattern: "{slug}--{leftmost-label}"   # applied to the leftmost label of `base`

sites:
  - name: main
    canonical: https://www.herrfors.fi
    base:      https://herrfors.ddev.site          # variant derived from this
    aliases:   [https://herrfors.genero-dev.com]
  - name: nat
    canonical: https://www.herrforsnat.fi
    base:      https://nat.herrfors.ddev.site
    aliases:   [https://herrforsnat.genero-dev.com]
```

Variants are **derived**, not written out: `--slug wt-a` yields
`https://wt-a--herrfors.ddev.site` and `https://wt-a--nat.herrfors.ddev.site`.
An explicit `variant:` on a site overrides derivation for the rare case that
needs it.

(Revision 2 wrote the second one as `nat.wt-a--herrfors.ddev.site`, which
contradicted both §5.4's rule — "prefixing the leftmost label" — and §5.6's own
example, `wt-a--nat.herrfors.ddev.site`. Corrected in M2 to the leftmost-label
form, which is the only one that is a function of `base` alone: it needs no
knowledge of which label is the DDEV project name, so it derives correctly for
`snellmanecom`'s three unrelated local bases as readily as for herrfors'.)

**The map is origin→origin (scheme + host + port), never host→host.**
`application.php:59` hardcodes `https://`, so every canonical URL in output is
`https://…`. A host-valued map plus a plain-HTTP listener would rewrite
`https://www.herrfors.fi/x` to `https://wt-a--herrfors.ddev.site/x` — wrong
scheme, wrong port, unreachable, and `force_ssl_admin()` then redirect-loops.
`:80`/`:443` are normalised away per §5.5.

`canonical` plus `base` plus `aliases` form blog *i*'s canonical set;
`variant.pattern` applied to `base` derives its variant, with an explicit
`variant:` per site as an override. **With canonical = production (§4.2), the production hostname is the
primary entry**; ddev and staging hosts remain listed so that a database which
*has* been through `db:pull` also works. Listing non-local environments is what lets residual production or
staging URLs — left behind by an imperfect `db:pull` — be corrected too.

**3. CLI flags.** `--upstream`, `--map`, `--slug`, `--canonical` override
everything, so the tool is usable with no config files at all.

**Genero's adapter is out of scope for this repo.** A robo task or the DDEV
add-on generates `hostshift.yaml` from `robo.yml`. The fleet edge cases found in
§4.2 — `steripolarnew`'s unequal environment lists, `fsi`'s empty `@ddev` list —
are that adapter's problem to resolve or refuse, not hostshift's.

**hostshift validates its own config and fails loudly when:**

- a canonical host appears in more than one site entry
- the map is not injective in both directions
- a generated variant collides with any canonical
- `sites` is empty and no DDEV config or `--map` is available

### 5.4 Variant hosts and idempotency

Variants are **generated**, one per blog, from a worktree slug — N new hostnames
per worktree, not one.

Generation must be deterministic and **exact-host disjoint** from every
canonical in the map. Prefixing the leftmost label is sufficient
(`kodinpellervo.ddev.site` → `wt-a--kodinpellervo.ddev.site`) *provided matching
is on exact host equality, never string-suffix*. The previous revision's
common-suffix map produced `nat.herrfors.ddev.site` →
`nat.wt-a.herrfors.ddev.site` → `nat.wt-a.wt-a.herrfors.ddev.site`: the
double-port bug in a new costume.

**Startup validation, refuse to run on failure:**

- **matching is on exact origin equality**, never string-suffix and never
  substring. Containment between a canonical host and a variant host is
  *permitted* — §4.4's left-anchored origin tokens with a delimiter check make it
  safe, which is precisely what allows the leftmost-label prefix scheme to be
  used at all
- **assert the anchoring property directly:** for every (canonical, variant)
  pair, running the §4.4 automaton over the variant origin must yield **zero
  matches**. That is the invariant, not a substring ban
- the map is injective in both directions

Collisions with *other projects* on the machine are not hostshift's problem —
see §5.6.

Idempotency is then a property of exact-set membership, and must still be
asserted by test 7 rather than assumed.

### 5.5 Parsing hazards to handle explicitly

- **Protocol-relative `//host/path`** — Go's `net/url.Parse` accepts these and
  fills `Host` with an empty `Scheme`; do not treat an empty scheme as failure,
  or every one is silently missed
- **Explicit `:443` / `:80`** — must compare equal to the bare host and serialise
  without the default port
- **Never re-serialise through `net/url`.** `url.Parse` is for *comparison
  only* — lowercase the scheme, normalise the port, punycode the host, decide.
  The splice then replaces only the origin byte range in the original value;
  path, query and fragment are copied verbatim. `URL.String()` lowercases the
  scheme and percent-encodes (measured: `https://host/a b` → `.../a%20b`), which
  would silently break test 24
- **Case** — compare ASCII-case-insensitively (`ms-settings.php:62` lowercases)
- **IDN / punycode** — real for `.fi` client domains; compare on normalised
  punycode, preserve the original form on output
- **Percent-encoded hosts in query values** — `redirect_to=https%3A%2F%2F…`.
  **Decided in §5.1: rewrite them, symmetrically.** Variant→canonical on the
  request line and query, canonical→variant on `Location`. `pluggable.php:1715`
  then sees a `redirect_to` whose host equals `home_url()`'s and accepts it.
  Rewriting query origins is only wrong when done response-side alone. Test 19
  is the round trip
- **`data:` / base64 URIs** containing URLs — passthrough, stated as an accepted
  limitation
- **`Range` requests** — rewriting a partial body is incoherent, so **bypass the
  `Range`, not the rewriter**. This line originally said only "bypass", and M5
  read it the other way: `Range` was forwarded upstream and a 206 skipped every
  rewriter, so any client could turn the whole engine off by asking for
  `bytes=0-<len-1>` and read the document whole with its production origins
  intact. That is test 28, and unlike the self-redirect `Location` — which §7
  enumerates as exactly one carve-out — it was selectable by whoever was
  browsing. Whether a response is rewritable is not knowable until it arrives,
  and a 206 cannot be turned back into something rewritable without a second
  round trip, so `Range` and `If-Range` are stripped on the way out and
  `Accept-Ranges` is dropped on the way back. RFC 9110 lets a server ignore
  `Range`; nginx does not serve ranges for a PHP response, so on a page this
  costs nothing, and on a media file it costs a re-fetch from zero on a seek,
  over loopback.
- **`Vary`** — ensure nothing downstream caches a variant-specific body. It
  belongs on **every** response, not only the ones with a rewritten body:
  headers are rewritten for all of them, and a 302 whose `Location` now names a
  variant is precisely what a shared cache keyed on path alone — nginx
  `proxy_cache_key $uri`, a Varnish default with no host in the key — will hand
  to a browser sitting on a different variant, bouncing it out of its own
  worktree on every login and post-save redirect.

### 5.6 Upstream selection — resolved

"One instance serves every worktree" is **dropped**. It was never designable
without a machine-wide registry, which §3 forbids.

**One hostshift process per DDEV project**, run as a compose service on that
project's network with a single static `--upstream http://web:80`. No host-based
upstream selection, no registry, no cross-project failure coupling, one upstream
and one connection pool.

Machine-wide hostname routing is DDEV's router's job, and it already does it —
including the registration that a three-label variant host requires anyway, since
`wt-a--nat.herrfors.ddev.site` is not covered by the `*.ddev.site` wildcard and
needs an explicit SAN regardless of hostshift.

Registering variant hostnames in a *worktree's* `.ddev/config.yaml` is
per-worktree runtime config generated by the worktree tooling, not per-repo
installed footprint, so §3's constraint is not violated.

**Routing to the service is not automatic and is M6 packaging work.** DDEV's
router reaches the `web` service via `VIRTUAL_HOST` / `HTTP_EXPOSE` /
`HTTPS_EXPOSE` environment variables and `com.ddev.*` labels (see
`herrfors/.ddev/.ddev-docker-compose-full.yaml`). Pointing variant hostnames at a
*different* compose service requires the same variables and labels on the
hostshift service, plus the variant hosts in the worktree's
`additional_hostnames` so the mkcert SAN covers them. M2's tests 10 and 10a–10e
will hit this first.

Cost: N processes at a few MB RSS. Irrelevant.

### 5.7 Transport and packaging

Plain HTTP by default, TLS optional and off — behind DDEV's router, DDEV
terminates TLS. Compression is specified in §5.2: identity upstream, identity downstream by
default, `--compress` to opt in. Do not leak uncompressed responses to the
browser the way v0.2 does (`lib.sh:246-251`).

**Language: Go.** Decided 2026-08-27 after an empirical evaluation that
**reversed an earlier decision for Rust**. The reasoning is recorded because the
earlier argument was wrong in a way worth not repeating.

The Rust case was: *"Go has no lossless streaming HTML rewriter; `x/net/html`
round-trips lossily, so a Go implementation must hand-write one — a large job
whose defects are silent."* Every clause is false. The tokenizer **is** a
lossless streaming rewriter with a documented partition guarantee (§5.2); the
hand-written part is ~100 lines; and its defects are the loudest available,
because the identity-map test is five lines and runs in 35 ms over 5.9 MB.

What settled it was measuring lol-html rather than trusting its reputation. Its
passthrough of untouched input is real — 306 identity runs, byte-identical — but
it is **documented nowhere** a user would look, and it does not hold for a tag
that was modified. `Attributes::into_bytes` re-emits every attribute with
single-space separators and forces double quotes, so changing one attribute on
the real herrfors homepage **deleted 400 lines and 2,852 bytes of whitespace**
(5157 → 4757 lines; the Go splice yields 5157 → 5157). Cloudflare issue #214,
closed without fix. That is precisely the silent divergence Rust was chosen to
avoid.

Rust with `html5gum` instead of lol-html was evaluated as the strongest remaining
alternative and still loses: its per-attribute spans save ~60 lines, but its
`BTreeMap` drops duplicate attribute names — losing splice sites — it gives up
lol-html's namespace tracking, and it leaves both the proxy shell and JSON spans
hand-written.

Go also wins the two largest chunks of work outright:

- **The proxy shell is free.** `httputil.ReverseProxy` is ~987 lines of hardened
  stdlib: hop-by-hop stripping per RFC 9110 §7.6.1, `X-Forwarded-*`, protocol
  upgrades, flush intervals, cancellation. Mishandled `Content-Length` on a
  body-rewriting proxy is a request-smuggling class of bug — not plumbing worth
  hand-rolling for a dev tool.
- **JSON spans are solved.** `encoding/json/jsontext`: `ReadValue` returns "the
  exact bytes of the input" and `InputOffset` "the location of the next byte
  immediately after the most recently returned token or value", so
  `start = end - len(v)` holds by construction, with RFC 6901 paths and
  key/value discrimination. It ships in Go 1.25/1.26 **behind
  `GOEXPERIMENT=jsonv2`** — verified: a plain import fails with "build
  constraints exclude all Go files" — and is expected on by default in a later
  release. Until then use `github.com/go-json-experiment/json/jsontext`, which is
  the same code. The earlier revision listed this as "the one genuine gap …
  Go has the same problem" — it does not.

Dependency shape: stdlib `net/http` + `httputil`, `golang.org/x/net/html`,
`jsontext`, `golang.org/x/net/idna` for punycode (§5.5; same module as
`x/net/html`), stdlib `compress/gzip`, and a CLI library of choice.

For the origin automaton (§4.4) use `github.com/petar-dambovaliev/aho-corasick`
or `github.com/BobuSumisu/aho-corasick` — both return match **positions**, which
`--explain` and the sliding-window sweep require. **Do not use
`github.com/cloudflare/ahocorasick`:** its `Match` returns dictionary indices
only, takes a whole `[]byte`, and cannot stream.

Working proof is in `spike/`: `go/full/main.go` (130 non-blank lines — framing,
the 64-line span scanner, per-value splicing) and `go/e2e/main.go` (98 non-blank
lines — the proxy), with acceptance tests 1, 15, 24, 27 and 28 green, plus the
corpus and adversarial fixtures.

**That is the M1 starting point for the framing and splicing — do not rebuild
those. Its *matching* is a placeholder unanchored `bytes.ReplaceAll` and must be
replaced by §4.4's anchored origin automaton before anything else;
`rewriteValue` is the seam.** Note also that the spike only *counts* structured
surfaces (`srcset` et al.) rather than parsing them, and its `// idempotency
(test 7)` block discards its result — test 7 is **not** green in the spike.

**Required regardless of the above:** rewriting with an **identity map must
produce byte-identical output**. Run it over `spike/corpus` and `spike/adv` from
M1 onward. It is the invariant that catches every splice and offset defect, it is
five lines, and it runs in 35 ms over 5.9 MB.

#### `httputil.ReverseProxy` mechanics that must be got right

All verified against Go 1.26.5. Each is a silent failure if missed.

- Use **`Rewrite`, not `Director`** — setting both is an error ("must have
  exactly one of Director or Rewrite set").
- `ProxyRequest.SetURL` ends with `r.Out.Host = ""`. **Assign the canonical
  `Out.Host` after `SetURL`**, or the upstream sees the container's host and
  multisite resolution fails silently.
- `SetXForwarded` sets `X-Forwarded-Proto: http` whenever `r.In.TLS == nil`,
  which is always, since hostshift listens plain. **`Set` the `https` value
  after `SetXForwarded()`**, never before, or `wp-login.php` redirect-loops.
- **`Del("X-Forwarded-Port")` explicitly.** It is not hop-by-hop, so it is
  forwarded verbatim; §2.3 is the reason this matters.
- **`resp.ContentLength = -1` alongside `resp.Header.Del("Content-Length")`.**
  `flushInterval` keys off the struct field, not the header; leaving it stale
  disables periodic flushing.
- **Request bodies:** `http.Request.ContentLength` drives the transport, not the
  header. After rewriting a request body set `Out.ContentLength` to the new
  length (or `-1`), clear `Out.TransferEncoding`, and set `Out.GetBody` if
  retries matter — otherwise the transport errors or truncates.
- **`ModifyResponse` errors are recoverable; mid-body errors are not.** An error
  returned from `ModifyResponse` becomes a clean 502 via `ErrorHandler`, but a
  rewriter error surfacing during `copyResponse` can only
  `panic(http.ErrAbortHandler)` — headers are already sent. Test 14 covers the
  first class only.

Bind `0.0.0.0` **inside the container** with no published host port — `127.0.0.1`
is unreachable from the DDEV router. When run as a bare binary, bind `127.0.0.1`.
Never publish the port.

Distribution: static binary, distroless image, and a DDEV add-on that is a
compose service, the loopback override, and **one host command**. No `lib.sh`,
no hooks, no guard — nothing runs during a request.

The command exists because the binary refuses to. Deriving a slug from a git
branch, deciding which hostnames `web` should keep, writing `.ddev/.env` and the
generated `additional_hostnames`: all of that is opinionated setup, and a binary
carrying it is a tool for one shop. `hostshift` maps origins and *reads* a DDEV
project's declared hostnames as one source for that map; `ddev hostshift` does
the rest, and can one day be its own repo without the binary noticing.

---

### 5.8 Command-line design

The rewriter must be usable **without running a server**. That is what makes it
testable, pipeable, and CI-friendly.

```
hostshift rewrite   --from https://a --to https://b   < in.html > out.html
hostshift proxy     --upstream http://web:80 --listen 127.0.0.1:8080
hostshift map       # print the resolved host map (table, or --json)
hostshift check     # validate config; exit 2 if invalid
```

`rewrite` is the whole engine as a Unix filter, content type from `--type` or
sniffed. It collapses the corpus diff (§7) to one line:

```bash
curl -s https://canonical/page | hostshift rewrite --from … --to … \
  | diff - <(curl -s https://variant/page)
```

Conventions:

- **stdout is data, stderr is diagnostics.** Always, in every subcommand
- **Exit codes:** 0 success, 1 runtime error, 2 invalid configuration
- `--json` emits machine-readable counters and traces
- **No daemonising.** Run in the foreground; DDEV or a supervisor owns the
  lifecycle. `SIGHUP` reloads config, `SIGTERM` drains
- Config precedence: CLI flags → `hostshift.yaml` → DDEV defaults (§5.3)

**`--explain` is the answer to "why was this URL not rewritten?"** For every
candidate the Aho–Corasick prefilter matched that did *not* result in a rewrite,
emit the surface (`html-attr`, `json-string`, `header`, `inline-script`, `none`)
and the reason: `not-a-url`, `host-not-in-map`, `unparseable`,
`encoding-not-decodable`, `size-cap-exceeded`, `depth-limit`. Given how many
silent-failure modes this design has, that trace is the difference between a
five-minute diagnosis and an afternoon.

`--dry-run` on `proxy` serves responses unmodified while logging every rewrite it
would have made — safe to point at a live canonical checkout.

**Limits:** JSON is buffered, not streamed; cap at a configurable size (8 MB
default) and pass through untouched beyond it, logging the skip. The same cap
applies to **request** bodies, including multipart. HTML streams end to end.

## 6. Non-goals

Cron and phpunit (do not travel over HTTP). **WP-CLI is not in this list** — it is
regressed by production-canonical and needs the environment change in §4.3. Rewriting the database (that
is the alternative, §4). Replacing DDEV (attempted and abandoned, §2.1).
Worktree lifecycle management. Production use. `Range` responses. URLs inside
`data:` URIs. URLs built by JS string concatenation at runtime.

---

## 7. Acceptance tests

*(Numbering has a gap at 6: the XML/RSS test was removed with §5.2 Tier 3.
Numbers are stable identifiers — do not renumber.)*

1. `Location` on a login redirect
2. `Set-Cookie` `Domain=` — on a site that sets `COOKIE_DOMAIN`; not herrfors
3. `srcset` with width descriptors and commas
4. JSON-escaped `https:\/\/C\/` in a REST response
5. `wpApiSettings` in an inline `<script>` (JS statement, unescaped slashes)
7. **Idempotency fixed point** — proxy output re-fed through the proxy is unchanged
8. Third-party host untouched
9. gzip upstream decoded correctly; unsupported encodings passed through
10. Multisite sibling blog: **request** `nat.V` lands on the `nat.C` blog, and
    its response URLs come back as `nat.V`
10a. A 5-blog site (pellervo) maps all five, with unrelated registrable domains
10b. Cross-blog link in content: blog 1 linking to blog 2's canonical is
     rewritten to blog 2's *variant*, not blog 1's
10c. Residual `@production` URL in a `@ddev` database is rewritten to the variant
10d. A config whose canonical sets overlap between sites is rejected at startup
10e. Bare DDEV defaults (name + additional_hostnames, no hostshift.yaml) produce
     a working map for a 2-blog site
11. `Link` REST discovery header
12. Binary passthrough byte-identical
13. Large streamed HTML buffers **at most one token** — assert peak RSS is
    bounded by O(largest token), not by response size, over a 5 MB page.
    `Tokenizer.SetMaxBuf(n)` caps it; note that exceeding the cap yields
    `ErrBufferExceeded` mid-body, which per §5.7 cannot become an error response
    once headers are sent
14. Upstream 5xx / connection failure surfaced, not swallowed
15. A rewritten response carries no stale `Content-Length` and no upstream `ETag`
16. A request `Host` absent from the map is rejected with **421**, never proxied
17. **Suffix-overlapping host sets rejected at startup** — but read this and 29c
    against §5.4, which they contradict as originally worded. Containment is
    *permitted*: `wt-a--herrfors.ddev.site` contains `herrfors.ddev.site` and the
    whole leftmost-label scheme depends on that being legal. What is rejected is
    a map in which the automaton **matches** a variant origin, so a second pass
    would rewrite again. Assert both halves — the containing map is accepted, the
    matched one is refused
18. `<base href>` rewritten or stripped
19. Full wp-admin login round trip, including `redirect_to` and `wp_get_referer()`
20. `FORCE_SSL_ADMIN` site does not redirect-loop
21. Protocol-relative `//C/path` rewritten
22. `content.rendered` HTML-in-JSON rewritten
23. CSP header rewritten
24. Identity-map byte-identity: with canonical == variant, output equals input
    byte for byte. Where URLs did change, output is byte-identical **everywhere
    else** — splicing never re-serialises, so this holds universally, including
    inside modified start tags (attribute order, quoting and whitespace all
    survive). Run over `spike/corpus` and `spike/adv`
25. A response whose `Content-Type` is outside the rewritable set never enters a
    rewriter — proven by a per-surface counter of zero
26. Undecodable content-encoding is passed through byte-identical and logged
27. `hostshift rewrite` as a filter produces the same bytes as the proxy path
28. **No dereferenceable production origin reaches the browser.** Extract every
    URL-valued position — HTML attribute values, CSS `url()`, JSON strings,
    JSON-LD, header values — and assert none has an origin in the canonical set.
    Bare hostnames in prose are explicitly out of scope and must **not** be
    rewritten. Safety-critical.
    **One enumerated carve-out (§4.4):** a 3xx whose `Location` the self-redirect
    guard passed through. Assert the carve-out is *enumerated and counted*, not
    merely tolerated, and that `--strict-origins` empties it
29. Straggler sweep: a URL in a deliberately unhandled position is caught,
    rewritten, and reported — and running the sweep twice is a fixed point
29c. Substring-overlapping canonical/variant sets are rejected at startup — with
     the same correction as test 17: it is *matching*, not containment, that is
     refused. An identity map (variant == its own canonical) is a legal, no-op
     configuration and must not be rejected, or test 24 has nothing to run on
29d. `ddev wp option get home` and `ddev wp site list` resolve correctly on a
     multisite repo with an unrewritten production database
29a. Server-side loopback (Site Health, an internal REST request) resolves
     locally, never reaching production — asserted by observing that no request
     for a mapped production host leaves the machine during a full crawl
29b. Loopback TLS verification **fails** against the mapped production hostname
     from inside the web container (§4.4 measures this and accepts it), and the
     failure is confined to the two documented surfaces: `wp_safe_remote_get`
     link-preview and internal oEmbed. `sslverify => false` callers — cron, Site
     Health — succeed. Assert both halves
30. Edit round trip: save a post containing an internal link through wp-admin,
    and assert the **database** holds the canonical (production) URL, not the
    variant
31. A REST write (`POST /wp-json/wp/v2/posts`) with a variant URL in the body is
    stored canonical
32. **Self-redirect guard (§4.4).** A missing `/app/uploads/` file behind the
    fleet's `redirect-uploads.conf` reaches a remote `Location` in a bounded
    number of hops and never loops, counted as `self-redirect-passthrough`.
    Blog 1 takes one hop; a **non-primary blog takes two**, because the conf
    hardcodes blog 1's origin — cover one, and assert the bound rather than the
    count. Under `--strict-origins` the same request returns 404 and no canonical
    origin leaves. Assert both halves, and assert that a 3xx whose `Location`
    differs from the incoming request URL is still rewritten normally (a login
    redirect, test 1, must not be caught by the guard)

**Corpus diff — the only test that validates against reality.** Crawl N URLs on
canonical and the same N through the proxy, normalise host, assert DOM
equality. Fixtures would not have caught the double-port bug; this would.

---

## 8. Milestones

**M0 — pre-flight (§4.5). Done 2026-08-27.** Usage survey and uploads-sync check,
both recorded in §4.5 and `docs/m0-preflight.md`. `.ddev/docker-compose.hostshift.yaml`
re-created from the template now in `ddev/` and verified merged onto the `web` service via
`ddev debug compose-config`. Found the `redirect-uploads.conf` redirect loop
(§4.4, test 32) and corrected five §4.2 claims. *No code.*

**M1 — proxy shell, observability, and the identity invariant. Done 2026-08-27.**
`httputil.ReverseProxy` skeleton with every §5.7 mechanic asserted; `hostshift
rewrite` filter mode; `--dry-run` and `--explain` (§5.8); per-surface counters.
The `bytes.ReplaceAll` placeholder is replaced by §4.4's anchored origin
automaton, the `Raw()` aliasing is fixed, and the span scanner's
`ValueStart`/`ValueEnd` assertion is landed over all 37,280 attribute values.
Tests 24 and 27 green, and 7, 8, 12, 14, 15, 25 came free. `spike/` is superseded
and kept only as evidence.

One library gotcha worth not rediscovering: `petar-dambovaliev/aho-corasick`'s
`findIter` advances with `pos = end - len + 1` — one byte past the match
*start*, not its end — so it yields overlapping matches even under
`LeftMostLongestMatch`. Non-overlapping semantics have to be recovered by the
caller; without that, `https://h` is matched and then `//h` again six bytes
inside it.

**M2 — host map, request direction, and request bodies. Done 2026-08-27.** Config
layering (DDEV defaults + `hostshift.yaml` + flags), variant derivation, startup
validation (§5.3–§5.4), then §5.1 in full: multisite inverse mapping, `Referer`,
`Origin`, `X-Forwarded-Proto`, request line and query, request bodies including a
splice-based multipart rewriter, `Location`/`Link`/CSP, `Set-Cookie` `Domain=`,
§4.4's self-redirect guard, and `wp-cli.local.yml` generation.

Tests 1, 2, 8, 10, 10a–10e, 11, 15, 16, 17, 20, 23, 29c, 32 green, plus the
request halves of 19, 30 and 31.

**29a, 29b and 29d are deferred to M6**, and were mis-scheduled here. All three
need a live DDEV project running against an unrewritten production database —
which is precisely the "done when" criterion M6 exists to prove. Nothing in M2's
code blocks them; they cannot be asserted before the pilot stands up.

**M3 — HTML. Done 2026-08-27.** Every-attribute scan, every raw-text element,
`<base href>`, the §4.4 straggler sweep as a streaming backstop, and the
trailing-root-dot fix M0 found. Tests 3, 5, 7, 12, 18, 19, 21, 25, 28, 29 green.

The structured attributes turned out to need no parsers (§5.2), and the
foreign-content gap turned out to be wider than described and was closed rather
than delegated to the sweep. Test 28 runs over the corpus with an extractor that
does not share code with the matcher: 51 documents, 10,271 URL-valued positions,
zero canonical origins.

**M4 — structured bodies. Done 2026-08-27.** JSON responses and the `jsontext`
span-scanner, with `application/json` and `application/*+json` added to the
rewritable set — until M4 the REST API was passing canonical origins straight to
the browser. Tests 4, 22 green; 30 and 31 green as far as the proxy can carry
them (see below). XML and standalone CSS/JS are **not** built — §5.2 Tiers 2 and 3.

`GOEXPERIMENT=jsonv2` is confirmed still required on Go 1.26.5 — a plain import
of `encoding/json/jsontext` fails with "build constraints exclude all Go files".
Requiring an environment variable to build a distributed binary is not a trade
worth making, so `github.com/go-json-experiment/json/jsontext` is used, exactly
as §5.7 anticipated. Swap the import when the standard library one lands.

**HTML-in-JSON needs no HTML rewriter — but it does need an escape carve-out.**
§5.2 says `content.rendered` "is a full HTML blob a URL-only rule skips" and that
the value should be "decoded, rewritten, re-encoded and spliced back". That was
written against an allowlist of URL-valued keys. With the anchored automaton
running over the *raw, still-escaped* bytes of every string value, the origins
inside `content.rendered` are found without any of it — they appear as
`https:\/\/host\/…` and the JSON-escaped form is in the token set. Decoding and
re-encoding every value would be strictly worse: re-encoding is
re-serialisation, and §5.2's core property is that output is byte-identical
everywhere a rewrite did not occur. The depth-2 recursion limit is unnecessary
and is not built.

The first draft of this paragraph went further and said such a value "must not"
be decoded at all. That is wrong, and three real spellings show why — each one a
dereferenceable production origin reaching the browser, i.e. test 28:

- **`\uXXXX`.** PHP's `json_encode` escapes every non-ASCII rune unless
  `JSON_UNESCAPED_UNICODE` is passed, and `wp_json_encode` does not pass it. So
  an IDN client site — §5.5 calls those real for `.fi` — serves
  `"https:\/\/hämeen.fi\/x"`, which no raw-byte scan can see. The page
  rewrites and the REST API does not, so Gutenberg and every JS fetch get
  production URLs.
- **HTML character references** inside `content.rendered`, the class §5.3 closes
  for attribute values. The identical post body is clean as `text/html` and
  leaks as `application/json`.
- **Double-escaped JSON-in-JSON**, `"https:\\/\\/host"`, from a block attribute
  holding JSON that is itself serialised into JSON.

So a string is unquoted, entity-decoded and re-matched, and re-encoded **only**
when that finds an origin the raw pass did not. A document that is already
correct never takes the path, byte-identity under an identity map is untouched,
and it is counted under its own surface, `json-escape`, because a non-zero count
means content is storing origins in a form §5.3 does not model.

**The JSON path gets §4.4's straggler sweep too.** M4 added a whole response
surface without one, so every miss on it was silent — the same post body clean
as `text/html` and carrying production as `application/json`, with no WARN and
no non-zero counter. JSON is buffered anyway, so the sweep needs none of the
streaming machinery: no carry-over window, no left-context byte, no offset map.

**A body that does not parse is passed through, and *reported*.** Leaving it
alone is right — half-rewritten JSON is worse than unrewritten JSON — but the
first implementation folded each value's events into the counters as it scanned,
so a decoder error part way through returned the original bytes while the
counters kept rewrites that had been undone. A duplicate object member is legal
JSON that `jsontext` rejects by default; it printed two production origins under
`"rewrites": {"json-string": 1}`, `"skips": {}`, exit 0. Events are held back
until the document parses, and the pass-through logs and counts a
`encoding-not-decodable` skip — the same treatment the size cap already got.

What the span scanner *does* earn is key/value discrimination — an origin in a
JSON key is not a link and is left alone — and an RFC 6901 path in `--explain`,
which is the difference between "something in this 200-URL REST response leaked"
and `/_links/self/0/href`.

**Tests 30 and 31 are split.** The proxy half is asserted now, against an
upstream that stores exactly the bytes it receives: a Gutenberg save carrying
variant URLs is stored canonical, and reading it back returns the variant. The
database half — asserting against real `wp_posts` rows — needs a live WordPress
and lands with the M6 pilot.

**M5 — transport. Done 2026-08-27.** Compression, streaming bounds, error
surfacing, `Vary`, `Range`. Tests 9, 13, 14, 26 green.

Test 13 measured: a 5 MB response streams with a **42-byte** peak token buffer.
The bound is asserted directly rather than through heap sampling, which GC timing
makes unreliable.

§7's note that `ErrBufferExceeded` "cannot become an error response once headers
are sent" is resolved by **not making it an error at all**. A token larger than
the cap makes the remainder of that response stream through unparsed, logged and
counted. Aborting the connection would be a worse answer than a page with one
unparsed region — and because §4.4's sweep sits *downstream* of the tokenizer,
origins in the passthrough tail are still caught, so nothing leaks. That is a
property worth stating explicitly: the sweep is what makes a bounded parser safe.

**The passthrough must start at `Raw()`, not at `Buffered()`.** x/net/html's
`readByte` advances `raw.end` *before* it tests `maxBuf`, so at
`ErrBufferExceeded` the oversized token is sitting in `Raw()` and `Buffered()`
holds only read-ahead. A text token is handed back as a partial `TextToken`
first, which is why text, `<script>` and comments survived; a *tag* errors from
inside `readStartTag` with its bytes still in `Raw()`. Emitting `Buffered()`
alone therefore deleted exactly `MaxToken` bytes — at the shipped 4 MiB default,
a 5 MB page arriving 4 MiB short with status 200 and no `Content-Length` to
check it against, the opening `<img src="data:image/png;base64,` gone and the
rest of its value rendered as visible page text. An inlined LCP image or a
multi-MB Elementor `data-settings` attribute is all it takes, and it broke test
24, the guard rail that is supposed to make this class of bug impossible.

The tail also bypasses the offset map §4.4 needs, so a final mark is recorded
when it starts; without it every straggler reported after the cap was given an
output offset, 4 MiB adrift on the page above.

One gzip detail worth not rediscovering: `gzip.NewReader` consumes the header
before it can fail, so a body labelled gzip that is not gzip loses however many
bytes the header check read — the whole body, when it is short. The head is
captured and put back.

**M6 — packaging + pilot. Packaging done 2026-08-27; pilot pending.**

Done: static binaries for linux and darwin on both architectures (~7 MB each), a
distroless image, the DDEV add-on — two compose files and an `install.yaml`, with
a test asserting it stays that way — and `hostshift diff`, the corpus diff.

The corpus diff is green against a local canonical site and the same site through
the proxy: **16 pages, 16 byte-identical, 0 leaks, 0 errors**, with line counts
preserved exactly. It reports byte equality, but the assertions that fail a run
are the two that cannot be innocent on a live site — a canonical origin reaching
the browser, and a page whose line count changed, which means something
re-serialised.

**Piloted against the running `herrfors` project**, read-only, with
`canonical = the ddev hosts` (the map `.ddev/config.yaml` alone produces).
**20 pages, 0 leaks, 0 errors, every line count identical.** Full write-up in
`docs/m6-pilot.md`; one page byte-identical and the other nineteen differing
only in CSP nonces, 158 of them, with equal byte counts.

The pilot paid for itself immediately: it found 25 stragglers per crawl, and
neither kind was a defect in the sweep. `sage-cachetags` emits a URL in an HTML
comment on every cached page, and a privacy-policy paragraph quotes its own URL
in visible prose. Both are now scanned in the structured pass, per §4.4's rule
that every straggler is a bug to fix there. The prose case matters more than it
looks: under production-canonical a visible URL pointing at production is exactly
the hazard §4.4 opens with, since a developer copy-pastes it and lands on live
production. After the change the sweep catches **zero** on real pages, which is
what a backstop should do.

**Pilot done, on canonical `herrfors` against an unrewritten production
database.** `ddev snapshot pre-hostshift-pilot` first; full write-up in
`docs/m6-pilot.md`.

`wp_blogs.domain` holds `www.herrfors.fi` and `www.herrforsnat.fi`, exactly as
production has them, and nothing rewrote it. **Tests 29a, 29b and 29d pass, and
the corpus diff is green: 20 pages, 0 leaks, 0 errors, 0 stragglers, every line
count identical.** pellervo's five blogs are green too — 12 pages, 0 leaks,
including a real JPEG returned byte-identical through the proxy.

Both halves of §8's "done when" corpus criterion are met. What remains is
installing the add-on into a project and starting it, which the pilot ran around
by driving `hostshift proxy` directly.

One artifact worth knowing about, unrelated to hostshift: **every retained
`db:pull` dump on this box begins with seven lines of PHP warnings** from
`config/wp-cli/pre-ssh.php`, before the MariaDB header. `ddev import-db` fails on
them, and it drops the database before it fails. That is a `db:pull` bug in the
Genero repos, not this one, but it is the reason the snapshot mattered.

This is not a small project. The previous revision's *"the tool is
straightforward once these are green"* is the sentence most likely to mislead an
implementing agent. Delete that expectation.

**Relationship to `generoi/ddev-worktree` — decided 2026-08-28: hostshift.**

The two are different answers to one question. `generoi-worktree` gives each
worktree its own database and reaches it on a port, `:808x` behind Caddy;
hostshift shares one database and gives each worktree a hostname. M6 proved the
second, and the choice is made: hostshift.

The difference that decided it is the database. A forked database per worktree
is a `db:pull` apiece — 548 MB on herrfors — and it diverges from the moment it
is taken, so a worktree is testing against data the site no longer has. Sharing
one means a worktree is testing the real thing, and the price is the hostname
mapping this document is about. Ports also make the *browser* the thing that
knows which worktree it is on, which absolute URLs in a database defeat: WordPress
redirects to the hostname it was told, and the port is not part of it.

Note that `wt-up`, `wt-guard` and `wt-wp` were already removed in v0.2 per its
README (though `install.yaml` still ships `wt-up` and `wt-wp`), and no
`sunrise.php` generation exists anywhere in the repo — do not plan to remove
things that are already gone. The add-on itself is one repo, installed in one
project, and worth archiving rather than deleting: it is the record of what was
tried.

---

## 8a. Day-one setup

**Done in M0.** `hostshift/` is a git repository, `generoi/hostshift`, **private**
— PLAN.md carries client domains, a production IP and fleet analysis, so it does
not start public the way `generoi/ddev-worktree` did. Root `README.md` and
`.gitignore` exist; there is still no root `go.mod` and no LICENSE.

**Left for M1**, because this plan does not decide them: the module path, and a
**minimum Go version** (`spike/go/go.mod` pins `go 1.26.5`, a patch-level
directive that refuses to build on 1.26.4 — use `go 1.26`).

**Toolchain note.** There is no `go` on `PATH` on this machine. Go 1.26.5 is in
the nix store at
`/nix/store/4im44k446822ixjai0mdaizqskc90qxr-go-1.26.5/bin/go` — the spike was
run through it. M1 needs Go on `PATH` (nix profile, `devShell`, or a `flake.nix`
in this repo) before `go build` works from a plain shell.

`spike/README.md` needs the same statistics corrections as §5.2 and does not yet
document `go/main.go`, `go/rewriter/main.go` or `go/svg/main.go`.

## 9. Risks

- **Fleet-wide coupling — the largest unstated risk.** Deleting `search-replace`
  from `db:pull` makes hostshift a hard runtime dependency for every developer on
  all 63 repos, on day one, for an unproven hand-written proxy. Without it
  running, a freshly pulled site loads live production assets into the dev
  browser (they *resolve* — M0 measured 95.2% of the *distinct* upload URLs
  referenced by herrfors' content as absent locally, §4.5), and an admin action
  can write to production.
  **Resolved 2026-08-28: the flip is not happening, at least not now.**
  `db:pull` keeps its search-replace and the main site keeps a `.ddev.site`
  database. hostshift is deployed for **worktrees**, where the whole point is
  that there is no second database to rewrite — so nothing about a normal pull
  changes and nobody acquires a runtime dependency they did not ask for.
  Production-canonical remains supported and piloted (herrfors, pellervo,
  both green), opt-in per repo via `hostshift.yaml`. This retires the risk
  rather than staging it: the fleet-wide coupling only ever existed because of
  the flip
- A documented **fallback to search-replace per site** must remain supported
- Performance: immaterial for dev, and the Aho–Corasick prefilter means most
  responses are never parsed at all
- ~~The tokenizer does not track foreign content (SVG/MathML), so URLs inside
  `<svg>` subtrees are missed and caught only by the §4.4 sweep. Quantify on the
  corpus during M3~~ — **retired in M3.** Quantified and then closed: scanning
  every raw-text element handles it in the structured pass, and the sweep now
  catches zero across the corpus (§5.2)
- HTTP/2 and websockets: passthrough behaviour undefined
- Every silent-failure mode above (C1-class especially) argues for M1 first

---

## 10. Prior art

The closest correct implementations are in **web archiving**, not dev tooling.
pywb and the Wayback Machine do content-type-aware URL rewriting of HTML, CSS,
JS and JSON at scale — replaying an archived site is the same problem. Read their
rewriters first.

Nothing packages that as a small development proxy, which is why every WordPress
shop reaches for `search-replace`. The problem is not WordPress-specific — any
CMS with absolute URLs in the DB has it — so the engine should be CMS-agnostic
with WordPress as one discovery adapter and the acceptance bar.

---

## 11. Sources

- `generoi/ddev-worktree` — git log, `generoi-worktree/lib.sh`, README, `install.yaml`
- Cursor session *"Leaner WordPress Dev"*, 2026-08-21, 286 turns,
  `~/.cursor/chats/1ddf8c12…/25fa7c51-…/store.db`
- Claude Code transcripts, `~/.claude/projects`, 2026-06-20 → 08-26
- WordPress core in `~/Projects/Genero/herrfors/web/wp`
- `herrfors/config/application.php`, `herrfors/database.@production.2026-06-09-162529.sql`
- Direct measurement of `~/Projects/Genero`, 2026-08-27
- Adversarial audit of revision 1, 2026-08-27
