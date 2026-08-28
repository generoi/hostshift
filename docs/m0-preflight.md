# M0 — pre-flight

Run 2026-08-27. No code, per PLAN §8. Three deliverables: the two §4.5 checks,
and re-creating the `.ddev/docker-compose.hostshift.yaml` loopback override that
was deleted at the end of the §4.4 experiment.

It found one hazard the plan did not anticipate and corrected four claims in
§4.2. Those corrections are applied to `PLAN.md` in this same change; this
document holds the method and the raw numbers behind them.

**A review pass corrected this document in turn.** Three of its own measurements
were wrong, two of them badly enough to invert a conclusion, and one had
"corrected" `PLAN.md` into being less accurate than before. Those are marked in
place rather than quietly rewritten, because how the numbers were got wrong is
the more transferable lesson.

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

**Two method errors, corrected after review. Both inverted the conclusion.**

*The denominator counted a different population from the numerator.* A subagent
transcript is not a session, and the two are distinguishable purely by path
depth: a session is `<project>/*.jsonl`, a subagent is
`<project>/<session-uuid>/subagents/*.jsonl`. The numerator counted only
sessions; the denominator counted every `.jsonl` on disk. Measured:
**184 top-level sessions and 5,648 subagent transcripts**, of which 4,238 belong
to one non-DDEV session. So the ratio was wrong by a factor of about 32, and the
original caveat here — that subagents "cannot be distinguished without reading
content" and would "inflate" the numbers — was wrong twice over.

*The worktree scan only looked at sibling directories* in `~/Projects/Genero/`,
so it missed every worktree checked out **inside** a repo.

**Result, corrected.**

| | |
|---|---|
| DDEV repos (`.ddev/config.yaml`) | 63 |
| …with any Claude Code history | **31** |
| Top-level sessions in DDEV repos | **113 of 184 — 61%** |
| Repos ever reaching 2 concurrent starts | **7** |
| Clustered start-pairs, entire history | **17** |
| Worktree checkouts on disk | **19** — 18 in `kokoomus/.claude/worktrees/`, 1 in `suomentyokalu` |
| Worktrees registered in `.git/worktrees/` | **57**, across 30 repos |

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

**Reading — the check passes, and the first draft of this document said the
opposite.** Most Claude Code sessions happen in DDEV repos, not a rounding error
of them. And the 18 `kokoomus/.claude/worktrees/agent-*` checkouts were created
on 2026-08-20 in bursts: four within 53 seconds (21:45:23 / 21:45:37 / 21:45:59 /
21:46:16), three more within 54 seconds the same morning. That is parallel agents
in worktrees, at a concurrency of three to four, in a DDEV repo — the exact
population §4.5 asked us to confirm.

kokoomus is also the project §3 cites as showing "0.0% parallel-session time".
Its 18 parallel agents are subagent transcripts, which the clustering metric
below excludes by construction, so the metric cannot see the very thing it was
built to measure. The clustering table is left in place because it is accurate
for *top-level* sessions, but it is a floor, not a count.

The same standard has to apply in both directions: this document is careful to
say that retained dumps are "a floor, not a count — they get overwritten and
deleted", and worktrees are deleted far more aggressively than dumps. Worktrees
under `/private/tmp/.../scratchpad/` are pruned when a session ends and cannot be
seen on disk at all.

**The `db:pull` prize is real too, and smaller than first stated.**

| | |
|---|---|
| Retained `database.@*.sql` / `wp-sync.sql` files | **19** |
| …of which are production pulls | **13** (5 are `@ddev` exports, 1 is `@staging`) |
| Total bytes on disk | **8.6 GB** (first stated as 9.6; no arithmetic over the files yields that) |
| Span | 2025-08-08 → 2026-08-27 |
| Multisite repos (search-replace is `--precise` across N pairs) | **12**, N from 2 to 9 |

Not every pull is a *multisite* `--precise` run either: 11 of the 19 files belong
to single-site repos. And these are one developer's dumps on one machine, so
"every developer on every pull" is an extrapolation, not a measurement — the same
caveat that applies to the worktree numbers above.

The 9.6 GB figure is reachable only by adding `.sql` files that are not `db:pull`
artifacts at all (`kokoomus/database.sql`, a Liana export, a cleaned staging
dump), which contradicts the sentence it was supporting. Worth knowing separately:
DDEV keeps a full copy of the last imported dump under `.ddev/.importdb*/`, so on
any repo that has imported one, bytes-on-disk is roughly double the number of
distinct pulls.

So both halves of §4.3 hold. The design does not change; what changes is that
the worktree case no longer needs to be argued down.

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
| Present locally as a regular file | **127** |
| **Missing locally** | **2,534 — 95.2% of distinct URLs** |

The first draft said 162 present and 94% missing. The existence test used `-e`,
which is true for directories: **35 of the 162 were directories**, not files.
The document contained the evidence of its own bug — 162 "present" against a
tree it described as 137 files is impossible — and did not notice.

