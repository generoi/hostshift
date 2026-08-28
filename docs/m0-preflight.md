# M0 — pre-flight

Run 2026-08-27. No code, per PLAN §8. Three deliverables: the two §4.5 checks,
and re-creating the `.ddev/docker-compose.hostshift.yaml` loopback override that
was deleted at the end of the §4.4 experiment.

It found one hazard the plan did not anticipate and corrected five claims in
§4.2. Those corrections are applied to `PLAN.md` in this same change; this
document holds the method and the raw numbers behind them.

---

## 1. Usage survey (§4.5)

**Question.** §4.5: "1,373 of 1,413 subagent spawns need no environment at all,
and the 40 that isolate are all in a project showing 0.0% parallel-session time.
Confirm the population this serves is non-trivial."

**Method, and its limit.** Transcript *contents* are unreadable in this session —
`grep` over `~/.claude/projects/**/*.jsonl` is refused by the auto-mode
classifier. So the subagent-spawn and `git worktree`-invocation counts §3 and
§4.5 quote could not be re-derived, and are taken on trust here.

What filesystem metadata *does* support is a better-targeted metric anyway.
Session **start** is a point event with an exact timestamp (inode birth time,
reliable on APFS). Session **end** is not: mtime tracks the last append, and a
resumed session shows a multi-day span, so `[birth, mtime]` overstates "open" by
orders of magnitude and cannot be used for occupancy. The metric used instead is
**clustered starts** — two sessions begun in the same repo within 30 minutes —
which is what "two agents needing a browsable environment at once" looks like
from outside.

Caveat: a `.jsonl` is a session transcript, and subagent transcripts cannot be
distinguished from top-level ones without reading content. If anything this
*inflates* the concurrency numbers below.

**Result.**

| | |
|---|---|
| DDEV repos (`.ddev/config.yaml`) | 63 |
| …with any Claude Code history | **31** |
| Sessions in DDEV repos | **113** of 5,826 total (**1.9%**) |
| Repos ever reaching 2 concurrent starts | **7** |
| Clustered start-pairs, entire history | **17** |
| Worktree-shaped session directories | **1** (`suomentyokalu`) |
| Worktree checkouts on disk | **1** (`herrfors-wt-pilot` — the pilot itself) |

Per repo, where any clustering occurred:

```
suomentyokalu    sessions=22  peak_in_window=3  clustered_pairs=6
kaskipuu         sessions=9   peak_in_window=3  clustered_pairs=6
solarplexius     sessions=11  peak_in_window=2  clustered_pairs=1
snellmanrecipes  sessions=6   peak_in_window=2  clustered_pairs=1
snellmangroup    sessions=5   peak_in_window=2  clustered_pairs=1
snellmanecom     sessions=6   peak_in_window=2  clustered_pairs=1
holmasto         sessions=6   peak_in_window=2  clustered_pairs=1
```

**Reading.** The check does not pass on the terms §4.5 set. Parallel agents in
worktrees is a real pattern but a small one — 17 clustered pairs across the whole
history, one worktree on disk. It must not be the headline justification, and
§3's observation that agents overwhelmingly share one directory is confirmed
rather than overturned.

**The project is justified by the other half of §4.3.** Counting the `db:pull`
artifacts that survive on this box:

| | |
|---|---|
| Retained `database.@*.sql` / `wp-sync.sql` dumps | **19** |
| Total pulled-database bytes on disk | **9.6 GB** |
| Span | 2025-08-08 → 2026-08-27 |
| Multisite repos (search-replace is `--precise` across N pairs) | **12**, N from 2 to 9 |

Retained dumps are a floor, not a count — they get overwritten and deleted. Every
one represents a full-DB multisite `--precise` search-replace that
production-canonical deletes outright, for every developer, on every pull,
needing no worktree at all. That population is not trivial and it is the one to
build for.

This does not change the design. It changes what the design is sold on, and it
makes §9's staging plan (`db:pull`'s search-replace stays the default through M6)
the right shape rather than merely a cautious one.

## 2. Uploads (§4.5)

**Question.** With production URLs in content, `https://www.herrfors.fi/app/uploads/…`
rewrites to the variant host and must resolve against locally synced uploads.
Verify the sync covers what content references.

**Method.** Extract every absolute `/app/uploads/` reference to a canonical host
from `herrfors/database.@production.2026-06-09-162529.sql` (524 MB, the pristine
production dump), in raw and JSON-escaped form; unique them; test each for
existence under `web/app/uploads/`.

