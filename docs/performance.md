# Performance

Measured 2026-08-28 on an Apple M4, over `spike/corpus/page1.html` — a real
508 KB WordPress page carrying ~1,100 canonical origins. `go test
./internal/rewrite/ -bench . -benchmem` reproduces all of it.

§9 calls performance "immaterial for dev", and it is — but one number was not
merely immaterial, it was a bug.

## Before and after

| | throughput | allocated per page | allocs |
|---|---|---|---|
| Passthrough (no origin on the page) | 156 → **199 MB/s** | 1.80 → **0.86 MB** | 36,780 → **14,173** |
| Identity map (every origin matches, none rewritten) | 139 → **192 MB/s** | 1.84 → **0.96 MB** | 39,033 → **16,289** |
| Rewrite (~1,100 origins spliced) | 124 → **185 MB/s** | 2.27 → **1.32 MB** | 40,572 → **17,523** |
| **Rewrite + straggler sweep — what the proxy runs** | **23.6 → 128 MB/s** | **179.5 → 1.54 MB** | 62,141 → **19,240** |

The full pipeline is **5.4× faster and allocates 117× less**.

Re-measured after the M2–M6 audits merged, which is why these are a little below
the numbers this file first carried: the audits added work the earlier figures
did not include — a left-context byte and an input-offset map in the sweep, an
entity decode on attribute values that contain "&", and a prefilter that now
tests both hex cases. All of it closes a leak or a guard-rail break, and the
pipeline is still four times faster than where M6 started.

## The four changes

**The sweep allocated a fresh 32 KB read buffer on every `Read` call.** The
tokenizer upstream hands back one small token at a time, so a single 500 KB page
drove roughly 5,600 of them: **179 MB of garbage to sweep half a megabyte**. The
buffer is now allocated once per stream. This was the whole gap between the
sweep costing 5× the rewrite and costing 1.4×.

**`Raw()` was copied for every token.** The defensive copy exists because
`TagName()`/`TagAttr()` may invalidate it (§5.2, and the aliasing bug at
`spike/go/full/main.go:100-105`) — but those are only ever called for start tags.
Every other token goes straight into the pending buffer, which copies anyway. The
copy is now restricted to start tags, which is about a third of the allocations
on a page with no rewrites.

**Every attribute name was lowercased into a string** to test it against five
structured-attribute names — one allocation per attribute, 37,280 of them across
the corpus, for a check that `bytes.EqualFold` answers without allocating.

**A separator prefilter before the automaton.** The Aho–Corasick library
allocates an iterator and a prefilter state on every `IterByte` call, so scanning
a value cost three allocations even when it could not possibly match. Every
pattern contains `//`, `\/` or `%2F`, because every origin form spells its
separator one of those three ways — so a value containing none of them is
returned untouched without the automaton being involved at all. Most attribute
values on a page are class names, ids and `data-` attributes.

**A prefilter must never be narrower than the thing it filters for**, and this
one was, twice. The automaton is built `AsciiCaseInsensitive`, so `%2f` matches
as readily as `%2F`; testing only the uppercase spelling short-circuited every
lowercase percent-encoded origin — a `redirect_to=https%3a%2f%2f…` went out
dereferenceable, and the sweep could not catch it because the sweep runs through
this same filter. It also made the proxy non-idempotent, since whether such an
origin was rewritten depended on whether some unrelated `//` happened to land in
the same buffer. And the early return handed back `consumed = len(b)` regardless
of `limit`, discarding §4.4's carry-over window whenever a chunk held no
separator, so an origin straddling a read boundary went out unrewritten. Both
were caught by the M6 audit, and both are the same lesson: the cheap check has
to accept a superset of what the expensive one would.

A fifth was found and left alone: **the replacement string was rebuilt on every
match**, from the variant's scheme, separator and host:port. It depends only on
the pattern's form and the pair's variant, both fixed at build time, so it is now
precomputed per (pattern, pair) in `NewMatcher`.

## What was deliberately not optimised

**`NewMatcher` costs 2.8 ms and 257k allocations** for a nine-blog site (81
patterns), almost all of it building the Aho–Corasick DFA. It runs once per
process. `DFA: false` would make construction cheaper and matching slower, which
is the wrong trade for a proxy that builds one map and then serves from it.

**Memory is bounded by the largest token, not the response** (test 13): a 5 MB
page streams with a **42-byte** peak token buffer. Throughput mattered less than
that bound, and the bound was already right.


## Second pass, 2026-08-28

An architectural audit found that the biggest remaining cost was work done for
data hostshift never reads.

| | throughput | allocated per page | allocs |
|---|---|---|---|
| Passthrough | 202 → **211 MB/s** | 0.86 → **0.31 MB** | 14,173 → **8,128** |
| Identity map | 191 → **200 MB/s** | 0.96 → **0.41 MB** | 16,289 → **10,244** |
| Rewrite | 185 → **194 MB/s** | 1.32 → **0.77 MB** | 17,523 → **11,477** |
| **Rewrite + straggler sweep** | 130 → **132 MB/s** | 1.54 → **0.99 MB** | 19,240 → **13,194** |

