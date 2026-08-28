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
| Rewrite (~1,100 origins spliced) | 124 → **158 MB/s** | 2.27 → **1.32 MB** | 40,572 → **17,523** |
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