**Result — it does not.**

| | |
|---|---|
| Occurrences of canonical `/app/uploads/` URLs | 122,179 |
| Distinct upload URLs referenced | 2,661 |
| Present locally | **162** |
| **Missing locally** | **2,499 (94%)** |

The local tree is 137 files, 868 KB: hand-added SVGs, plus an empty directory
skeleton (`uploads/.github/workflows`, `uploads/config/`, `uploads/web/app/`)
left by a mis-targeted rsync. `web/app/uploads/*` is fully gitignored and
`robo files:pull` has evidently never completed on this checkout.

Missing uploads are **not** a regression hostshift introduces — under the status
quo the same 2,499 files are absent. The finding matters for two other reasons,
both recorded in §4.5: it makes the redirect hazard in §3 below load-bearing
rather than theoretical, and it puts a number on §9 (on a production-canonical
database with hostshift not running, 94% of herrfors media loads from live
production).

Syncing uploads is `robo files:pull`'s job and is explicitly not a hostshift
deliverable.

### What the dump also settles about the origin automaton

Origin *forms* actually present in a production database, per host:

```
www.herrfors.fi        https=15155  http=2    pct-enc=6   json-esc=0
www.herrforsnat.fi     https=18091  http=138  pct-enc=38  json-esc=0
herrfors.ddev.site     https=713    http=0
nat.herrfors.ddev.site https=0      http=165
herrforsnat.genero-dev.com https=574
nat.herrfors.kinsta.cloud  https=260
```

Four things follow, all confirming §4.4/§5.5 with measurements rather than
argument:

- **Both schemes are required.** `nat.herrfors.ddev.site` appears **only** over
  `http://` (165, zero `https://`). A host-keyed map would be wrong; §5.3's
  origin→origin map is right.
- **Percent-encoded origins are real**, 46 occurrences of `https%3A%2F%2F…`.
- **JSON-escaped origins are zero in the database** — but that is where you
  cannot measure them. `https:\/\/` is produced at render time by the REST API,
  not stored. Do not conclude from this that §5.2's JSON handling is unnecessary.
- **`robo.yml` is not the whole canonical set.** `nat.herrfors.kinsta.cloud`
  occurs 260 times and appears in no environment in herrfors' `robo.yml`. The
  adapter (§4.2) emits what `robo.yml` declares; anything else in content is
  simply not rewritten and leaks. Acceptable — that host no longer resolves — but
  the straggler sweep will not report it either, because it is not in the
  canonical set. Worth knowing before reading a clean sweep as proof of coverage.

Two anchoring hazards also turned up, both small in count and both real:

- `https://www.herrfors.fi.` and `https://www.herrforsnat.fi.` — **trailing-dot
  FQDN form**, 5 occurrences. `.` is not in §4.4's delimiter set, so these do not
  match and are not rewritten; a browser resolves them to production. A **test 28
  violation** unless the trailing dot is normalised away when comparing hosts.
- `http://herrfors.fi` and `https://www.herrforsverkko.fi` — **apex and sibling
  hosts not in the map**, 5 occurrences. Production redirects apex → `www`, so
  these are dereferenceable production origins too. The config must be able to
  carry apex aliases; §5.3's `aliases:` list is the place.

Both belong to M3 (§5.5 comparison rules and the sweep), not M0.

## 3. The hazard M0 found: the fleet's uploads redirect loops

**53 of 63 DDEV repos (84%)** ship a committed nginx snippet that 302-redirects
any missing `/app/uploads/` request to a hardcoded **production** origin —
`.ddev/nginx/redirect-uploads.conf` in most, `uploads-redirect.conf` in three
(`ekorosk`, `familjen-snellman-sweden`, `generogrowth`), folded into
`nginx_full/nginx-site.conf` in three more (`niva`, `snellmanecom`,
`solarplexius`).

Under hostshift, that 302's `Location` is Tier 1 and gets rewritten back to the
variant host — which re-requests the same missing file. `ERR_TOO_MANY_REDIRECTS`,
on 94% of media requests, on 84% of the fleet. Today it works, because no proxy
is in the path and the redirect simply leaves for production.

Full analysis and the decision — a counted self-redirect guard, with
`--strict-origins` to turn it into a 404 — are in PLAN §4.4, with acceptance
test 32 and a carve-out written into test 28. It lands in M2, where `Location`
rewriting lands.

