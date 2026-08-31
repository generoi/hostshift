package rewrite

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// lengthen is a stand-in rewrite: it lengthens, which is what makes a stale prefix
// visible. Using the real matcher would work too, but this keeps the fixtures
// readable and the failure obvious.
func lengthen(b []byte) []byte {
	return []byte(strings.ReplaceAll(string(b), "a.test", "a.test.local"))
}

// blob builds `s:LEN:"v";` with a correct length, in the literal spelling.
func blob(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }

// Every length in the output must describe the data that follows it, at every
// depth. This file had no test at all when it shipped, and the coverage report
// showed the nested branch at count 0 — which is exactly the branch that was
// broken, and the one the commit message advertised.
func TestSerializedLengthsAreCorrectAtEveryDepth(t *testing.T) {
	for _, c := range []struct{ name, in string }{
		{"a flat span", `a:1:{s:1:"u";` + blob("https://a.test/x") + `}`},
		{"two spans", `a:2:{s:1:"u";` + blob("https://a.test/x") + `s:1:"v";` + blob("https://a.test/y") + `}`},
		{"a nested payload",
			`a:1:{s:1:"o";` + blob(`a:1:{s:1:"u";`+blob("https://a.test/x")+`}`) + `}`},
		{"three deep",
			`a:1:{s:1:"o";` + blob(`a:1:{s:1:"p";`+blob(`a:1:{s:1:"u";`+blob("https://a.test/x")+`}`)+`}`) + `}`},
		{"a span beside prose", `see https://a.test/b and ` + blob("https://a.test/x")},
		{"multibyte content, where bytes and runes differ",
			`a:1:{s:1:"u";` + blob("https://a.test/käyttö") + `}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := string(RepairSerialized([]byte(c.in), lengthen))
			if strings.Contains(out, "//a.test/") {
				t.Errorf("the rewrite did not reach every span, so this asserts little:\n%s", out)
			}
			assertEveryLength(t, out)
		})
	}
}

// assertEveryLength walks every `s:N:"` in s and checks the data closes exactly
// N bytes later — which is what PHP checks, and the only verdict that counts.
func assertEveryLength(t *testing.T, s string) {
	t.Helper()
	for i := 0; ; {
		j := strings.Index(s[i:], `s:`)
		if j < 0 {
			return
		}
		j += i
		k := j + 2
		for k < len(s) && s[k] >= '0' && s[k] <= '9' {
			k++
		}
		if k == j+2 || k+1 >= len(s) || s[k] != ':' || s[k+1] != '"' {
			i = j + 2
			continue
		}
		n, _ := strconv.Atoi(s[j+2 : k])
		start := k + 2
		if start+n >= len(s) || s[start+n] != '"' {
			t.Fatalf("s:%d: does not close where it says, so PHP refuses this:\n%s", n, s)
		}
		// Descend into the data, so a nested prefix is checked too.
		i = start
	}
}

// The rewrite must be applied exactly once to every byte. The nested call used
// to run over already-rewritten data and then re-apply the rewrite to its gaps,
// so `A` came back as `ABB`.
func TestTheRewriteIsAppliedOnce(t *testing.T) {
	grow := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), "A", "AB"))
	}
	in := `a:1:{s:1:"o";` + blob(`a:1:{s:1:"k";`+blob("A")+`}`) + `}`
	out := string(RepairSerialized([]byte(in), grow))
	if strings.Contains(out, "ABB") {
		t.Errorf("the rewrite ran twice over the same bytes:\n%s", out)
	}
	if !strings.Contains(out, `"AB"`) {
		t.Errorf("the rewrite did not reach the innermost value:\n%s", out)
	}
	assertEveryLength(t, out)
}