**"95.2% of distinct URLs" is not "95.2% of requests", and the difference
matters.** Request volume is nowhere near uniform across distinct URLs: the 127
present files are theme SVGs, icons and fonts, which is exactly what loads on
*every* page view, while the 2,534 missing are long-tail post attachments.
Weighted by requests the figure would be far lower, and nothing in this method
measures requests. Wherever this number is quoted, it is a distinct-URL rate.

The local tree is 159 files, 1.2 MB — 137 SVGs plus 6 `woff2`, 6 `woff`, 4 HTML
and 4 CSS. The `2016`–`2026` folders are ordinary WordPress structure; only
`uploads/.github/`, `uploads/config/` and `uploads/web/app/` are the empty
skeleton left by a mis-targeted rsync. `web/app/uploads/*` is fully gitignored
and `robo files:pull` has evidently never completed on this checkout.

Missing uploads are **not** a regression hostshift introduces — under the status
quo the same files are absent. The finding matters for two other reasons, both
recorded in §4.5: it makes the redirect hazard in §3 below load-bearing rather
than theoretical, and it puts a number on §9.

Syncing uploads is `robo files:pull`'s job and is explicitly not a hostshift
deliverable.

### What the dump also settles about the origin automaton

Which origin *forms* are present, per host. These are **matching lines**
(`grep -c`), not occurrences — a single SQL insert line can carry hundreds of
URLs, which is why `www.herrfors.fi` shows 15,155 here and 125,534 occurrences
when counted with `grep -o`. The table is for reading which *forms* exist, not
how many:

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
- **Percent-encoded origins are real** — 44 matching lines (6 + 38 in the table
  above; the first draft said 46, a slip).
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

**55 of 63 DDEV repos (87%)** ship a committed nginx snippet that 302-redirects
any missing `/app/uploads/` request to a hardcoded **production** origin —
`.ddev/nginx/redirect-uploads.conf` in 49, `uploads-redirect.conf` in three
(`ekorosk`, `familjen-snellman-sweden`, `generogrowth`), folded into
`nginx_full/nginx-site.conf` in three more (`niva`, `snellmanecom`,
`solarplexius`). The first draft said 53; the three-and-three sub-lists were
right and the total was not.

About a third target a `*.kinsta.cloud` hosting hostname rather than the site's
public production domain, so "production origin" is loose for those — but every
one of them is a host the repo's own `robo.yml` declares, so the loop argument
is unaffected.

Under hostshift, that 302's `Location` is Tier 1 and gets rewritten back to the
variant host — which re-requests the same missing file. `ERR_TOO_MANY_REDIRECTS`,
for the 95.2% of distinct upload URLs that are absent locally, on 87% of the
fleet. Today it works, because no proxy
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
| `fsi` is `multisite: true` with no `url` lists at all | **False.** Complete index-aligned **9**-entry lists for `@ddev`, `@staging`, `@production`. The largest multisite in the fleet, across 5 registrable domains |
| `suomentyokalu` `@staging` empty | **False.** Aligned at 2 |
| `spfpension` `@staging` has 1 URL against 4 | **The plan was right.** It is a *scalar* `url: 'https://stg-spfpension-staging.kinsta.cloud'`, one URL. The audit script counted `- ` list items and got 0, and the first draft of this document "corrected" the plan into being wrong |
| "N from 2 to 5" | N from **2 to 9** |
| table of multisite repos | omitted `fsi` (9) and `steripolar` (3) |

Three further corrections, none of them a claim §4.2 made but all assumptions it
invites:

- **The environment set is not fixed.** Fleet-wide: `@ddev`, `@dev`, `@kinsta`,
  `@legacy`, `@legacy-production`, `@legacy-staging`, `@netvisor`, `@production`,
  `@staging`, `@vagrant`, some with the key quoted. An earlier draft of this
  paragraph listed five of the ten while arguing against assuming a fixed set.
- **A URL can repeat within one environment's list**, so the derived map is not
  injective: `spfpension`'s `@ddev` lists `osterbotten.spfpension.ddev.site`
  twice against two different canonical origins, and `mutti` and `panini` repeat
  hosts too. That is test 29c firing on real fleet data, and the adapter must
  resolve it rather than emit a map hostshift refuses at startup.
- **Values interpolate** — `mutti`'s URLs are `http://${machine_name}.ddev.site`.
  The adapter must expand placeholders, not read them literally.

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
herrfors (2) + pellervo (5): nine blogs across five registrable domains, and an
`@staging` list that is `http://` where fsi's other environments are `https://`.
(Not a fleet-wide first: `mutti`'s `@ddev` and `@dev`, and `beamex` and
`suomentyokalu`'s `@vagrant`, are `http://` too.)
§8's "done when" is left as written; this is a note, not a change.

## 7. Toolchain

There is **no `go` on `PATH`**. Go 1.26.5 exists at
`/nix/store/4im44k446822ixjai0mdaizqskc90qxr-go-1.26.5/bin/go`, which is how the
spike was run. M1 must put Go on `PATH` — nix profile, devShell, or a `flake.nix`
in this repo — before `go build` works from a plain shell. Recorded in §8a.