**42% fewer allocations and 64% less garbage on a page with nothing to rewrite**,
which is most pages.

**`Tokenizer.TagName()` is `bytes.ReplaceAll`, and Go's `bytes.Replace` copies
even when it replaces nothing.** Every start tag paid a copy of its element name
purely to be compared against ten constants. The name is `raw[1:i]` where `i` is
where `scanAttrs` already stops — the same terminator set `readTagName` uses —
so `tagNameOf` returns a slice and `bytes.EqualFold` answers the comparison
without allocating.

**That copy forced a second one.** `TagName()`/`TagAttr()` may invalidate
`Raw()`; the docs promise only the partition guarantee, and safety rested on
`TagAttr` happening to allocate before its in-place unescape. So every raw start
tag was cloned defensively. With neither function called any more the hazard is
*gone* rather than guarded against — the aliasing bug at
`spike/go/full/main.go:100-105` cannot recur — and two allocations per start tag
go with it. Worth 3,279 allocations a page.

**`scanAttrs` allocated a fresh span slice per tag.** The spans are offsets into
`raw` and are consumed before the next token, so one buffer serves the whole
page. Worth 2,766 allocations a page.

Verified byte-identical rather than argued: the full `NewResponseBody` output was
hashed for all 51 fixtures in `spike/corpus` and `spike/adv`, crossed with four
chunk sizes (1, 7, 4096, 1 MiB) and four maps (rewriting, identity, a shorter
variant, a map matching nothing). **All 816 SHA-256 hashes are unchanged.**

### The measurement that was missing, and what it did not justify

Every benchmark here measured the rewriter in isolation, where `io.Copy` hands it
a 32 KiB buffer and there is no HTTP layer. `BenchmarkE2EPage` in
`internal/proxy` measures a whole request, which is what a browser waits for, and
it says something the others cannot.

`httputil`'s `flushInterval()` returns -1 — flush after every `Read` — whenever
`res.ContentLength` is -1, and ignores `p.FlushInterval` in that branch.
hostshift sets `ContentLength` to -1 on every rewritten response because every
rewrite changes the length, and `HTML.Read` returns one token, about 50 bytes. A
508 KB page is therefore roughly **ten thousand chunked writes, ten thousand
flushes and ten thousand write syscalls**. Profiling a whole request puts the
tokenizer, matcher and sweep together at **1.4% of CPU** and raw syscalls at
**78.8%**.

Batching the reads measures **13.55 ms → 4.38 ms, 3.1×.** It is not taken.

Filling the caller's buffer means one more `Read` than there is data, which
blocks — holding a progressively flushed response until 32 KiB accumulates or
the operation ends. That is wp-admin's update and import screens, where watching
progress is the point. Measured: the first flush never arrived. Two cheaper
discriminators failed too. Asking the pipeline "have you more in hand" does not
work, because a tokenizer's `Buffered()` being non-empty does not mean another
token can be produced — a trailing text token is incomplete until the next `<`
or EOF — and it blocked anyway. Gating on the upstream having sent a
`Content-Length` is correct and useless: a WordPress page far exceeds nginx's
FastCGI buffers, so it arrives chunked and the benchmark went straight back to
12.9 ms. Decoupling production from consumption does work and costs a goroutine
and a ring buffer in the response path; a first cut allocated **45 MB per page**,
an 8 KiB chunk per 50-byte token.

**Every one of those attacked the read side, and blocking is inherent there.**
The expensive half is the flush, not the write — net/http buffers 2 KiB before
it chunks, so swallowing a flush costs a bounded delay and no memory at all.

`httputil` calls `http.NewResponseController(dst).Flush` after every write, and
`dst` is the `ResponseWriter` hostshift hands it. So wrap it. `coalescer` passes
`Write` straight through and defers `Flush` by at most 100 µs: it is httputil's
own `maxLatencyWriter`, moved one layer down where `flushInterval` cannot
overrule it. **11.81 ms → 3.75 ms, 3.15×, and 5,381 chunks on the wire → 206.**
Output byte-identical, the whole suite green unmodified, `-race` clean, and the
first flush of a progressive response arrives 269 µs after the upstream sends
it, against 98 µs unbatched.

Confirmed on the live stack rather than only on loopback. Traefik does coalesce
toward the browser — the client sees a few hundred reads, not 5,381 — but
hostshift still pays for the storm. Container CPU per byte, measured through the
real browser → Traefik → hostshift → nginx path: **27.0 and 36.4 ms/MB for
rewritten HTML against 1.35–2.24 ms/MB for a pass-through JS file** on the same
path. Twelve to twenty-seven times the CPU per byte, about 12 ms per page.

Nobody perceives 8 ms of a 280 ms page, and §9 is still right that performance
is immaterial for dev. It is taken because the *cost* side collapsed rather than
because the win grew: the reason batching was rejected does not apply to this
shape, and what is left is that hostshift stops burning 12 ms of laptop CPU per
page and stops making Traefik perform five thousand reads, on a machine already
running Docker, a browser, an IDE and several DDEV projects.