// Both spellings in one buffer. Stopping at the first that matched left the
// other rewritten by the gap handling and never repaired.
func TestBothSpellingsInOneBody(t *testing.T) {
	in := `opt1=` + blob("a.test") + `&opt2=s%3A6%3A%22a.test%22%3B`
	out := string(RepairSerialized([]byte(in), lengthen))
	if strings.Contains(out, "s:6:") || strings.Contains(out, "s%3A6%3A") {
		t.Errorf("a span was rewritten and left with its old length:\n%s", out)
	}
	assertEveryLength(t, out)
	if !strings.Contains(out, "s%3A12%3A") {
		t.Errorf("the percent-encoded span was not repaired:\n%s", out)
	}
}

// An identity map must not change a byte — test 24 — including the spelling of
// a length prefix it did not need to touch.
func TestAnIdentityRewriteChangesNothing(t *testing.T) {
	same := func(b []byte) []byte { return b }
	for _, in := range []string{
		blob("hello"),
		`s:05:"hello";`,          // a leading zero
		`s%3a5%3a%22hello%22%3b`, // lowercase hex
		`a:1:{s:1:"o";` + blob(`a:1:{s:1:"u";`+blob("x")+`}`) + `}`,
		`The value is s:6:"a.test"; and that is all.`,
		`s:99:"short";`, // a length that was already wrong before we saw it
		`s:-1:"x";`,
		`s:0:"";`,
		`s:5:"hel`, // truncated
		`o=%zz%3A`, // an invalid escape
	} {
		t.Run(in, func(t *testing.T) {
			if out := string(RepairSerialized([]byte(in), same)); out != in {
				t.Errorf("an identity rewrite changed bytes:\n in  %s\n out %s", in, out)
			}
		})
	}
}

// Prose that merely resembles a span must not stop the rest of the body being
// rewritten.
//
// The previous version of this asserted that an origin after a span-shaped
// quotation was rewritten — which is true whether the span is believed or not,
// because the gaps are rewritten either way. It passed under a mutation that
// removed the very guard it named. This uses a quotation that cannot parse, so
// the decline path is the one under test.
func TestProseThatCannotParseStillRewritesAroundIt(t *testing.T) {
	canon, variant := "https://a.test", "https://a.test.local"
	for _, in := range []string{
		`The value is s:6:"a.test and see ` + canon + `/x`, // no closing quote
		`Example: s:99:"short"; and see ` + canon + `/x`,   // length far past the data
		`Try s:abc:"x"; and see ` + canon + `/x`,           // not a number
	} {
		t.Run(in, func(t *testing.T) {
			got := string(RepairSerialized([]byte(in), func(b []byte) []byte {
				return []byte(strings.ReplaceAll(string(b), canon, variant))
			}))
			if strings.Contains(got, canon+"/x") {
				t.Errorf("an origin after an unparseable quotation was not rewritten:\n%s", got)
			}
		})
	}
}

// Past the depth limit the repair declines cleanly — it falls back to rewriting
// without repair, rather than repairing the outer levels and leaving the inner
// ones stale. Repairing part of a structure is the failure this file exists to
// prevent: the outer parses, so nothing errors, and only a later unserialize of
// the inner value returns false.
func TestPastTheDepthLimitDeclinesRatherThanPartlyRepairing(t *testing.T) {
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	canon, variant := "https://a.test/", "https://a.test.local/"
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}
	for _, depth := range []int{1, 8, 32, 33, 40} {
		in := str(canon)
		for i := 0; i < depth; i++ {
			in = str(in)
		}
		out := string(RepairSerialized([]byte(in), rw))
		if depth <= maxSerializedDepth {
			assertEveryLength(t, out)
			continue
		}
		// Beyond the limit: no repair at all, not a partial one.
		if out != string(rw([]byte(in))) {
			t.Errorf("depth %d was partly repaired instead of declined", depth)
		}
	}
}

// Depth is bounded, and hitting the bound must not corrupt anything — it just
// stops repairing further in.
func TestDeepNestingIsBounded(t *testing.T) {
	in := blob("x")
	for i := 0; i < 12; i++ {
		in = blob(in)
	}
	out := string(RepairSerialized([]byte(in), func(b []byte) []byte { return b }))
	if out != in {
		t.Errorf("an identity rewrite changed a deeply nested payload:\n in  %s\n out %s", in, out)
	}
}

