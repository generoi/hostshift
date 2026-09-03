package rewrite

import (
	"bytes"
	"os"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Every audit round has fuzzed this package and thrown the harness away
// afterwards — tens of millions of executions per round, rebuilt from scratch
// each time and never run again. The invariants have held every time, which is
// itself the finding: what keeps breaking is the *model* of what a browser does,
// not the engine's arithmetic. But the arithmetic is what grows index maps, and
// composeView added a second layer of them, so the harness belongs in the repo
// where it runs by default and accumulates a corpus.
//
// Three properties, none of which needs an oracle:
//
//   - an identity map returns the input byte for byte (acceptance test 24);
//   - a real map is idempotent — the output re-fed through is unchanged
//     (acceptance test 7);
//   - and nothing panics, which is what the position maps are for.
//
// Run it long with:
//
//	go test ./internal/rewrite -run XXX -fuzz FuzzRewriteInvariants -fuzztime 10m
func FuzzRewriteInvariants(f *testing.F) {
	for _, path := range []string{"../../spike/adv", "../../spike/corpus"} {
		entries, err := os.ReadDir(path)
		if err != nil {
			continue
		}
		for _, e := range entries {
			b, err := os.ReadFile(path + "/" + e.Name())
			// The big real pages make each execution slow enough to starve the
			// mutator; the adversarial fixtures are the interesting shapes.
			if err == nil && len(b) < 64*1024 {
				f.Add(b)
			}
		}
	}
	// The shapes rounds nine to eighteen turned up, in case the fixtures move.
	for _, s := range []string{
		`<a href="https:\\www.example.fi/x">t</a>`,
		`<a href="https:&#47;&#47;www.example.fi/x?a=&#6&#48;;b">t</a>`,
		`<div style="background:url(https&#92;3a &#92;2f &#92;2f www.example.fi/x.png)">t</div>`,
		`<script>f(decodeURIComponent("https%3A%5C%2F%5C%2Fwww.example.fi%2Fx"))</script>`,
		`<p>[http:www.example.fi/x] [http:a] [::1] [</p>`,
		`<svg><style>#d{background:url(https&#92;3a &#92;2f &#92;2f www.example.fi/x.png)}</style></svg>`,
		`<a href="https://www.example.fi。:443/x">t</a>`,
		`<a href="https://u@www.example.fi:8080/x">t</a>`,
		`<p>see https://www.example.fi. thanks</p>`,
	} {
		f.Add([]byte(s))
	}

	ident, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://www.example.fi"),
	}})
	if err != nil {
		f.Fatal(err)
	}
	real, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt-a--example.ddev.site"),
	}})
	if err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, in []byte) {
		if out := runHTML(t, in, ident, Options{}); !bytes.Equal(in, out) {
			t.Fatalf("identity map changed the bytes (%d in, %d out)", len(in), len(out))
		}
		once := runHTML(t, in, real, Options{})
		if twice := runHTML(t, once, real, Options{}); !bytes.Equal(once, twice) {
			t.Fatalf("not a fixed point (%d -> %d on the second pass)", len(once), len(twice))
		}
	})
}

// The same three properties for the standalone entry points, which are a
// different code path — no tokenizer above them, and the request direction runs
// every view including the composed one.
func FuzzHostLeaksInvariants(f *testing.F) {
	for _, s := range []string{
		`{"u":"https:\\/\\/www.example.fi/x"}`,
		`https%3A%5C%2F%5C%2Fwww.example.fi%2Fx`,
		`https:&#47;&#47;www.example.fi/x`,
		`[http:[http:[http:`,
		`<link>https://www.example.fi/x</link>`,
		`https://[::1]:8080/x`,
		`https://u@www.example.fi./x`,
	} {
		f.Add([]byte(s))
	}
	ident, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://www.example.fi"),
	}})
	if err != nil {
		f.Fatal(err)
	}
	real, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt-a--example.ddev.site"),
	}})
	if err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, in []byte) {
		for name, fn := range map[string]func(*origin.Matcher, []byte) []byte{
			"HostLeaks":     func(m *origin.Matcher, b []byte) []byte { return HostLeaks(m, b, true) },
			"HostLeaksXML":  func(m *origin.Matcher, b []byte) []byte { return HostLeaksXML(m, b, true) },
			"HostLeaksBack": HostLeaksBack,
		} {
			if out := fn(ident, in); !bytes.Equal(in, out) {
				t.Fatalf("%s: identity map changed the bytes (%d in, %d out)", name, len(in), len(out))
			}
			once := fn(real, in)
			if twice := fn(real, once); !bytes.Equal(once, twice) {
				t.Fatalf("%s: not a fixed point:\n once  %q\n twice %q", name, once, twice)
			}
		}
	})
}