`TestProgressiveResponseIsNotHeld` guards it and now asserts a *bound* — it
allowed five seconds, which would have passed at a fifty-millisecond coalescing
latency and stopped guarding what its comment claimed. `BenchmarkE2EPage` keeps
the number honest.

One consequence worth knowing: `flushInterval` returns -1 for
`text/event-stream` too, and the coalescer sits below that, so SSE events gain
up to 100 µs. Harmless, and a two-line exemption in `WriteHeader` if it ever is
not.

Profiling allocations over a *whole request* rather than the rewriter also put
`schemeBefore` at **19% of all objects**: it concatenated `"https" + schemeSep()`
per candidate, about 1,100 a page. A precomputed table fixes it — a matcher hit
goes **331 → 197 ns** and 8 → 4 allocations, a miss **117 → 82 ns** and 2 → 0,
and `RewriteWithSweep` sheds another 2,920 allocations a page.

### Measured and not taken

- **`x/net/html`'s attribute dedup map** is 50–81% of what remains: a
  `bytes.Clone`, a `string` and a map insert per attribute, to dedupe names for
  `TagAttr()` — which hostshift never calls. Patching it locally takes
  passthrough to **1,503 allocations a page, 9.4× below where this started**.
  It belongs upstream: forking the tokenizer is the one dependency not worth
  forking, because test 24 rests entirely on its `Raw()` partition guarantee.
- **Lock-free `Stats` counters.** The mutex profile looks damning — 2.66 s of
  contended delay at `-cpu 10`, 96% of it in `Stats.Record` — and replacing the
  mutex with per-key atomics measured **no change at all** (p=0.667, n=14). The
  contention is real and off the critical path.
- **Short-circuiting the pipeline under an identity map.** It would make
  `BenchmarkIdentity` free and gut the guard rail: test 24 is meaningful because
  it drives the real tokenizer, splicer and sweep. A bypassed test asserts that
  `return r` returns `r`.
- **A reusable Aho–Corasick iterator.** `findIter` and `prefilterState` are
  unexported and `IterByte` allocates both per call, so this means forking the
  library — for ~52% of what is left, in a proxy §9 calls "immaterial for dev",
  in the exact component that has already leaked twice.
- **Request-direction fixes.** `Site.CanonicalSet()` allocates per call and
  `rewriteRequest` assigns unconditionally; fixing both moved a small GET from
  192 to 190 allocations. hostshift's own share of a request is ~20%; the rest is
  `net/http` and `httputil.ReverseProxy`.


## Third pass, 2026-08-28 — the automaton goes

Aho–Corasick is the right structure for thousands of patterns. This map has
tens: nine blogs is 81 patterns, and most are a handful of hosts spelled three
ways. Against that it cost 2.9 ms and 257,000 allocations to build, allocated an
iterator and a prefilter state on every `IterByte`, and stepped a transition
table byte by byte through a document that is 99.8% uninteresting.

| | before | after |
|---|---|---|
| Passthrough | 211 MB/s | **248 MB/s** |
| Identity map | 200 MB/s | **237 MB/s** |
| **Rewrite + straggler sweep** | 132 MB/s | **181 MB/s** |
| One value, hit | 517 ns | **331 ns** |
| One value, miss | 242 ns | **117 ns** |
| Building the map | 2.98 ms, 257,385 allocs | **14 µs, 361 allocs** |

The pattern set is what makes something simpler possible. Every pattern contains
a separator, and every explicit-scheme pattern *ends with* its relative form —
`https://H` is `https:` followed by `//H`. So finding the separator finds every
candidate: the host reads forwards from it, the scheme backwards. `bytes.Index`
skips to the next separator at whatever speed the platform manages, and page1
has one per ~480 bytes.

**Nothing about matching changed.** The swap is confined to which candidates are
offered; anchoring, ports, the root dot, port disambiguation and replacement are
the same code, and the scanner deliberately reproduces the automaton's
*sequence* — leftmost-longest, one candidate per position, advancing one byte
past each match start — so even the skipped-candidate events are identical.

Proven, not argued. The automaton is still built, in `scan_ac_test.go`, purely
as the oracle: it is the implementation every audit ran against. The two are
compared on identical output, `consumed` and full event stream over every shape
the audits turned up, all 51 corpus and adversarial fixtures at three limits and
both value/prose semantics, 20,000 random-byte inputs, four different maps, and
**40,000 composed fuzz cases of which 23,429 produce candidates and 25,228
produce rewrites** — the counts are asserted, because a fuzz that matches
nothing proves nothing, and the first attempt at one matched in 502 cases out of
40,000.

`go build` links neither the automaton nor its construction cost; `go version -m`
on the binary reports no `aho-corasick`. It stays in `go.mod` as a test
dependency, which is the point — the oracle survives the thing it replaced.

None of this is visible end to end (13 ms, unchanged), for the reason the
previous section gives: syscalls dominate. It is taken because it is a straight
substitution with no behavioural trade, and it removes a dependency from the
shipped binary.