## 4. Loopback override re-created

`herrfors/.ddev/docker-compose.hostshift.yaml`, from
[`templates/docker-compose.hostshift.yaml`](../templates/docker-compose.hostshift.yaml).
Verified merged onto the `web` service without starting the project:

```
$ ddev debug compose-config | grep -A2 extra_hosts
    extra_hosts:
      - www.herrfors.fi=127.0.0.1
      - www.herrforsnat.fi=127.0.0.1
```

§4.4 already verified the mechanism end to end (both schemes stay on the box,
mkcert SAN carries no production name); that was not re-run.

**One footprint problem for M6.** The file is *not* gitignored — `.ddev/.gitignore`
is `#ddev-generated` and enumerates only DDEV's own outputs, and sibling
`docker-compose.phpmyadmin.yaml` is tracked. So it shows as untracked in every
`git status` in the site repo, which is the pressure that gets things committed
(§3). The add-on must handle this; the same problem does not affect
`wp-cli.local.yml`, which is already gitignored in 53 of 60 repos.

## 5. Claims checked and confirmed

Verified unchanged, so M2 can build on them:

- **`wp-cli.local.yml` (§4.3).** Gitignored in exactly **53 of 60** repos with a
  `wp-cli.yml`, and the 7 exceptions are exactly the ones named: `ekorosk`,
  `fsi`, `kokoomus`, `niva`, `panini`, `snellman-group`, `vendoprint`.
- **No repo has a root-level `url:`** in `wp-cli.yml` — **0 of 60**, as §4.3
  states. The generated override has nothing to conflict with.
- **`snellmanecom`** `@kinsta` has 2 URLs against 5 elsewhere.
- **`steripolarnew`** carries `@legacy-staging` / `@legacy-production`, all with
  equal 4-entry lists.

## 6. Claims corrected

Applied to `PLAN.md` §4.2 in this change.

| §4.2 said | Measured |
|---|---|
| `fsi` is `multisite: true` with no `url` lists at all | **False.** Complete index-aligned **9**-entry lists for `@ddev`, `@staging`, `@production`. The largest multisite in the fleet, across 7 registrable domains |
| `suomentyokalu` `@staging` empty | **False.** Aligned at 2 |
| `spfpension` `@staging` has 1 URL against 4 | **0**, not 1 |
| "N from 2 to 5" | N from **2 to 9** |
| table of multisite repos | omitted `fsi` (9) and `steripolar` (3) |

One further correction, not a claim §4.2 made but an assumption it invites: the
environment set is **not** `{@ddev, @staging, @production}`. `@vagrant` (beamex,
suomentyokalu), `@dev` (mutti), `@kinsta` (snellmanecom) and `@legacy-*`
(steripolarnew) all occur. The adapter must enumerate, not assume.

Full per-repo audit:

```
beamex         @ddev=2 @vagrant=2 @staging=2 @production=2
fluo           @ddev=3 @staging=3 @production=3
fsi            @ddev=9 @staging=9 @production=9
herrfors       @ddev=2 @staging=2 @production=2
mutti          @ddev=2 @dev=2 @staging=2 @production=2
panini         @ddev=2 @staging=2 @production=2
pellervo       @ddev=5 @staging=5 @production=5
snellmanecom   @ddev=5 @staging=5 @production=5 @kinsta=2
spfpension     @ddev=4 @staging=0 @production=4
steripolar     @ddev=3 @staging=3 @production=3
steripolarnew  @ddev=4 @production=4 @staging=4 @legacy-staging=4 @legacy-production=4
suomentyokalu  @ddev=2 @vagrant=2 @staging=2 @production=2
```

`fsi` is the hardest case in the fleet and a better M6 pilot than the plan's
herrfors (2) + pellervo (5): nine blogs, seven registrable domains, and an
`@staging` list that is `http://` where every other environment is `https://`.
§8's "done when" is left as written; this is a note, not a change.

## 7. Toolchain

There is **no `go` on `PATH`**. Go 1.26.5 exists at
`/nix/store/4im44k446822ixjai0mdaizqskc90qxr-go-1.26.5/bin/go`, which is how the
spike was run. M1 must put Go on `PATH` — nix profile, devShell, or a `flake.nix`
in this repo — before `go build` works from a plain shell. Recorded in §8a.