// A stale length that lands on a `";` inside its own data must not be believed.
//
// This is how a valid wp_options row was destroyed. The HTML response arm does
// not repair, so the browser is served a blob whose declared length is stale;
// the request arm then trusted that length, found a `";` six bytes early — and
// `";` is the commonest two-byte sequence in serialized data holding HTML or
// CSS — and wrote a *different* wrong number. A round trip that used to
// self-heal became permanent corruption, `unserialize()` returning false.
//
// Declining a mis-parse is always safe: the caller falls back to rewriting
// without repair, which is what the code did before repair existed.
func TestAStaleLengthIsNotBelieved(t *testing.T) {
	// The arithmetic has to be exact or the fixture proves nothing, so it is
	// derived rather than written down. The row was valid at the canonical
	// host; the response arm rewrote to a variant six bytes longer and left the
	// length alone; the stale length now lands exactly on the `"` of the `";`
	// inside the data.
	const canon, variant = "https://x.ddev.site/x", "https://wt-a--x.ddev.site/x"
	declared := len(canon + `";abcd`) // correct before the host changed
	if declared != len(variant) {
		t.Fatalf("the fixture does not set up a false boundary: declared %d, "+
			"the quote sits at %d", declared, len(variant))
	}
	in := `a:1:{s:3:"css";s:` + strconv.Itoa(declared) + `:"` + variant + `";abcd";}`
	out := string(RepairSerialized([]byte(in), func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), variant, canon))
	}))
	// The number must be untouched, so the request direction restores exactly
	// what the response direction changed and the round trip comes home.
	if !strings.Contains(out, `s:`+strconv.Itoa(declared)+`:`) {
		t.Errorf("a stale length was rewritten from a false boundary:\n in  %s\n out %s", in, out)
	}
	if strings.Contains(out, variant) {
		t.Errorf("the host was not rewritten:\n%s", out)
	}
}

// A declared length larger than the buffer cannot be honest, and using it as an
// offset overflowed to a negative index — a panic, and a 502 from the proxy, on
// any request or response body carrying that byte sequence. Post and comment
// content can carry it.
func TestAnAbsurdLengthDoesNotPanic(t *testing.T) {
	for _, in := range []string{
		`a:1:{s:9223372036854775807:"x";}`,
		`a:1:{s:99999999999999999999:"x";}`,
		`s%3A9223372036854775807%3A%22x%22%3B`,
		`s:2147483647:"x";`,
	} {
		t.Run(in, func(t *testing.T) {
			out := string(RepairSerialized([]byte(in), func(b []byte) []byte { return b }))
			if out != in {
				t.Errorf("an identity rewrite changed bytes:\n in  %s\n out %s", in, out)
			}
		})
	}
}

