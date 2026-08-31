package rewrite

import (
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/generoi/hostshift/internal/origin"
)

// Three separate quadratics have been found in this package by audit, each one
// in a pass whose sibling had already been fixed for the same thing:
// scan.go's separator search, then locateHostIn's per-candidate strip, then
// foldedHostLeak restarting inside a slash run, then normaliseURLLeak's
// unbounded authority scan. Every one was reachable from page or request
// content, and the worst extrapolated to hours of pinned CPU for a single
// response, with no timeout anywhere on the path.
//
// None of them changed a byte of output, so no correctness test could see them.
// This one measures instead: quadrupling the input must not raise the time by
// much more than four, and the shapes are the ones that actually bit.
func TestPassesStayLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt-a--example.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		// build returns a document whose interesting region is n units long.
		build func(n int) string
	}{
		{
			// The unbounded authority scan: a candidate at every `http:`, each
			// scanning for a delimiter that is not there. Every byte here is a
			// legal authority byte, so only maxHost stops the scan running to
			// the end of the buffer — a space would end it after one byte and
			// prove nothing.
			"scheme candidates with no delimiter",
			func(n int) string { return "<p>" + strings.Repeat("http:", n) + "</p>" },
		},
		{
			// foldedHostLeak restarting inside a slash run. One non-ASCII byte
			// anywhere in the value is the whole gate.
			"a long slash run beside a non-ASCII byte",
			func(n int) string { return "<p>café " + strings.Repeat("/", n*6) + "</p>" },
		},
		{
			// The same, in an attribute, which is where the per-candidate strip
			// was quadratic.
			"a long token list in a URL attribute",
			func(n int) string { return `<a ping="` + strings.Repeat(" /", n*3) + `">x</a>` },
		},
		{
			// Brackets, the fourth instance of the class. `[` is an authority
			// byte — for an IPv6 literal — *and* a token boundary, so it both
			// began a candidate and failed to end one, and the `]` search in the
			// bracketed branch ran to the end of the buffer as well. 400 KB took
			// 58 seconds; 8 MiB, which is DefaultMaxBody, extrapolates to about
			// seven hours of pinned CPU for one request.
			"brackets, which are both an authority byte and a boundary",
			func(n int) string { return "<p>" + strings.Repeat("[http:", n) + "</p>" },
		},
		{
			// The same bug in the other branch, and the two need separate
			// shapes: with `[http:` the authority starts *on* the bracket and
			// takes the IPv6 path, whose `]` search is what runs away. One byte
			// after the colon moves the start off the bracket and onto the
			// general scan, where `[` was an authority byte that did not
			// terminate it. Bounding one branch leaves the other quadratic.
			"a bracket the general authority scan has to stop at",
			func(n int) string { return "<p>" + strings.Repeat("[http:a", n) + "</p>" },
		},
		{
			// The reference views, which the guard had no shape for at all —
			// every case here was reference-free, so the three newest and most
			// expensive decoders were never timed. `&` is the whole gate.
			"character references, which gate three views",
			func(n int) string {
				return `<div style="` + strings.Repeat(`&#92;3a `, n) + `">x</div>`
			},
		},
		{
			// CSS escapes, the newest decoder.
			"css escapes",
			func(n int) string {
				return "<style>a{background:url(" + strings.Repeat(`\3a `, n) + ")}</style>"
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			const small, factor = 20000, 4
			run := func(n int) time.Duration {
				in := c.build(n)
				start := time.Now()
				r := NewResponseBody(io.NopCloser(strings.NewReader(in)), m, nil,
					Options{Stats: NewStats(false)})
				if _, err := io.Copy(io.Discard, r); err != nil {
					t.Fatal(err)
				}
				return time.Since(start)
			}
			// Warm, then measure, so the first allocation of the run does not
			// land on the small case and flatter the ratio.
			run(small / 4)
			a, b := run(small), run(small*factor)

			// A generous bound: quadratic would be 16x, and the floor keeps a
			// sub-millisecond small case from turning noise into a failure.
			const floor = 20 * time.Millisecond
			if a < floor {
				a = floor
			}
			if ratio := float64(b) / float64(a); ratio > 8 {
				t.Errorf("%s: %dx the input took %.1fx the time (%v → %v); "+
					"linear is %d, quadratic is %d",
					c.name, factor, ratio, a, b, factor, factor*factor)
			}
		})
	}
}

