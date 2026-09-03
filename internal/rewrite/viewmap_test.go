package rewrite

import (
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The position map, whatever it is stored as, must answer exactly what a dense
// per-byte map would.
//
// The map is ~76% of this package's allocation and carries one discontinuity
// every 62 to 189 bytes, so most of what a dense map stores is derivable. That
// makes it worth compressing — and compressing the arithmetic that decides where
// a splice lands is the most dangerous edit available in this file, because a
// wrong offset does not fail loudly: one call site checks `from < 0`, the rest
// either panic on a slice bound or silently skip a host, which is a §4.3 leak
// with no log line.
//
// So the dense map is kept here as an oracle. `denseMap` rebuilds what the old
// representation held, by construction from the same builder, and every accessor
// answer is compared against it — over real captured pages, every view, and
// randomised inputs shaped like the escape families §4.4 enumerates.
//
// If this test is ever deleted, the compression has to be reverted with it.

// denseMap is what `pos`/`end` held when they were one entry per view byte.
type denseMap struct {
	pos []int32
	end []int32
}

// viewBuilders are every view the locator can build, so none is compressed with
// its neighbours untested.
var viewBuilders = []struct {
	name string
	fn   func([]byte, ctlMode) normalised
}{
	{"url", stripForURL},
	{"refs", stripForRefs},
	{"jsonesc", stripForJSONEsc},
	{"jsonescctl", stripForJSONEscCtl},
	{"percent", stripForPercent},
	{"css", stripForCSS},
	{"refscss", stripForRefsCSS},
}

func checkAgainstDense(t *testing.T, what string, n normalised, d denseMap) {
	t.Helper()
	if n.length() != len(d.pos) {
		t.Errorf("%s: view has %d bytes, dense map has %d entries — the map and "+
			"the bytes it indexes have to be the same length",
			what, n.length(), len(d.pos))
		return
	}
	for i := 0; i < n.length(); i++ {
		if got, want := n.at(i), d.pos[i]; got != want {
			t.Errorf("%s: at(%d) = %d, dense pos = %d — a splice lands on the "+
				"wrong byte", what, i, got, want)
			return
		}
		if got, want := n.endAt(i), d.end[i]; got != want {
			t.Errorf("%s: endAt(%d) = %d, dense end = %d — a splice replaces the "+
				"wrong range", what, i, got, want)
			return
		}
	}
}

// dense is the map as the *builder* recorded it, one entry per view byte, taken
// straight from `push` under `denseCheck`. It shares nothing with the run
// arithmetic the accessors use, which is the whole point: reading it back
// through the accessors would compare the compression against itself.
func dense(n normalised) denseMap {
	return denseMap{pos: n.densePos, end: n.denseEnd}
}

func TestTheViewMapAnswersWhatADenseMapWould(t *testing.T) {
	denseCheck = true
	defer func() { denseCheck = false }()

	var inputs []string

	// Real captured pages, which is what the views actually run over.
	files, _ := filepath.Glob("../../spike/corpus/*.html")
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil {
			inputs = append(inputs, string(b))
		}
	}
	if len(inputs) == 0 {
		t.Fatal("no corpus pages found, so this would assert nothing")
	}

	// And the escape families, each next to the bytes that terminate it — the
	// shapes where a run breaks and the arithmetic is not a straight line.
	inputs = append(inputs,
		"https://www.example.fi/x",
		"https:&#47;&#47;www.example.fi&#9;next",
		"https:&#x2f;&#x2f;www.example.fi&#10;next",
		`https:\/\/www.example.fi\tnext`,
		`https://www.example.fi
next`,
		"https%3A%2F%2Fwww.example.fi%09next",
		`url(https\3a \2f \2f www.example.fi\9 next)`,
		"a\tb\nc\rd https://www.example.fi\te",
		"&#9;&#10;&#13;&Tab;&NewLine; https://www.example.fi",
		strings.Repeat("&#47;", 200)+"https://www.example.fi",
		strings.Repeat("\t", 200)+"https://www.example.fi\t"+strings.Repeat("\t", 200),
		"",
		"&",
		"%",
		`\`,
	)

	// Randomised, drawn from the alphabet the decoders actually branch on, so a
	// run boundary can land anywhere rather than only where a fixture put it.
	rng := rand.New(rand.NewSource(20260903))
	alphabet := []string{
		"a", "z", "0", ".", "-", "/", ":", "https", "www.example.fi",
		"\t", "\n", "\r", " ", "&", "#", ";", "%", "3A", "2F", `\`, "u002d",
		"&#9;", "&#10;", "&#x2f;", "&Tab;", `\t`, `\n`, `
`, `\3a `, "%09", "%2F",
	}
	for i := 0; i < 3000; i++ {
		var sb strings.Builder
		for j := 0; j < 1+rng.Intn(40); j++ {
			sb.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		inputs = append(inputs, sb.String())
	}

	checked := 0
	for _, in := range inputs {
		for _, vb := range viewBuilders {
			for _, mode := range []ctlMode{ctlJoin, ctlProse, ctlProseKeepTab} {
				n := vb.fn([]byte(in), mode)
				checkAgainstDense(t, vb.name, n, dense(n))
				checked++
				if t.Failed() {
					t.Fatalf("stopping at the first disagreement, on input %q", in)
				}
			}
		}
	}
	t.Logf("%d view builds agreed with a dense map, over %d inputs", checked, len(inputs))
}
