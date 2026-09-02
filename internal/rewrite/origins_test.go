package rewrite

import "testing"

// HostsIn backs `hostshift origins`, which backs `check`'s off-map scan. Round
// 54 added all three without a test, and round 55 found the census inflated:
// a dozen views run over one buffer, several find the same URL, and every hit
// incremented. One `wp_json_encode`-escaped URL — which is every WordPress page
// — multiplied the count of every *other* host on the page with it.
//
// The threshold that consumes these numbers means "five links", so inflation is
// not cosmetic: a page carrying two off-map URLs was reported as carrying eight.
func TestHostsInCountsEachOccurrenceOnce(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want map[string]int
	}{{
		"one plain URL",
		`<img src="https://g.example/a">`,
		map[string]int{"g.example": 1},
	}, {
		// The round-55 repro. The escaped URL is found by the plain view and by
		// the escape view; before the fix that made it 4, and dragged the
		// unrelated `g.example` up to 3 with it.
		"an escaped URL beside a plain one",
		`<script>var a="https:\/\/s.example\/x";</script><img src="https://g.example/a">`,
		map[string]int{"s.example": 1, "g.example": 1},
	}, {
		"the same host twice is two",
		`<a href="https://g.example/a">a</a><a href="https://g.example/b">b</a>`,
		map[string]int{"g.example": 2},
	}} {
		t.Run(c.name, func(t *testing.T) {
			got := HostsIn([]byte(c.in))
			for h, n := range c.want {
				if got[h] != n {
					t.Errorf("%s: %d, want %d (all: %v)", h, got[h], n, got)
				}
			}
			if len(got) != len(c.want) {
				t.Errorf("hosts: %v, want %v", got, c.want)
			}
		})
	}
}

// And the reason the scan moved into the binary at all: a shell grep saw one
// spelling out of the dozen a browser resolves. Each of these is a spelling
// WordPress or a browser actually produces, and every one must be found — that
// claim was made in round 54's commit message and never asserted.
func TestHostsInReadsEverySpelling(t *testing.T) {
	for name, in := range map[string]string{
		"plain":        `<a href="https://shop.acme.fi/x">`,
		"json escaped": `<script>{"u":"https:\/\/shop.acme.fi\/x"}</script>`,
		"percent":      `<a href="https%3A%2F%2Fshop.acme.fi%2Fx">`,
		"reference":    `<a href="https:&#47;&#47;shop.acme.fi/x">`,
		"css escape":   `<style>a{background:url(https\3a \2f \2f shop.acme.fi/x)}</style>`,
	} {
		if got := HostsIn([]byte(in)); got["shop.acme.fi"] < 1 {
			t.Errorf("%s: shop.acme.fi not found (got %v) — the off-map scan is "+
				"blind to a spelling WordPress emits", name, got)
		}
	}
}

// The census reads a whole served page, script bodies and all, so it asks for
// the straggler's alphabet and not raw-text's.
//
// `raw-text` names the markup inside <noscript> and <title>, where no string
// decoder runs — round 55 moved it to the path alphabet, and HostsIn had been
// borrowing the name for something else entirely. Under the path reading the
// backslash below is a delimiter and the host comes out as shop.acme.fi; under
// the script reading `\x41` is an `A`, so ada resolves the whole thing to
// shop.acme.fia and this page carries no reference to shop.acme.fi at all.
func TestHostsInReadsAPageAsAPage(t *testing.T) {
	in := `<script>fetch("https://shop.acme.fi\x41")</script>`
	if got := HostsIn([]byte(in)); got["shop.acme.fi"] != 0 {
		t.Errorf("reported %d reference(s) to shop.acme.fi, but a browser resolves "+
			"this to shop.acme.fia — the scan is reading the page with the wrong "+
			"alphabet: %v", got["shop.acme.fi"], got)
	}
}

// The census must not invent a host.
//
// A view can end a host on a byte that leaves nothing behind:
// `https:&#47;&#47;c.example/0` yielded one named `&`. check prints the census,
// so it advised adding `&` to hostshift.yaml as an alias — and hostshift
// accepted it, `map --external-canonical-hosts` listed it, and
// `ddev hostshift loopback` would have written `- "&:127.0.0.1"` into the
// compose file. A phantom with a straight path to a broken deployment.
func TestHostsInInventsNoHosts(t *testing.T) {
	for name, in := range map[string]string{
		"numeric reference": `https:&#47;&#47;c.example/0`,
		"hex reference":     `https:&#x2F;&#x2F;c.example/0`,
		"named reference":   `https:&sol;&sol;c.example/0`,
	} {
		got := HostsIn([]byte(in))
		if got["c.example"] < 1 {
			t.Errorf("%s: c.example not found (got %v)", name, got)
		}
		for h := range got {
			if h != "c.example" {
				t.Errorf("%s: reported a host named %q, which no browser resolves "+
					"— check prints this and advises adding it to hostshift.yaml", name, h)
			}
		}
	}
	// And the shapes that must survive the filter, since the census is the only
	// instrument that can name an origin the map does not.
	for _, in := range []string{
		`<a href="https://xn--hmeen-loa.fi/x">`,
		`<a href="https://hämeen.fi/x">`,
		`<a href="https://a-b.c_d.example/x">`,
		`<a href="https://www.example.fi./x">`,
	} {
		if got := HostsIn([]byte(in)); len(got) == 0 {
			t.Errorf("the filter dropped a real host: %s", in)
		}
	}
}

// The census filter, in both directions.
//
// Requiring a dot dropped every IPv6 literal — a perfectly ordinary origin for a
// page to carry, and one that could therefore never appear in the list `check`
// prints — while `-bad.example` went through, because a leading hyphen was not
// checked. A filter that is uneven in both directions is worse than either.
func TestHostsInFiltersEvenly(t *testing.T) {
	got := HostsIn([]byte(`<a href="https://[2001:db8::1]/x">a</a>` +
		`<a href="https://-bad.example/y">b</a>` +
		`<a href="https://bad-.example/y">c</a>` +
		`<a href="https://ok.example/z">d</a>` +
		`<a href="https://a-b.ok.example/w">e</a>`))
	for _, want := range []string{"2001:db8::1", "ok.example", "a-b.ok.example"} {
		if got[want] < 1 {
			t.Errorf("%s was dropped: %v", want, got)
		}
	}
	for _, bad := range []string{"-bad.example", "bad-.example"} {
		if _, ok := got[bad]; ok {
			t.Errorf("%s reached the census, and check tells the developer to "+
				"paste that into hostshift.yaml: %v", bad, got)
		}
	}
}