// The same shapes through the request direction, which is a different code path
// with the same passes behind it — HostLeaks, not the HTML pipeline — and where
// the 8 MiB body cap makes the worst case bigger than any response.
func TestHostLeaksStaysLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://wt-a--example.ddev.site"),
		Variant:   origin.MustParse("https://www.example.fi"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	// HostLeaksBack, not HostLeaks: the request direction runs strictly more
	// views — the reference one and the composed refs-then-CSS one — so timing
	// HostLeaks left the expensive half of the request path unmeasured, on the
	// side where the 8 MiB body cap makes the worst case biggest.
	build := func(n int) []byte {
		return []byte(`{"u":"` + strings.Repeat("[http:&#92;3a ", n) + `"}`)
	}
	run := func(n int) time.Duration {
		b := build(n)
		start := time.Now()
		HostLeaksBack(m, b)
		return time.Since(start)
	}
	run(5000)
	a, b := run(20000), run(80000)
	const floor = 20 * time.Millisecond
	if a < floor {
		a = floor
	}
	if ratio := float64(b) / float64(a); ratio > 8 {
		t.Error(fmt.Sprintf("4x the request body took %.1fx the time (%v → %v)", ratio, a, b))
	}
}

// Transient allocation, held to a ceiling so it cannot drift unnoticed.
//
// Each view allocates a decoded copy plus two position maps — 17 bytes per
// input byte — and composeView adds two more maps on top of an intermediate.
// Round nineteen measured 186x the body on a shape that fires every view: 1.5 GB
// churned and a 447 MB heap high-water mark for one 8 MiB request, against ~37x
// recorded in earlier rounds. Timing extrapolates linearly: one 8 MiB request
// pins a core for about 0.4 s. It is a memory ceiling problem, not a latency
// one.
//
// These ceilings went *up* in round twenty. An attempt to skip the composed
// view when the references did not spell a backslash took the ampersand-only
// shapes to 85x and 80x — and was a leak, because stripForCSS falls through to
// stripForURL and that is the layer removing reference-spelled tab, LF and CR.
// The allocation is not worth a leak; the numbers here record what correctness
// actually costs.
//
// The structural halving that remains would be `pos`/`end` as []int32 — safe,
// since DefaultMaxBody is 8 MiB and well under 2^31, and worth about 47%. It is
// 141 index sites in the most delicate arithmetic in this package, so it is a
// measured, deliberate deferral rather than something done in the same pass as
// a leak fix. This test is what makes it a tracked number instead of a
// forgotten one.
func TestAllocationStaysBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation")
	}
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt-a--example.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, unit string
		ceiling    float64
	}{
		{"every view fires", `&#92;3a %5C\3a [http:`, 200},
		{"ampersands alone", `&`, 128},
		{"references and brackets", `&#91;http:`, 115},
	} {
		t.Run(c.name, func(t *testing.T) {
			// At the shipped cap, not a convenient megabyte. Measuring at 1 MiB
			// and printing a ratio invites the reader to multiply wrong: the
			// number that matters is the peak heap for one request at
			// DefaultMaxBody, and concurrent bodies multiply it.
			b := []byte(strings.Repeat(c.unit, (8<<20)/len(c.unit)))
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			HostLeaksBack(m, b)
			runtime.ReadMemStats(&after)
			ratio := float64(after.TotalAlloc-before.TotalAlloc) / float64(len(b))
			t.Logf("%s: %.0fx the body — %.0f MB churned, %.0f MB peak heap for one "+
				"%d MiB request", c.name, ratio,
				float64(after.TotalAlloc-before.TotalAlloc)/(1<<20),
				float64(after.HeapSys)/(1<<20), len(b)>>20)
			if ratio > c.ceiling {
				t.Errorf("%s: transient allocation is %.0fx the body, ceiling %.0fx",
					c.name, ratio, c.ceiling)
			}
		})
	}
}