// The failure that drove three rewrites of this file: a valid row, served
// through an arm that cannot repair, and posted back.
//
// The streamed HTML arm rewrites hosts without re-emitting lengths, so what the
// browser holds has every length stale. The request direction must put it back
// exactly — and to do that it has to *decline*, because the numbers it would
// compute are for the variant, not for what the row originally held.
//
// Earlier attempts guessed locally: believe the length if the span closes where
// it says, then also if what follows can follow a serialized value. Both were
// wrong, because `";}` and `";i:` are what correct serialized data looks like.
// Only walking the grammar separates a real boundary from a false one.
func TestAStaleBlobComesHomeUnchanged(t *testing.T) {
	canon, variant := "https://www.acmecorp.fi", "https://wt-a--acmecorp.ddev.site"
	toVariant := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}
	toCanon := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), variant, canon))
	}

	// Every length is computed. Three fixtures in this file have now been wrong
	// by a byte or two because they were written by hand, and a span that does
	// not parse exercises nothing — the test passes and proves nothing.
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	arr := func(parts ...string) string {
		return `a:` + strconv.Itoa(len(parts)/2) + `:{` + strings.Join(parts, "") + `}`
	}
	for _, row := range []string{
		// The shape the breaker measured: an array serialized inside a string,
		// so the stale outer length lands on the inner string's own `";`.
		str(arr(`i:0;`, str(canon), `i:1;`, `N;`)),
		// Three URLs, so the deltas accumulate.
		arr(`i:0;`, str(canon), `i:1;`, str(canon+"/a"), `i:2;`, str(canon+"/b")),
		// An object.
		`O:8:"stdClass":1:{` + str("u") + str(canon) + `}`,
		// Doubly serialized.
		str(str(arr(`i:0;`, str(canon), `i:1;`, `N;`))),
	} {
		t.Run(row, func(t *testing.T) {
			// Assert the fixture is what we think before asserting the result.
			assertEveryLength(t, row)
			// What a streamed arm serves: hosts swapped, no length re-emitted.
			wire := string(toVariant([]byte(row)))
			if wire == row {
				t.Fatal("the fixture does not contain the canonical host")
			}
			for _, ct := range []string{"literal", "percent"} {
				in := wire
				rw := toCanon
				if ct == "percent" {
					in = url.QueryEscape(wire)
					rw = func(b []byte) []byte {
						return []byte(strings.ReplaceAll(string(b),
							url.QueryEscape(variant), url.QueryEscape(canon)))
					}
				}
				got := string(RepairSerialized([]byte(in), rw))
				want := row
				if ct == "percent" {
					got, _ = url.QueryUnescape(got)
				}
				if got != want {
					t.Errorf("%s: the row did not come home:\n got  %s\n want %s", ct, got, want)
				}
			}
		})
	}
}

// A one-way write — content composed at the variant and posted, never served —
// must still have its lengths re-emitted, because nothing will restore them.
func TestAOneWayWriteIsRepaired(t *testing.T) {
	canon, variant := "https://www.acmecorp.fi", "https://wt-a--acmecorp.ddev.site"
	row := `a:1:{s:1:"u";s:` + strconv.Itoa(len(variant)) + `:"` + variant + `";}`
	assertEveryLength(t, row)
	got := string(RepairSerialized([]byte(row), func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), variant, canon))
	}))
	want := `a:1:{s:1:"u";s:` + strconv.Itoa(len(canon)) + `:"` + canon + `";}`
	if got != want {
		t.Errorf("a one-way write was not repaired:\n got  %s\n want %s", got, want)
	}
}

// A value that ends a line, an element or a CDATA section still repairs. The
// previous attempt required the next bytes to be a serialized token, which made
// the text and XML response arms stop repairing entirely.
func TestATrailingContextDoesNotPreventRepair(t *testing.T) {
	canon, variant := "https://a.test", "https://a.test.local"
	body := `s:` + strconv.Itoa(len(canon)) + `:"` + canon + `";`
	// A bare `"` is deliberately absent, and TestAQuoteAfterASpanIsDeclined says
	// why: it is the one trailing byte that cannot be told from the residue of a
	// truncated string.
	for _, tail := range []string{"", "\n", "\r\n", " ", "\t", "]", ",", "]]>", "</meta>", "&x=1"} {
		t.Run(strconv.Quote(tail), func(t *testing.T) {
			got := string(RepairSerialized([]byte(body+tail), func(b []byte) []byte {
				return []byte(strings.ReplaceAll(string(b), canon, variant))
			}))
			want := `s:` + strconv.Itoa(len(variant)) + `:"` + variant + `";` + tail
			if got != want {
				t.Errorf("a trailing %q prevented repair:\n got  %s\n want %s", tail, got, want)
			}
		})
	}
}

