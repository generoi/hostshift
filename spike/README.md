# spike/ — evidence for the Go decision

Working code from the 2026-08-27 language evaluation. Not production code; it is
the proof behind §5.2 and §5.7 and the intended M1 starting point.

- `go/full/main.go` — 130 non-blank lines: `x/net/html` Tokenizer for framing,
  the 64-line intra-tag attribute-span scanner, per-value splicing.
  Byte-identical under an identity map across corpus and fixtures (0.02 s over
  5.9 MB).
- `go/e2e/main.go` — 98 non-blank lines, proxy on `httputil.ReverseProxy`. Green
  on acceptance tests 1, 15, 24, 27, 28. Its `// idempotency (test 7)` block
  discards its result — **test 7 is not covered**.
- `go/attrspan/main.go` — the span scanner plus its validator (19,953 start
  tags, 37,280 attributes, 9 divergences, all duplicate attribute names).
  The validator compares counts and names only; the `ValueStart`/`ValueEnd`
  assertion still needs landing.
- `go/frag`, `go/caveat`, `go/svg` — text-fragmentation, `Raw()`-lifetime and
  foreign-content probes.
- `go/rewriter/main.go`, `go/main.go` — earlier whole-tag `bytes.ReplaceAll`
  drafts, kept only for comparison. **Not** the starting point.
- `corpus/` — 15 real pages (5,940,172 bytes). `adv/` — 36 adversarial fixtures.

**Before building on this:** the matching in `full` and `e2e` is an unanchored
`bytes.ReplaceAll` placeholder — the exact construction PLAN §4.4 identifies as
the double-port bug. Replace it with the anchored origin automaton first;
`rewriteValue` is the seam. Keep the framing and splicing.

Run the identity check over `corpus/` and `adv/` before trusting any change.