// The one trailing byte that costs a repair, and why that is the right way
// round.
//
// A value followed immediately by `"` is indistinguishable from a parse that
// stopped short inside a string: both leave a quote sitting where the walk
// finished. Believing it is how an ordinary `custom_css` option silently lost
// six bytes on every view-and-save, parsing cleanly each time so nothing ever
// reported it. Declining costs a repair on a shape that is rare — a serialized
// value abutting a quote with no separator — and buys back the one failure in
// this file that destroys data.
func TestAQuoteAfterASpanIsDeclined(t *testing.T) {
	canon, variant := "https://a.test", "https://a.test.local"
	body := `s:` + strconv.Itoa(len(canon)) + `:"` + canon + `";"`
	got := string(RepairSerialized([]byte(body), func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}))
	// The host is still rewritten — only the length is left alone.
	if !strings.Contains(got, variant) {
		t.Errorf("the host was not rewritten:\n%s", got)
	}
	if !strings.Contains(got, `s:`+strconv.Itoa(len(canon))+`:`) {
		t.Errorf("the length was re-emitted from a parse that may have been "+
			"truncated:\n%s", got)
	}
}

// The measured failure, kept as a fixture: an ordinary Customizer `custom_css`
// option whose CSS ends in `";}` and three newlines.
//
// The stale length lands on that `"`, the string closes, the very next `}`
// closes the array with its arity satisfied, and the parse is *structurally
// complete* — so walking the grammar is not by itself enough. What gives it
// away is the residue: the tail of the true string, left over after a parse
// that consumed a prefix. Each view-and-save trimmed six more bytes, and the
// row parsed cleanly every time, so nothing reported it.
func TestAnOrdinaryCustomCSSOptionSurvivesARoundTrip(t *testing.T) {
	canon, variant := "https://jz25.ddev.site", "https://wt-a--jz25.ddev.site"
	css := `.hero{background:url(` + canon + `/wp-content/uploads/bg.png);` +
		`font-family:"Inter";}` + "\n\n\n"
	seed := `a:1:{s:10:"custom_css";s:` + strconv.Itoa(len(css)) + `:"` + css + `";}`
	assertEveryLength(t, seed)

	// What a streamed arm serves: hosts swapped, no length re-emitted.
	wire := strings.ReplaceAll(seed, canon, variant)
	// Assert the setup really does create a false boundary, or this proves
	// nothing — three fixtures in this file have failed that way.
	stale := strings.Index(wire, `s:`+strconv.Itoa(len(css))+`:"`) + len(`s:`+strconv.Itoa(len(css))+`:"`)
	if wire[stale+len(css)] != '"' {
		t.Fatalf("the fixture does not put a quote at the stale offset; it has %q",
			wire[stale+len(css)])
	}

	back := string(RepairSerialized([]byte(wire), func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), variant, canon))
	}))
	if back != seed {
		t.Errorf("the row did not come home:\n got  %q\n want %q", back, seed)
	}
}

// An array whose contents do not match its declared arity is not something we
// understand, and must be declined rather than partly rebuilt.
//
// Stopping at the first child that fails and closing the array anyway would
// emit a *shorter* structure than arrived — which is the same class of silent
// data loss as believing a stale length, arrived at from the other direction.
func TestAnArrayMustSatisfyItsArity(t *testing.T) {
	canon, variant := "https://a.test", "https://a.test.local"
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	for _, c := range []struct{ name, in string }{
		{"declares two pairs, holds one", `a:2:{s:1:"u";` + str(canon) + `}`},
		{"declares one pair, holds none", `a:1:{}`},
		{"a child that does not parse", `a:1:{s:1:"u";s:99:"` + canon + `";}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := string(RepairSerialized([]byte(c.in), func(b []byte) []byte {
				return []byte(strings.ReplaceAll(string(b), canon, variant))
			}))
			// The host is still rewritten; nothing is re-emitted or dropped.
			want := strings.ReplaceAll(c.in, canon, variant)
			if got != want {
				t.Errorf("a malformed array was rebuilt rather than declined:\n"+
					" got  %s\n want %s", got, want)
			}
		})
	}
}
