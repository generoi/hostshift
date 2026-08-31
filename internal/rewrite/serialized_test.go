package rewrite

import (
	"encoding/json"
	"fmt"
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
	// Only what can end a *field*: nothing, whitespace, or the next `&`. A value
	// followed by `]`, `,`, `"` or markup is embedded in a larger document, and
	// there the walk cannot tell a short length from a real one — see
	// occupiesItsField and TestAnEmbeddedValueIsNotRepaired.
	for _, tail := range []string{"", "\n", "\r\n", " ", "\t", "&x=1"} {
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

// A value embedded in a larger document is rewritten but not re-emitted.
//
// The walk can only trust the lengths when it accounts for the whole field: if
// bytes are left over, some length was short and which one cannot be known.
// Inside a paragraph, a CDATA section or an XML element there is always
// something left over, so those decline — and the response direction declines
// for the same reason, which keeps the two consistent and the round trip exact.
//
// This is the cost of the field rule, and it is the right way round: the five
// pattern-matching rules it replaced each cost nothing here and destroyed rows
// instead.
func TestAnEmbeddedValueIsNotRepaired(t *testing.T) {
	canon, variant := "https://a.test", "https://a.test.local"
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	// A value that *fills* an element's text node or a CDATA section does
	// occupy its field, and repairs — the delimiters are matched, so the close
	// is expected rather than residue. That fixes the WXR export case round
	// twenty-seven had to document as a limitation. What stays declined is a
	// value with other content beside it, where the boundary is genuinely
	// unknowable.
	for _, c := range []struct{ name, in string }{
		{"beside prose", `see ` + canon + `/b and ` + str(canon+"/x")},
		{"with a second value after it", str(canon+"/x") + ` ` + str(canon+"/y")},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := string(RepairSerialized([]byte(c.in), func(b []byte) []byte {
				return []byte(strings.ReplaceAll(string(b), canon, variant))
			}))
			// `want` is the input with every origin rewritten and no length
			// touched, so comparing to it asserts both halves at once. A
			// Contains check cannot: the canonical host is a prefix of the
			// variant here, so it matches its own replacement.
			want := strings.ReplaceAll(c.in, canon, variant)
			if got != want {
				t.Errorf("an embedded value had its length re-emitted:\n got  %s\n want %s",
					got, want)
			}
		})
	}

	// And the case that moved out of that list: a value glued to the text in
	// front of it, with nothing after it. `v1|` is not one of the separators a
	// value may follow, so the position gate skipped it and the length went out
	// stale — the same shape as an ACF label written without punctuation.
	//
	// It is repaired now, by an arm that only ever accepts a value parsing
	// completely with nothing but whitespace behind it. That is why it belongs
	// here rather than in the list above: the boundary is *not* unknowable when
	// the value runs to the end of what it sits in.
	t.Run("glued to unrelated content, with nothing after it", func(t *testing.T) {
		in := `v1|` + str(canon+"/x")
		want := `v1|` + str(variant+"/x")
		rw := func(b []byte) []byte {
			return []byte(strings.ReplaceAll(string(b), canon, variant))
		}
		got := string(RepairSerialized([]byte(in), rw))
		if got != want {
			t.Errorf("\n got  %s\n want %s", got, want)
		}
		// And it round-trips, which is what makes repairing it safe.
		back := string(RepairSerialized([]byte(got), func(b []byte) []byte {
			return []byte(strings.ReplaceAll(string(b), variant, canon))
		}))
		if back != in {
			t.Errorf("did not round-trip:\n got  %s\n want %s", back, in)
		}
	})

	// The other half of that arm: with something after the value, the boundary
	// is unknowable again and it must decline. Accepting a glued value with
	// residue is what would make the arm dangerous rather than additive —
	// the parse can stop at a false close, and there is nothing in front of the
	// value to say where it really began.
	t.Run("glued to unrelated content, with more after it", func(t *testing.T) {
		in := `v1|` + str(canon+"/x") + ` (cachad)`
		want := strings.ReplaceAll(in, canon, variant)
		got := string(RepairSerialized([]byte(in), func(b []byte) []byte {
			return []byte(strings.ReplaceAll(string(b), canon, variant))
		}))
		if got != want {
			t.Errorf("a glued value with residue had its length re-emitted:"+
				"\n got  %s\n want %s", got, want)
		}
	})
}

// The shapes round twenty-six measured as still corrupting, in both spellings.
//
// The residue rule caught roughly two thirds of false boundaries; these are
// from the other third, where the truncated string's remainder begins with an
// ordinary character — a space, a `.`, a letter — rather than the `"`, `;` or
// `}` the rule looked for. `";` followed by a space is ordinary CSS.
func TestTheResidueRulesBlindSpotComesHome(t *testing.T) {
	canon, variant := "https://www.example.fi", "https://wt-a--example.ddev.site"
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	css := `.a{background:url(` + canon + `/a.png);content:"x"; c:red}`

	for _, seed := range []string{
		str(css),
		`a:1:{s:10:"custom_css";` + str(css) + `}`,
		str(`.b{background:url(` + canon + `/b.png);content:"y";color:red}.c{d:1}`),
	} {
		t.Run(seed[:40], func(t *testing.T) {
			assertEveryLength(t, seed)
			wire := strings.ReplaceAll(seed, canon, variant)
			if wire == seed {
				t.Fatal("the fixture does not contain the canonical host")
			}
			got := string(RepairSerialized([]byte(wire), func(b []byte) []byte {
				return []byte(strings.ReplaceAll(string(b), variant, canon))
			}))
			if got != seed {
				t.Errorf("the row did not come home:\n got  %s\n want %s", got, seed)
			}
			// And the spelling a form actually sends.
			penc := url.QueryEscape(wire)
			pgot := string(RepairSerialized([]byte(penc), func(b []byte) []byte {
				return []byte(strings.ReplaceAll(string(b),
					url.QueryEscape(variant), url.QueryEscape(canon)))
			}))
			if dec, err := url.QueryUnescape(pgot); err != nil || dec != seed {
				t.Errorf("percent spelling did not come home:\n got  %s\n want %s", dec, seed)
			}
		})
	}
}

// A string that merely *begins* like a header is an ordinary string.
//
// Deciding on shape — `valueStart` — declined on `a:hover{color:red}`,
// `d:\shares\logo.png`, `i:12345` and `O:brien`, all of which are ordinary
// values. And because a decline abandoned repair for the whole buffer, one such
// field left every other option in the same POST with a stale length. What
// separates them is whether the inner parse *committed*: got past a real length
// and its opening delimiter. `a:hover` has no length, so it never commits.
func TestAStringThatMerelyLooksLikeAHeaderIsRepaired(t *testing.T) {
	canon, variant := "https://hs26.test", "https://wt-a--hs26.test"
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	for _, css := range []string{
		`a:hover{color:red}`, `d:\shares\logo.png`, `i:12345`, `O:brien`,
		`b:before{content:""}`, `N;not really`, `color:red`,
	} {
		t.Run(css, func(t *testing.T) {
			in := `a:2:{s:4:"logo";` + str(variant+"/logo.png") + `s:3:"css";` + str(css) + `}`
			want := `a:2:{s:4:"logo";` + str(canon+"/logo.png") + `s:3:"css";` + str(css) + `}`
			got := string(RepairSerialized([]byte(in), func(b []byte) []byte {
				return []byte(strings.ReplaceAll(string(b), variant, canon))
			}))
			if got != want {
				t.Errorf("a value beginning %q stopped the repair:\n got  %s\n want %s",
					css[:2], got, want)
			}
		})
	}
}

// One field must not contaminate its neighbours.
//
// `options.php` posts every option on a settings page in one body. A decline
// abandoned repair buffer-wide, so an option holding `a:hover{color:red}` — no
// hostname in it at all — destroyed the length of a different option that did.
func TestOneFieldDoesNotContaminateAnother(t *testing.T) {
	canon, variant := "https://hs26.test", "https://wt-a--hs26.test"
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	// Deliberately unparseable, so this field really does decline.
	bad := `opt_b=a:9:{s:3:"css";` + str(`x`) + `}`
	good := `opt_a=a:1:{s:4:"logo";` + str(variant+"/logo.png") + `}`
	wantGood := `opt_a=a:1:{s:4:"logo";` + str(canon+"/logo.png") + `}`

	// RepairSerializedFields, because contamination is a form-body concern and
	// the split now comes from the caller's content type rather than a guess
	// about the bytes.
	got := string(RepairSerializedFields([]byte(good+"&"+bad), func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), variant, canon))
	}))
	if !strings.HasPrefix(got, wantGood) {
		t.Errorf("a neighbouring field's decline destroyed this one:\n got  %s\n want %s…",
			got, wantGood)
	}
}

// `&` is a field separator only when it introduces a `key=` pair. A value can
// carry `&#47;` — which is exactly what the response direction emits into an
// href — and breaking there cut the origin in half so nothing could find it.
func TestAReferenceIsNotAFieldSeparator(t *testing.T) {
	canon, variant := "https://hs26.test", "https://wt-a--hs26.test"
	in := `content=<a href="https:&#47;&#47;` + variant + `/x">t</a>&next=1`
	got := string(RepairSerialized([]byte(in), func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), variant, canon))
	}))
	if strings.Contains(got, variant) {
		t.Errorf("an origin spanning a character reference was not rewritten:\n%s", got)
	}
}

// The types `serialize()` emits that had no case: references, repeated object
// instances, enums, and custom serialization.
//
// Without them an array holding one failed to parse, which cost the repair for
// that whole field — and `R:` in particular is emitted routinely, whenever the
// same value appears twice in a structure.
func TestTheOtherSerializedTypesParse(t *testing.T) {
	canon, variant := "https://hs27.test", "https://wt-a--hs27.test"
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	for _, c := range []struct{ name, extra string }{
		{"a reference", `i:1;R:2;`},
		{"a repeated object", `i:1;r:2;`},
		{"an enum", `i:1;E:11:"Suit:Hearts";`},
		{"custom serialization", `i:1;C:3:"Foo":4:{abcd}`},
		{"a float", `i:1;d:1.5E+3;`},
		{"INF", `i:1;d:INF;`},
		{"null", `i:1;N;`},
		{"a bool", `i:1;b:1;`},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := `a:2:{i:0;` + str(variant+"/x") + c.extra + `}`
			want := `a:2:{i:0;` + str(canon+"/x") + c.extra + `}`
			got := string(RepairSerialized([]byte(in), func(b []byte) []byte {
				return []byte(strings.ReplaceAll(string(b), variant, canon))
			}))
			if got != want {
				t.Errorf("%s stopped the repair:\n got  %s\n want %s", c.name, got, want)
			}
		})
	}
}

// A `C:` payload is arbitrary bytes and must be rewritten like any other.
//
// Copying it verbatim sent a production URL inside a WooCommerce-shaped blob to
// the browser while the structure around it was rewritten, and wrote the
// variant hostname into the database on the way back. Before `C:` was handled
// at all, the field declined and the whole-buffer rewrite covered the payload —
// so adding the case traded a stale length for a live origin, which is the
// worse of the two.
//
// The previous fixture for this type used `C:3:"Foo":4:{abcd}`, whose payload
// holds no hostname, so it asserted only that the type does not stop the repair
// — never the question `C:` actually raises.
func TestACustomPayloadIsRewritten(t *testing.T) {
	canon, variant := "https://www.example.fi", "https://v.ddev.site"
	for _, c := range []struct{ name, in, want string }{
		{"a bare custom value",
			`C:7:"WC_Data":` + strconv.Itoa(len(canon)) + `:{` + canon + `}`,
			`C:7:"WC_Data":` + strconv.Itoa(len(variant)) + `:{` + variant + `}`},
		// No `;` after the `}` — `C:` has no terminator, unlike every other
		// type. Writing one made the array fail to parse, so the fixture fell
		// back to the whole-buffer rewrite and measured nothing.
		{"inside an array",
			`a:1:{i:0;C:7:"WC_Data":` + strconv.Itoa(len(canon)) + `:{` + canon + `}}`,
			`a:1:{i:0;C:7:"WC_Data":` + strconv.Itoa(len(variant)) + `:{` + variant + `}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := string(RepairSerialized([]byte(c.in), func(b []byte) []byte {
				return []byte(strings.ReplaceAll(string(b), canon, variant))
			}))
			if strings.Contains(got, canon) {
				t.Errorf("an origin inside a custom payload was not rewritten:\n%s", got)
			}
			if got != c.want {
				t.Errorf("\n got  %s\n want %s", got, c.want)
			}
		})
	}
}

// A `&name=` inside a URL query string is not a field separator.
//
// Guessing field boundaries from the bytes cut a serialized value in half at
// the `&utm_medium=` of an ordinary tracking URL — both halves declined and the
// length was re-emitted from neither, so a one-way write put a stale length
// into wp_postmeta permanently. The split now comes from the caller knowing the
// body is urlencoded, where a `&` inside a value is `%26`.
func TestAQueryStringIsNotAFieldBoundary(t *testing.T) {
	canon, variant := "https://hs27.test", "https://wt-a--hs27.test"
	u := variant + "/landing/?utm_source=news&utm_medium=email"
	in := `a:1:{s:3:"url";s:` + strconv.Itoa(len(u)) + `:"` + u + `";}`
	cu := strings.ReplaceAll(u, variant, canon)
	want := `a:1:{s:3:"url";s:` + strconv.Itoa(len(cu)) + `:"` + cu + `";}`

	got := string(RepairSerialized([]byte(in), func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), variant, canon))
	}))
	if got != want {
		t.Errorf("a query string was treated as a field boundary:\n got  %s\n want %s",
			got, want)
	}
}

// The vehicles a serialized option actually travels in, and the delimiters that
// pair with each.
//
// Requiring a value to sit tight against `&` or `=` accepted only a blob
// written flush against its attribute quote. An indented `<textarea>`, a text
// node, a CDATA section and `wp_localize_script`'s `{"opt":"…"}` were all found,
// parsed correctly, and thrown away — and the throw-away is a whole-buffer
// decline, so the host was rewritten under the old length. That was four of six
// realistic vehicles, including the one the commit claiming to have fixed this
// named.
//
// Accepting any quote to fix that let a truncation residue through again: a
// trailing `"` is both a legitimate close and the remains of a parse that
// stopped short. Which one it is depends on how the value opened, so the opener
// chooses what may close it.
func TestTheVehiclesAValueTravelsIn(t *testing.T) {
	canon, variant := "https://www.acmecorp.fi", "https://wt-a--acmecorp.ddev.site"
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	blob := func(host string) string { return `a:1:{s:3:"url";` + str(host+"/a") + `}` }
	esc := func(v string) string { return strings.ReplaceAll(v, `"`, "&quot;") }
	jsn := func(v string) string { return strings.ReplaceAll(v, `"`, `\"`) }

	for _, c := range []struct {
		name, in, want string
	}{
		{"tight in an attribute",
			`<input value="` + esc(blob(canon)) + `">`,
			`<input value="` + esc(blob(variant)) + `">`},
		{"an indented textarea",
			"\n  " + esc(blob(canon)) + "\n",
			"\n  " + esc(blob(variant)) + "\n"},
		{"a JSON string, as wp_localize_script writes it",
			`{"opt":"` + jsn(blob(canon)) + `"}`,
			`{"opt":"` + jsn(blob(variant)) + `"}`},
		{"a CDATA section, as a WXR export writes it",
			`<![CDATA[` + blob(canon) + `]]>`,
			`<![CDATA[` + blob(variant) + `]]>`},
		{"an element's whole text node",
			`<meta>` + blob(canon) + `</meta>`,
			`<meta>` + blob(variant) + `</meta>`},
		{"its own field in a form body",
			`opt=` + blob(canon),
			`opt=` + blob(variant)},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := string(RepairSerialized([]byte(c.in), func(b []byte) []byte {
				return []byte(strings.ReplaceAll(string(b), canon, variant))
			}))
			if got != c.want {
				t.Errorf("\n got  %s\n want %s", got, c.want)
			}
		})
	}
}

// An ampersand under esc_attr, both readings.
//
// `esc_attr` runs with `$double_encode = false`, so an `&amp;` already in the
// data passes through as its five literal bytes — and the serialized length
// counts five — while a bare `&` in the data becomes `&amp;` and counts one.
// The two are identical in the attribute.
//
// Neither can be chosen locally, and both attempts to try were wrong in one
// direction or the other: counting every reference as one byte destroyed the
// first reading, and counting only the quote entity declined the second, which
// still writes the old length onto a rewritten value. So the closing delimiter
// decides — the reading that lands on `&quot;;` is the one the data was written
// under — and where both land, nothing is chosen.
func TestAnAmpersandUnderEscAttr(t *testing.T) {
	canon, variant := "https://www.canon.test", "https://v.ddev.site"
	esc := func(v string) string { return strings.ReplaceAll(v, `"`, "&quot;") }
	blob := func(data string) string {
		return `a:1:{s:3:"url";s:` + strconv.Itoa(len(data)) + `:"` + data + `";}`
	}
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}

	t.Run("a literal &amp; in the data repairs", func(t *testing.T) {
		data := canon + "/shop/?add-to-cart=42&amp;q=1"
		vdata := strings.ReplaceAll(data, canon, variant)
		in := `<input value="` + esc(blob(data)) + `">`
		want := `<input value="` + esc(blob(vdata)) + `">`
		if got := string(RepairSerialized([]byte(in), rw)); got != want {
			t.Errorf("\n got  %s\n want %s", got, want)
		}
	})

	t.Run("a bare & in the data repairs", func(t *testing.T) {
		// The data holds one `&`, which esc_attr writes as five bytes. Reading
		// those five as one lands on the closing `&quot;;`; reading them as five
		// lands four bytes short, in the middle of the text. Only one closes, so
		// only one was ever the reading the data was written under.
		data := canon + "/shop/?a=1&b=2"
		vdata := strings.ReplaceAll(data, canon, variant)
		enc := func(v string) string {
			return strings.ReplaceAll(esc(blob(v)), "&b=2", "&amp;b=2")
		}
		in := `<input value="` + enc(data) + `">`
		want := `<input value="` + enc(vdata) + `">`
		if got := string(RepairSerialized([]byte(in), rw)); got != want {
			t.Errorf("\n got  %s\n want %s", got, want)
		}
	})

	t.Run("a value with two readings is decided by the enclosing parse", func(t *testing.T) {
		// Found by exhaustive search over every string up to length six: with
		// `aa<";a` the literal reading of `&lt;` closes at the first `&quot;;`
		// and the escaped reading closes at the second. Locally nothing ranks
		// them — which is why this used to decline.
		//
		// The array around it does rank them. Only one reading lets `a:1:{…}`
		// parse to its end and fill its field, so both are tried and the one
		// that survives is the answer. Declining here was not neutral: it left
		// the host rewritten under the length it arrived with.
		data := `aa<";a`
		enc := func(host string) string {
			return `<input value="` + strings.NewReplacer(
				`"`, "&quot;", "<", "&lt;").Replace(blob(host+data)) + `">`
		}
		got := string(RepairSerialized([]byte(enc(canon)), rw))
		if want := enc(variant); got != want {
			t.Errorf("\n got  %s\n want %s", got, want)
		}
		if n := BrokenSerialized([]byte(enc(variant))); n != 0 {
			t.Errorf("the repaired page counted %d broken values", n)
		}
	})
}

// `esc_attr(wp_json_encode(…))` — the spelling neither of its halves can read.
//
// The JSON escaping runs first, turning `"` into `\"`; `esc_attr` then escapes
// that quote, giving `\&quot;`. Elementor's `data-settings`, WooCommerce block
// attributes and ACF all emit it. Unread, the value was rewritten without
// repair — and the *canonical* page counted as broken too, so the corpus diff's
// baseline subtraction cancelled a real defect against its own blind spot and
// reported a page PHP refuses as GREEN. A false GREEN hides the class the check
// exists for; it is worse than the false RED it replaced.
func TestTheCombinedJSONAndHTMLSpelling(t *testing.T) {
	canon, variant := "https://mz28a.ddev.site", "https://wt-a--mz28a.ddev.site"
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	blob := func(h string) string { return `a:1:{s:4:"home";` + str(h) + `}` }
	// json.Marshal then esc_attr, exactly as WordPress composes them.
	comb := func(v string) string {
		j, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(j[1:len(j)-1]), `"`, "&quot;")
	}
	in := `<div data-settings="` + comb(blob(canon)) + `"></div>`
	want := `<div data-settings="` + comb(blob(variant)) + `"></div>`

	got := string(RepairSerialized([]byte(in), func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}))
	if got != want {
		t.Errorf("\n got  %s\n want %s", got, want)
	}
	// And the detector must not call either side broken.
	if n := BrokenSerialized([]byte(in)); n != 0 {
		t.Errorf("the canonical page counted %d broken values; a blind spot here "+
			"credits the baseline and silences the variant side", n)
	}
	if n := BrokenSerialized([]byte(want)); n != 0 {
		t.Errorf("the repaired page counted %d broken values", n)
	}
}

// The cheap gate must never say no to something the walk would repair.
func TestTheSerializedGateNeverMissesAValue(t *testing.T) {
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	for _, in := range []string{
		str("x"),
		`a:1:{i:0;` + str("x") + `}`,
		`s%3A1%3A%22x%22%3B`,
		strings.ReplaceAll(str("x"), `"`, "&quot;"),
		strings.ReplaceAll(str("x"), `"`, `\"`),
		strings.ReplaceAll(str("x"), `"`, `\&quot;`),
		`N;`,
		`C:3:"Foo":1:{x}`,
	} {
		t.Run(in, func(t *testing.T) {
			if !mayHoldSerialized([]byte(in)) {
				t.Errorf("the gate refused a value the walk repairs: %s", in)
			}
		})
	}
	// And it should say no to ordinary content, or it buys nothing.
	for _, in := range []string{
		`<a href="https://x/a">t</a>`,
		`.hero{background:url(https://x/a.png)}`,
		`{"url":"https://x/a"}`,
	} {
		if mayHoldSerialized([]byte(in)) {
			t.Errorf("the gate admitted ordinary content, so it saves nothing: %s", in)
		}
	}
}

// A serialized payload nested inside a string does not have to start at byte
// zero of that string, and when it does not, its length is left stale while the
// string around it is re-measured.
//
// repairString tries the nested parse at offset 0 of the data and nowhere else.
// One byte of prefix — a newline, a space, the `<div data-x="` of an Elementor
// fragment stored in an option — and the parse fails without committing, so the
// data falls to the plain `rw` and the nested `s:N:` keeps the number it had
// while its bytes change underneath it. The outer length is then re-emitted
// correctly, which is what makes this the exact outcome repairString's own
// comment names as "the worst available": the outer parses, so nothing errors,
// and the failure surfaces on a later unserialize of the inner value.
//
// Verified against PHP 8.4: the input's inner value unserializes to an array,
// the served output's returns false.
//
// The assertion is fix-agnostic. Repairing the nested value passes; so does
// declining the whole field, because a decline leaves the outer length stale
// too and BrokenSerialized then reports it. What must not happen is the third
// outcome — corrupt bytes that the detector calls clean.
func TestANestedPayloadThatDoesNotStartTheStringKeepsItsLength(t *testing.T) {
	canon, variant := "https://www.canon.test/a", "https://v.ddev.site/a"
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	inner := func(h string) string { return `a:1:{s:3:"url";` + str(h) + `}` }
	outer := func(data string) string { return `a:1:{s:3:"raw";` + str(data) + `}` }
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}

	for _, c := range []struct{ name, prefix, suffix string }{
		{"a leading newline", "\n", ""},
		{"a leading space", " ", ""},
		{"an attribute around it", `<div data-x="`, `"></div>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := outer(c.prefix + inner(canon) + c.suffix)
			want := outer(c.prefix + inner(variant) + c.suffix)
			got := string(RepairSerialized([]byte(in), rw))
			if got == want {
				return
			}
			if n := BrokenSerialized([]byte(got)); n == 0 {
				t.Errorf("the nested length was not re-emitted and the detector "+
					"calls the served bytes clean:\n in   %s\n got  %s\n want %s",
					in, got, want)
			}
		})
	}
}

// BrokenSerialized stops at the top-level value, so a stale length inside a
// string is invisible to it.
//
// The detector exists because `hostshift diff` compared the proxy's output
// against the scorer's reimplementation of it, so a defect in both scored GREEN.
// It asserts on the served bytes — but only on the outermost value: once that
// parses, the walk skips to its end and never looks at what the strings hold.
// A nested payload is where WordPress keeps most of its serialized data, and
// where the repair above leaves a stale number.
//
// The fixture is hostshift's own output for the case above, with every length
// computed rather than written down: the outer describes its data exactly, and
// the inner declares the canonical host's byte count over the variant's.
func TestTheDetectorSeesAStaleLengthInsideAString(t *testing.T) {
	canon, variant := "https://www.canon.test/a", "https://v.ddev.site/a"
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	// The inner value as the repair leaves it: the variant host under the
	// canonical host's length. PHP returns false for this.
	stale := `a:1:{s:3:"url";s:` + strconv.Itoa(len(canon)) + `:"` + variant + `";}`
	if len(canon) == len(variant) {
		t.Fatal("the two hosts are the same length, so this fixture proves nothing")
	}
	data := "\n" + stale
	in := `a:1:{s:3:"raw";` + str(data) + `}`

	if n := BrokenSerialized([]byte(in)); n == 0 {
		t.Errorf("a value PHP refuses counted as clean, so the corpus diff "+
			"reports GREEN on it:\n%s", in)
	}
}

// A blob written by esc_attr(wp_json_encode(...)) as a JSON object *value* is
// repaired.
//
// It pins which delimiter opens the field in the combined spelling, which is
// not the one it looks like: the `\&quot;` are the value's own quotes, and the
// quote that opens the field is the structural `&quot;`. Round 28 assumed
// otherwise and gave the spelling an opener of its own; it was unreachable, and
// removing it is what this test holds in place. Checked against every shape
// wp_json_encode can put a string in — bare, object value, array element,
// nested object — the preceding bytes are `&quot;` in all four.
func TestASerializedBlobInsideAJSONStringAttribute(t *testing.T) {
	canon, variant := "https://mz29a.ddev.site", "https://wt-a--mz29a.ddev.site"
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	// esc_attr(wp_json_encode(["url" => 's:NN:"…";'])), so the blob is a JSON
	// string value and its quotes are opened and closed by `\&quot;`.
	comb := func(v string) string {
		j, err := json.Marshal(map[string]string{"url": v})
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(j), `"`, "&quot;")
	}
	in := `<div data-settings="` + comb(str(canon)) + `"></div>`
	want := `<div data-settings="` + comb(str(variant)) + `"></div>`

	got := string(RepairSerialized([]byte(in), func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}))
	if got != want {
		t.Errorf("\n got  %s\n want %s", got, want)
	}
	if n := BrokenSerialized([]byte(want)); n != 0 {
		t.Errorf("the repaired page counted %d broken values", n)
	}
}

// A nested payload with text after it is repaired, and a nested payload whose
// length overruns its data is reported.
//
// This replaces an assertion that was simply wrong. It held that a payload
// followed by other bytes must be declined, because a stale length consumes a
// prefix of its data and closes cleanly, so residue is the signature. PHP says
// otherwise: unserialize stops at the end of the first complete value and
// ignores whatever follows, without an error. So residue is not evidence of
// anything, and the fixture that test called broken parses at both levels.
//
// Declining on it made the walk disagree with PHP, and that disagreement is
// what destroyed pages: `serialize([...]) . " (cachad)"` in a field is ordinary
// content, and it was served with every length stale. What is actually broken
// is a length that overruns its data, and that is what the detector must see.
func TestTrailingTextIsRepairedAndAnOverrunIsReported(t *testing.T) {
	canon, variant := "https://mz31f.ddev.site", "https://wt-a--mz31f.ddev.site"
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	nest := func(host, suffix string) string {
		inner := `a:1:{` + str("u") + str(host+"/x") + `}` + suffix
		return `a:1:{` + str("a") + str(inner) + `}`
	}
	for name, suffix := range map[string]string{
		"none": "", "prose": " (cachad)", "punctuation": "!", "quote": `"`,
	} {
		in, want := nest(canon, suffix), nest(variant, suffix)
		if got := string(RepairSerialized([]byte(in), rw)); got != want {
			t.Errorf("%s:\n got  %s\n want %s", name, got, want)
		}
		if n := BrokenSerialized([]byte(want)); n != 0 {
			t.Errorf("%s: the detector called %d value(s) broken on bytes PHP accepts", name, n)
		}
	}

	// And the real defect: a declared length longer than the data it describes.
	// The value cannot parse at all, at either level, and PHP refuses it.
	inner := `a:1:{` + str("u") + `s:99:"` + canon + `/x";}`
	over := `a:1:{` + str("a") + str(inner) + `}`
	if n := BrokenSerialized([]byte(over)); n == 0 {
		t.Errorf("a length overrunning its data was reported clean:\n %s", over)
	}
}

// A length-declaring header of any type raises the broken count and declines
// its field.
//
// The commit gate listed `s`, `a` and `O` only, so a custom-serialized or enum
// header whose length did not describe its data was invisible to the detector
// and, worse, did not stop the scan: it carried on at the next byte and could
// repair spans *inside* the opaque payload, which is what repairCustom exists
// to avoid. The count is compared against the same defect written as a string,
// because the point is that the type must not change the answer.
func TestEveryLengthPrefixedTypeCommits(t *testing.T) {
	host := "https://mz29c.ddev.site/a"
	over := strconv.Itoa(len(host) + 3) // a length that overruns its data
	cases := map[string]string{
		"custom": `C:3:"Foo":` + over + `:{` + host + `}`,
		"enum":   `E:` + over + `:"` + host + `";`,
		"string": `a:1:{s:1:"x";s:` + over + `:"` + host + `";}`,
	}
	for name, body := range cases {
		if n := BrokenSerialized([]byte(body)); n == 0 {
			t.Errorf("%s: a length that overruns its data was reported clean:\n %s", name, body)
		}
	}
}

// The content a real site actually has, in the two spellings an attribute
// carries it in.
//
// Every one of these was served with a length PHP refuses. `wp_json_encode`
// writes non-ASCII as `\uXXXX`, six source bytes for one to three decoded ones,
// and the walk declined rather than measure it — so every ä, ö and å on the
// fleet. `esc_attr` writes `&`, `<` and `'` as references, and the walk charged
// them their literal width — so every client name with an ampersand in it.
//
// A decline is not neutral, which is the part the old comments had wrong: it
// still rewrites the host and re-emits nothing, leaving the old length on a
// value whose byte count has changed.
func TestTheContentARealSiteHas(t *testing.T) {
	canon, variant := "https://mz29d.ddev.site", "https://wt-a--mz29d.ddev.site"
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}
	// serialize(["u" => host, "t" => text]), lengths in bytes as PHP counts them.
	blob := func(host, text string) string {
		return `a:2:{s:1:"u";s:` + strconv.Itoa(len(host)) + `:"` + host + `";` +
			`s:1:"t";s:` + strconv.Itoa(len(text)) + `:"` + text + `";}`
	}
	// esc_attr with $double_encode = false.
	escAttr := strings.NewReplacer(
		`"`, "&quot;", `'`, "&#039;", "&", "&amp;", "<", "&lt;", ">", "&gt;")
	spellings := map[string]func(string) string{
		"esc_attr(serialize)": escAttr.Replace,
		"esc_attr(wp_json_encode(serialize))": func(s string) string {
			j, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			return escAttr.Replace(string(j))
		},
	}
	texts := map[string]string{
		"ascii":  "Read more",
		"nordic": "Läs mer",
		"amp":    "Snellman & Co",
		"apos":   "Genero's",
		"lt":     "a < b",
		"emoji":  "ok \U0001F389",
	}
	for sname, enc := range spellings {
		for tname, text := range texts {
			in := `<div data-x="` + enc(blob(canon, text)) + `">y</div>`
			want := `<div data-x="` + enc(blob(variant, text)) + `">y</div>`
			got := string(RepairSerialized([]byte(in), rw))
			if got != want {
				t.Errorf("%s / %s:\n got  %s\n want %s", sname, tname, got, want)
			}
			if n := BrokenSerialized([]byte(got)); n != 0 {
				t.Errorf("%s / %s: served %d value(s) PHP refuses:\n %s", sname, tname, n, got)
			}
		}
	}
}

// phpJSONEncode escapes the way PHP's json_encode does by default: every
// non-ASCII rune as `\uXXXX`, a rune outside the BMP as a surrogate pair, and
// the solidus as `\/`.
//
// Go's json.Marshal does the opposite — it leaves non-ASCII raw and escapes
// `<`, `>` and `&` — so a fixture built with it contains no `\uXXXX` at all.
// Two mutation tests passed against such a fixture while the `\u` measurement
// was disabled, which is what this exists to prevent.
func phpJSONEncode(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '/':
			b.WriteString(`\/`)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		case r < 0x80:
			b.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// A serialized blob inside a plain JSON string — wp_localize_script, a REST
// response, an admin-ajax reply — with the non-ASCII escaped as PHP writes it.
//
// `\uXXXX` is six source bytes for one to three decoded ones, and twelve for a
// surrogate pair. The walk used to refuse to measure it and decline, under a
// comment claiming that "never writes a wrong length" — but a decline rewrites
// the host and re-emits nothing, so the old length stayed on a value that had
// changed size. That is every ä, ö and å on the fleet.
func TestPHPJSONEscapedContent(t *testing.T) {
	canon, variant := "https://mz29e.ddev.site", "https://wt-a--mz29e.ddev.site"
	// json_encode escapes the solidus, so the host reaches the rewriter as
	// `https:\/\/…`. The engine's decoder view sees through that; a plain
	// string replace has to be told.
	esc := func(v string) string { return strings.ReplaceAll(v, "/", `\/`) }
	rw := func(b []byte) []byte {
		s := strings.ReplaceAll(string(b), canon, variant)
		return []byte(strings.ReplaceAll(s, esc(canon), esc(variant)))
	}
	blob := func(host, text string) string {
		return `a:2:{s:1:"u";s:` + strconv.Itoa(len(host)) + `:"` + host + `";` +
			`s:1:"t";s:` + strconv.Itoa(len(text)) + `:"` + text + `";}`
	}
	for name, text := range map[string]string{
		"ascii":     "Read more",
		"nordic":    "Läs mer",
		"threeByte": "日本語",
		"surrogate": "ok \U0001F389",
		"mixed":     "Snellman & Co — Läs mer \U0001F389",
	} {
		in := `{"settings":` + phpJSONEncode(blob(canon, text)) + `}`
		want := `{"settings":` + phpJSONEncode(blob(variant, text)) + `}`
		if got := string(RepairSerialized([]byte(in), rw)); got != want {
			t.Errorf("%s:\n got  %s\n want %s", name, got, want)
		}
		if n := BrokenSerialized([]byte(want)); n != 0 {
			t.Errorf("%s: the repaired page counted %d broken values", name, n)
		}
	}
}

// One ambiguous reference in a value of ordinary size, which is all it takes.
//
// advanceReadings explores states of (offset, remaining), and a single
// ambiguous `&amp;` gives every offset after it two of them — so the state
// count is about twice the length of the value, and maxStringReadings = 2048 is
// reached at roughly a kilobyte of data with *one* ampersand in it. Past that
// the search returns nothing, stringEnd reads that as "no reading closes", and
// the field declines: the host is rewritten and the length is not re-emitted,
// which is the corruption this file exists to prevent. `unserialize()` on the
// served bytes returns false.
//
// A kilobyte is nothing. A widget's text, an ACF field, a theme mod, a
// wp_localize_script blob — and "Snellman & Co" is the very example the reading
// search was added for. Below the cap it repairs; above it, silently, it does
// not.
//
// And nothing sees it. How many readings a value has does not depend on which
// host is in it, so the cap trips on the canonical page too and
// BrokenSerialized scores both sides alike — which the corpus diff's baseline
// subtraction then cancels to zero. A blind spot that fires on both sides has
// been the root cause twice.
func TestALongValueWithOneAmpersandIsStillRepaired(t *testing.T) {
	canon, variant := "https://www.canon.test", "https://v.ddev.site"
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	// esc_attr, with $double_encode = false: the data holds a bare `&`, so five
	// bytes reach the attribute where the declared length counts one.
	esc := strings.NewReplacer(`"`, "&quot;", "&", "&amp;").Replace
	blob := func(h, text string) string {
		return `a:1:{s:4:"text";` + str(text+" "+h+"/x") + `}`
	}
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}
	for _, c := range []struct {
		name string
		text string
	}{
		{"under a kilobyte", "Snellman & Co. " + strings.Repeat("lorem ipsum dolor sit amet, ", 20)},
		{"over a kilobyte", "Snellman & Co. " + strings.Repeat("lorem ipsum dolor sit amet, ", 40)},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := `<input value="` + esc(blob(canon, c.text)) + `">`
			want := `<input value="` + esc(blob(variant, c.text)) + `">`
			if got := string(RepairSerialized([]byte(in), rw)); got != want {
				t.Errorf("a %d-byte value was not repaired:\n got  %s\n want %s",
					len(c.text), got, want)
			}
			// The detector has to see the same thing PHP does. It scores the
			// canonical page identically, so the corpus diff subtracts this
			// away and reports GREEN on a page unserialize() refuses.
			if n := BrokenSerialized([]byte(in)); n != 0 {
				t.Errorf("the canonical page counted %d broken values; a blind spot "+
					"here credits the baseline and silences the variant side", n)
			}
		})
	}
}

// The percent spelling counts *decoded* bytes, and the re-emitted length is a
// delta in *source* bytes. Those are the same number only while the rewrite
// stays clear of the escapes — and it does not.
//
// `options.php` posts every option in one `application/x-www-form-urlencoded`
// body, so a URL in a serialized value arrives as `http%3A%2F%2Flocalhost%3A8080%2Fx`:
// three source bytes for each of the delimiters, one decoded byte each. When
// the variant and the canonical differ in scheme or port the splice covers
// those delimiters — it has to, or the variant's scheme and port are dropped —
// and it hands back `https%3A%2F%2Fwww.example.fi%2Fx`, one source byte shorter
// and one decoded byte longer. `n + len(repaired) - len(data)` is then two
// short of the truth, `unserialize()` returns false, and it is a request: the
// row goes into the shared database PLAN §4.3 says stays byte-identical to
// production.
//
// Both maps below are ordinary. `hostshift diff` never sees this, because it
// scores responses.
func TestAFormBodyLengthCountsDecodedBytesNotSourceBytes(t *testing.T) {
	for _, mp := range []struct{ name, canonical, variant string }{
		{"a variant with a port", "https://www.example.fi", "http://localhost:8080"},
		{"a canonical with a port", "https://www.example.fi:8443", "https://v.ddev.site"},
		{"portless, one scheme", "https://www.example.fi", "https://v.ddev.site"},
	} {
		t.Run(mp.name, func(t *testing.T) {
			// The request direction: the browser posts the variant host back.
			rev := pairMatcher(t, mp.variant, mp.canonical)
			data := mp.variant + "/wp-admin/options.php?x=1"
			payload := `a:1:{s:1:"u";s:` + strconv.Itoa(len(data)) + `:"` + data + `";}`
			body := "opt=" + url.QueryEscape(payload)

			out := string(RepairSerializedFields([]byte(body), func(b []byte) []byte {
				nv, _ := rev.Rewrite(b, SurfaceRequestBody, false)
				return HostLeaksBackCounted(rev, nv, NewStats(false), SurfaceRequestBody, 0)
			}))
			got, err := url.QueryUnescape(strings.TrimPrefix(out, "opt="))
			if err != nil {
				t.Fatalf("the body no longer decodes: %v", err)
			}
			if strings.Contains(got, mp.variant) {
				t.Fatalf("the variant host was not rewritten, so this asserts little:\n%s", got)
			}
			assertEveryLength(t, got)
		})
	}
}

// A declared length that lands in the middle of a character is not believed.
//
// The readings walk carries a shared counter and retires a reading when the
// counter reaches it exactly. A multi-byte unit can step the counter *past* a
// reading, which means that length stops inside a character — something a count
// of whole characters cannot do. Recording it anyway accepts the offset after
// the character, and where the closing quote sits exactly there the value
// parses perfectly with a length one short of the truth.
//
// `\u00e4` is the shape that can do it: six source bytes worth two decoded
// ones, so the counter can go from one short to one over in a single step. A
// raw `ä` cannot — it is two ordinary bytes and the counter passes through
// every value.
func TestALengthLandingMidCharacterIsDeclined(t *testing.T) {
	canon, variant := "https://mz30a.ddev.site", "https://wt-a--mz30a.ddev.site"
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}
	// The combined spelling: the blob's own quotes are `\&quot;`. Declare one
	// byte less than the truth, with the escape last, so the step lands on the
	// closing quote.
	q := `\&quot;`
	short := strconv.Itoa(len(canon) + 1) // the truth is len(canon)+2
	blob := `a:1:{s:1:` + q + `u` + q + `;s:` + short + `:` + q + canon + `\u00e4` + q + `;}`
	in := `<div data-x="&quot;` + blob + `&quot;">y</div>`

	got := string(RepairSerialized([]byte(in), rw))
	if strings.Contains(got, canon) {
		t.Errorf("the origin was not rewritten:\n%s", got)
	}
	if !strings.Contains(got, `s:`+short+`:`) {
		t.Errorf("a length landing mid-character was believed:\n%s", got)
	}
	// And it is a page the detector names, rather than one it certifies.
	if n := BrokenSerialized([]byte(got)); n == 0 {
		t.Errorf("the served bytes were reported clean:\n%s", got)
	}
}

// A value holding both an escaped character and the host is re-emitted, so the
// length it gets is measured rather than assumed.
//
// The matrix tests keep the text and the host in separate fields, and a field
// the rewrite does not touch returns its original bytes without re-emitting
// anything — so nothing there exercises the measurement of a `\uXXXX` at all.
// Putting them in one string is what makes the number get computed.
func TestAnEscapedCharacterInTheSameStringAsTheHost(t *testing.T) {
	canon, variant := "https://mz30b.ddev.site", "https://wt-a--mz30b.ddev.site"
	esc := func(v string) string { return strings.ReplaceAll(v, "/", `\/`) }
	rw := func(b []byte) []byte {
		s := strings.ReplaceAll(string(b), canon, variant)
		return []byte(strings.ReplaceAll(s, esc(canon), esc(variant)))
	}
	for name, text := range map[string]string{
		"two-byte":   "Läs mer",
		"three-byte": "日本語",
		"surrogate":  "\U0001F389",
	} {
		one := func(h string) string {
			v := h + "/x?t=" + text
			return `a:1:{s:1:"u";s:` + strconv.Itoa(len(v)) + `:"` + v + `";}`
		}
		in := `{"s":` + phpJSONEncode(one(canon)) + `}`
		want := `{"s":` + phpJSONEncode(one(variant)) + `}`
		if got := string(RepairSerialized([]byte(in), rw)); got != want {
			t.Errorf("%s:\n got  %s\n want %s", name, got, want)
		}
	}
}

// A nested payload behind a label, in every spelling and after every prefix a
// real field carries.
//
// A decline is not neutral — it rewrites the host and re-emits nothing — so a
// nested payload the walk would not look at was served with lengths PHP
// refuses. Twenty-two of these thirty-six combinations failed, for the sake of
// an ACF field label or an option edited in a textarea.
//
// Two separate causes, and the matrix is what separated them. `occupiesItsField`
// asks what *precedes* a value, which inside a string means nothing: scanning at
// any offset is the point there, so a prefix is the ordinary case. And
// `valueStart` matched raw whitespace only, so in a spelling that escapes a
// newline the byte before the payload is an ordinary `n` and the header was
// invisible — the scan then found the fields inside the payload instead and
// declined, correctly, on a structure that was perfectly sound.
func TestANestedPayloadBehindALabel(t *testing.T) {
	canon, variant := "https://mz30c.ddev.site", "https://wt-a--mz30c.ddev.site"
	ser := func(k, v string) string {
		return `a:1:{s:` + strconv.Itoa(len(k)) + `:"` + k + `";` +
			`s:` + strconv.Itoa(len(v)) + `:"` + v + `";}`
	}
	// The blob a real page carries: an outer array whose first field holds a
	// prefix and then another serialized payload.
	blob := func(host, prefix string) string {
		inner := prefix + ser("link", host+"/inner/x")
		return `a:2:{s:1:"f";s:` + strconv.Itoa(len(inner)) + `:"` + inner + `";` +
			`s:4:"home";s:` + strconv.Itoa(len(host)) + `:"` + host + `";}`
	}
	escAttr := strings.NewReplacer(
		`"`, "&quot;", `'`, "&#039;", "&", "&amp;", "<", "&lt;", ">", "&gt;")
	jsonBody := func(s string) string {
		j, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(j[1 : len(j)-1])
	}
	spellings := map[string]func(string) string{
		"raw":      func(s string) string { return s },
		"attr":     escAttr.Replace,
		"json":     jsonBody,
		"jsonattr": func(s string) string { return escAttr.Replace(jsonBody(s)) },
	}
	prefixes := map[string]string{
		"none": "", "space": " ", "newline": "\n", "tab": "\t",
		"nordic": "Läs ", "amp": "A & B ", "apos": "Genero's ",
		"quote": `say "hi" `, "prose": "Obs: ",
		// Labels that end in a letter or a digit, with no separator between
		// them and the payload. Every prefix above ends in one — a space, a
		// colon, a quote — and the nested scan's gate tested exactly that byte,
		// so a label written without trailing punctuation was invisible and the
		// whole column passed for the wrong reason.
		"bare letter": "x", "word": "Åtgärd", "digit": "v2",
	}
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}
	for sname, enc := range spellings {
		for pname, prefix := range prefixes {
			in := "<div>" + enc(blob(canon, prefix)) + "</div>"
			want := "<div>" + enc(blob(variant, prefix)) + "</div>"
			got := string(RepairSerialized([]byte(in), rw))
			if got != want {
				t.Errorf("%s / %s:\n got  %s\n want %s", sname, pname, got, want)
			}
			if n := BrokenSerialized([]byte(got)); n != 0 {
				t.Errorf("%s / %s: served %d value(s) PHP refuses:\n %s",
					sname, pname, n, got)
			}
		}
	}
}

// A nested payload behind a label is found in the percent spelling too.
//
// `valueStart` gates the scan on what precedes a value and accepts the
// percent-encoded separators alongside the literal ones — `pctIs(b, i-1, '{')`,
// `pctIs(b, i-1, ';')`, `pctIs(b, i-1, '"')`, and since round 30 the
// percent-encoded whitespace as well. None of those seven cases can ever fire.
// A percent escape is three bytes wide and its `%` sits at `i-3`; what stands at
// `i-1` is the second hex digit. `pctIs` checks for a `%` at the offset it is
// handed, so every one of these asks whether `B` is a `%` and is told no.
//
// So in the one spelling `options.php` posts every option in, a payload inside a
// string is invisible unless it starts at offset zero — which is the case
// `repairNested` was written to stop relying on, and the case a textarea, an ACF
// default or a line of prose in front of a blob all break.
//
// What it costs is this file's own failure mode in its quietest form. The outer
// string still parses, so its length is faithfully re-emitted over the new
// bytes. The inner one is never looked for, so the host inside it is rewritten
// and its `s:NN:` is left describing the byte count it had before —
// `unserialize()` returns false on the inner value. And `BrokenSerialized` gates
// on the same `valueStart`, so it reports zero on the served bytes and zero on
// the canonical ones, which the corpus diff subtracts to zero: GREEN, on a body
// PHP refuses. That is the shape rounds 28 to 30 each turned out to be.
//
// The literal spelling is here as the control: same payload, same prefixes, and
// it repairs.
func TestANestedPayloadBehindALabelInAFormBody(t *testing.T) {
	canon, variant := "https://mz31a.ddev.site", "https://wt-a--mz31a.ddev.site"
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	// An outer array whose first field holds a prefix and then another payload —
	// the blob a real options row carries.
	blob := func(host, prefix string) string {
		inner := prefix + `a:1:{s:4:"link";` + str(host+"/inner/x") + `}`
		return `a:2:{s:1:"f";` + str(inner) + `s:4:"home";` + str(host) + `}`
	}
	// rawurlencode / encodeURIComponent: everything but the unreserved set, and
	// no `+` for a space, which is what a REST or admin-ajax client posts.
	rawEnc := func(s string) string {
		const hex = "0123456789ABCDEF"
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			if c := s[i]; c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
				c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
				b.WriteByte(c)
				continue
			}
			b.WriteByte('%')
			b.WriteByte(hex[s[i]>>4])
			b.WriteByte(hex[s[i]&0xf])
		}
		return b.String()
	}
	rawDec := func(t *testing.T, s string) string {
		out, err := url.PathUnescape(s)
		if err != nil {
			t.Fatalf("the body no longer decodes: %v", err)
		}
		return out
	}
	rw := func(b []byte) []byte {
		s := strings.ReplaceAll(string(b), canon, variant)
		return []byte(strings.ReplaceAll(s, rawEnc(canon), rawEnc(variant)))
	}
	spellings := map[string]func(string) string{
		"literal": func(s string) string { return s },
		"percent": rawEnc,
	}
	// "none" is the control: at offset zero `valueStart` returns true before it
	// ever looks at what precedes the value, so that one repairs in both.
	prefixes := map[string]string{
		"none": "", "space": " ", "newline": "\n", "tab": "\t", "label": "Obs: ",
	}
	for sname, enc := range spellings {
		for pname, prefix := range prefixes {
			t.Run(sname+"/"+pname, func(t *testing.T) {
				in := "opt=" + enc(blob(canon, prefix))
				want := "opt=" + enc(blob(variant, prefix))
				got := string(RepairSerialized([]byte(in), rw))
				if got != want {
					t.Errorf("the nested length was not re-emitted; "+
						"BrokenSerialized calls the served bytes clean (%d) and the "+
						"canonical ones clean (%d), so the corpus diff subtracts it "+
						"to zero:\n got  %s\n want %s",
						BrokenSerialized([]byte(got)), BrokenSerialized([]byte(in)),
						got, want)
				}
				// What PHP is handed, in the spelling PHP is handed it in.
				body := strings.TrimPrefix(got, "opt=")
				if sname == "percent" {
					body = rawDec(t, body)
				}
				assertEveryLength(t, body)
			})
		}
	}
}

// A `C:` payload carrying a character reference is repaired, not declined.
//
// WooCommerce stores custom-serialized blobs, and their payload is arbitrary
// bytes — a URL with a query string is ordinary content there. The payload was
// measured with syn.advance, which knows one reading, so an `&amp;` under
// esc_attr declined the whole field. A decline still rewrites the host and
// re-emits nothing, which leaves a length describing the bytes from before.
func TestACustomPayloadWithAnAmpersand(t *testing.T) {
	canon, variant := "https://mz31b.ddev.site", "https://wt-a--mz31b.ddev.site"
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}
	blob := func(host string) string {
		p := host + "/p?q=1&r=2"
		return `C:7:"WC_Data":` + strconv.Itoa(len(p)) + `:{` + p + `}`
	}
	escAttr := strings.NewReplacer(`"`, "&quot;", "&", "&amp;")
	for name, enc := range map[string]func(string) string{
		"literal": func(s string) string { return s },
		"attr":    escAttr.Replace,
	} {
		in := `<div>` + enc(blob(canon)) + `</div>`
		want := `<div>` + enc(blob(variant)) + `</div>`
		if got := string(RepairSerialized([]byte(in), rw)); got != want {
			t.Errorf("%s:\n got  %s\n want %s", name, got, want)
		}
		if n := BrokenSerialized([]byte(want)); n != 0 {
			t.Errorf("%s: the repaired page counted %d broken values", name, n)
		}
	}
}

// A nested payload behind a label in a form body, in the spelling a browser
// actually posts.
//
// `application/x-www-form-urlencoded` writes a space as `+`, not `%20`, so a
// field label ending in a space — which is what a label ends with — puts a `+`
// immediately before the payload. Every prefix here reaches the scan as a
// different byte: `+` for the space, `%3A` for the colon, `%0A` for the
// newline, a raw hex digit for the `ä`.
//
// The percent openers were all off by two and had never fired, so this whole
// column was invisible: the payload was rewritten, its length left behind, and
// the row went into the database on an options.php save. The detector gates on
// the same function, so both sides of the diff read zero and it printed GREEN.
func TestANestedPayloadBehindALabelInAPostedForm(t *testing.T) {
	canon, variant := "https://mz31c.ddev.site", "https://wt-a--mz31c.ddev.site"
	// The host, not the whole origin: QueryEscape encodes `://` but leaves the
	// hostname literal, and it is the hostname the engine's decoder view
	// matches. Substituting the full origin would never fire here and the test
	// would pass on bytes nobody rewrote.
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b),
			"mz31c.ddev.site", "wt-a--mz31c.ddev.site"))
	}
	ser := func(k, v string) string {
		return `a:1:{s:` + strconv.Itoa(len(k)) + `:"` + k + `";` +
			`s:` + strconv.Itoa(len(v)) + `:"` + v + `";}`
	}
	blob := func(host, prefix string) string {
		inner := prefix + ser("link", host+"/inner/x")
		return `a:2:{s:1:"f";s:` + strconv.Itoa(len(inner)) + `:"` + inner + `";` +
			`s:4:"home";s:` + strconv.Itoa(len(host)) + `:"` + host + `";}`
	}
	for name, prefix := range map[string]string{
		"none": "", "space": " ", "newline": "\n", "tab": "\t",
		"label": "Obs: ", "nordic": "Läs ", "amp": "A & B ",
	} {
		// url.QueryEscape is what a browser does: space becomes `+`.
		in := "opt=" + url.QueryEscape(blob(canon, prefix))
		want := "opt=" + url.QueryEscape(blob(variant, prefix))
		got := string(RepairSerializedFields([]byte(in), rw))
		if got != want {
			t.Errorf("%s:\n got  %s\n want %s", name, got, want)
		}
		if n := BrokenSerialized([]byte(got)); n != 0 {
			t.Errorf("%s: posted %d value(s) PHP refuses:\n %s", name, n, got)
		}
	}
}

// An `esc_attr` value comes home unchanged.
//
// "The length one direction broke, the other restores" is the sentence the whole
// decline path rests on, and in this spelling it is not true. Repair is not an
// involution here: the forward pass repairs and the reverse pass declines, so
// the length the forward pass wrote stays on data the reverse pass shortened,
// and `unserialize()` returns false on a row the browser posted back.
//
// The reason is that `spanEnd`'s "exactly one reading closes" rule is a function
// of the *declared length*, and the declared length is exactly what the two
// directions disagree about. `&quot;` is six source bytes worth either one
// decoded byte or six, so a span holding k of them has up to 2^k readings; the
// value below has one reading that closes at n=66 and two at n=72. One is a
// repair, two is a decline, and the host sits inside the string, so the outer
// value is measured at 66 going out and 72 coming back.
//
// It needs three things at once, which is why a generator that varies only the
// data misses it: the host inside the outer string, so its declared length
// moves between the directions; enough `&quot;` in that string for a second
// assignment to exist; and the arithmetic to land. Measured over the fixture
// family below, a one- or two-member array never fails and a six-member one
// fails 38% of the time.
func TestAnEscAttrValueComesHomeUnchanged(t *testing.T) {
	canon, variant := "//canon.test", "//wt-a--canon.test"
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	// esc_attr: the quotes become `&quot;`, and nothing else here needs escaping.
	esc := strings.NewReplacer(`"`, "&quot;").Replace
	// An outer string holding an array of `members` pairs, the last of which
	// carries the host. Every length computed.
	blob := func(host string, members, pad int) string {
		inner := `a:` + strconv.Itoa(members) + `:{`
		for m := 0; m < members-1; m++ {
			inner += str("a"+strconv.Itoa(m)) + str("v")
		}
		inner += str("a") + str(host+"/"+strings.Repeat("x", pad)) + `}`
		return str(inner)
	}
	fwd := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}
	rev := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), variant, canon))
	}
	// Pads chosen so the *forward* pass repairs: the canonical length has one
	// closing reading and the variant length has two. (Other pads make the
	// forward pass decline instead, on the same rule — see the note above.)
	for _, c := range []struct {
		name         string
		members, pad int
	}{
		{"three members", 3, 0},
		{"four members", 4, 2},
		{"five members", 5, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := esc(blob(canon, c.members, c.pad))
			out := RepairSerialized([]byte(in), fwd)
			if string(out) == in {
				t.Fatalf("the forward pass did not rewrite, so this asserts little:\n%s", in)
			}
			// What the forward pass served has to parse, and what comes back has
			// to be what went out.
			assertEveryLength(t, strings.ReplaceAll(string(out), "&quot;", `"`))
			back := string(RepairSerialized(out, rev))
			if back != in {
				t.Errorf("the reverse pass declined where the forward pass "+
					"repaired, so the length the forward pass wrote is now stale "+
					"on shortened data:\n in   %s\n out  %s\n back %s", in, out, back)
			}
			assertEveryLength(t, strings.ReplaceAll(back, "&quot;", `"`))
		})
	}
}

// A serialized payload nested inside an esc_attr'd string, swept over array
// size and path length, in both directions and through the detector.
//
// `&quot;` is this spelling's own delimiter, so a nested payload is *made of*
// them. Offering each one two readings — escaped, worth one byte, or the six
// literal bytes it would be if the data already held `&quot;` before esc_attr
// saw it — gives a span holding k of them up to 2^k readings, and almost all of
// them are spurious. `spanEnd` then finds two that close and declines.
//
// The cost was not theoretical. At six members it declined 34 of 60 paddings,
// in *both* directions: the browser was served a blob PHP refuses, from a
// canonical page that was fine. And the detector runs the same walk, so it
// called the repaired page broken — a false RED on healthy bytes, which is the
// one failure mode this file's history says is worse than a false GREEN,
// because a check that is always red is a check nobody reads.
//
// The sweep is the test rather than a few fixtures because whether a second
// reading exists is arithmetic: it depends on the declared length, the number
// of quotes and the padding all at once, so any single shape passes for
// uninteresting reasons.
func TestANestedPayloadUnderEscAttrSurvivesBothDirections(t *testing.T) {
	canon, variant := "//mz31d.test", "//wt-a--mz31d.test"
	fwd := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}
	rev := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), variant, canon))
	}
	esc := func(v string) string { return strings.ReplaceAll(v, `"`, "&quot;") }
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	// An n-member array whose last value carries the host, all lengths computed.
	blob := func(host string, members, pad int) string {
		s := `a:` + strconv.Itoa(members) + `:{`
		for k := 0; k < members-1; k++ {
			s += str("a"+strconv.Itoa(k)) + str("v")
		}
		return s + str("a") + str(host+"/"+strings.Repeat("x", pad)) + `}`
	}
	for members := 2; members <= 7; members++ {
		for pad := 0; pad <= 60; pad++ {
			in := esc(str(blob(canon, members, pad)))
			want := esc(str(blob(variant, members, pad)))

			got := string(RepairSerialized([]byte(in), fwd))
			if got != want {
				t.Fatalf("members=%d pad=%d: the response direction did not repair:\n got  %s\n want %s",
					members, pad, got, want)
			}
			if back := string(RepairSerialized([]byte(got), rev)); back != in {
				t.Fatalf("members=%d pad=%d: the request direction did not restore:\n got  %s\n want %s",
					members, pad, back, in)
			}
			if n := BrokenSerialized([]byte(want)); n != 0 {
				t.Fatalf("members=%d pad=%d: the detector called %d value(s) broken on a page PHP accepts:\n %s",
					members, pad, n, want)
			}
		}
	}
}

// escAttrNoDouble is `esc_attr` as WordPress calls it: htmlspecialchars with
// $double_encode = false, which leaves a reference already in the data alone
// and escapes only a bare `&`.
//
// A Replacer that rewrites every `&` is not this. It turns `&amp;` into
// `&amp;amp;`, so a fixture built with one never contains the literal reading
// at all and tests nothing about it.
func escAttrNoDouble(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch {
		case s[i] == '&' && refRun([]byte(s), i) > 0:
			w := refRun([]byte(s), i)
			b.WriteString(s[i : i+w])
			i += w
			continue
		case s[i] == '&':
			b.WriteString("&amp;")
		case s[i] == '"':
			b.WriteString("&quot;")
		case s[i] == '\'':
			b.WriteString("&#039;")
		case s[i] == '<':
			b.WriteString("&lt;")
		case s[i] == '>':
			b.WriteString("&gt;")
		default:
			b.WriteByte(s[i])
		}
		i++
	}
	return b.String()
}

// Content that already held `&amp;` before either escaper saw it, in the
// combined spelling.
//
// `esc_attr` runs with `$double_encode = false`, so those five bytes pass
// through unchanged and the serialized length counts five. The escaped reading
// — one byte, for content that held a bare `&` — is the other one, and both
// have to be on offer or a value takes whichever the walk assumes.
//
// Unlike the `&quot;` case next door, this one stays ambiguous in the combined
// spelling: there a data quote is written `\&quot;`, so a bare `&quot;` is
// literal content too, and the nested-payload explosion that forces `&quot;` to
// one byte under plain esc_attr cannot arise when the delimiters are seven
// bytes wide.
func TestLiteralEntitiesInContentUnderTheCombinedSpelling(t *testing.T) {
	canon, variant := "https://mz31e.ddev.site", "https://wt-a--mz31e.ddev.site"
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}
	for name, text := range map[string]string{
		// The five bytes are already in the data; esc_attr leaves them.
		"literal amp":   "Snellman &amp; Co",
		"literal lt":    "a &lt; b",
		"literal quote": "say &quot;hi&quot;",
		// And the escaped readings, for contrast.
		"bare amp": "Snellman & Co",
		"bare lt":  "a < b",
	} {
		one := func(h string) string {
			return `a:2:{s:1:"u";s:` + strconv.Itoa(len(h)) + `:"` + h + `";` +
				`s:1:"t";s:` + strconv.Itoa(len(text)) + `:"` + text + `";}`
		}
		// wp_json_encode, then esc_attr with $double_encode = false.
		comb := func(h string) string {
			j, err := json.Marshal(one(h))
			if err != nil {
				t.Fatal(err)
			}
			// Undo Go's HTML escaping, which PHP's json_encode does not do, so
			// the entities reach esc_attr as themselves.
			s := strings.NewReplacer(`\u0026`, "&", `\u003c`, "<", `\u003e`, ">").
				Replace(string(j))
			return escAttrNoDouble(s)
		}
		in := `<div data-settings="` + comb(canon) + `">y</div>`
		want := `<div data-settings="` + comb(variant) + `">y</div>`
		if got := string(RepairSerialized([]byte(in), rw)); got != want {
			t.Errorf("%s:\n got  %s\n want %s", name, got, want)
		}
		if n := BrokenSerialized([]byte(want)); n != 0 {
			t.Errorf("%s: the repaired page counted %d broken values", name, n)
		}
	}
}

// A serialized payload nested inside a serialized string, under esc_attr, with
// ampersands in the content.
//
// This is ordinary WordPress — a transient, a widget instance, a cached ACF
// payload, an option echoed into an `esc_attr` input or `esc_textarea`. It was
// served unparseable about thirty per cent of the time, and the reason is that
// `&quot;;` stands at every internal string boundary of a nested payload: charge
// one `&amp;` its literal five bytes instead of one and the count lands on a
// boundary that is not the real one. More than one reading closes, and refusing
// to choose meant refusing constantly.
//
// The choice is not made by preference. Both are tried and the enclosing parse
// decides — a reading that does not let the whole value parse and fill its
// field is not a reading. What still declines is two *complete* parses that
// disagree, which nothing in the bytes ranks.
func TestAPayloadNestedInsideASerializedString(t *testing.T) {
	canon, variant := "https://mz32d.ddev.site", "https://wt-b--mz32d.ddev.site"
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	// serialize(["cache"=>…, "payload"=>serialize([…]), "note"=>…]), every
	// length computed, with the words a Finnish agency's content actually has.
	blob := func(host, note string) string {
		inner := `a:2:{` + str("k1") + str(host+"/sv/") + str("Hej!") + str(note) + `}`
		payload := `a:1:{` + str("Åäö") + str(inner) + `}`
		return `a:3:{` + str("cache") + str("x") +
			str("payload") + str(payload) + str("note") + str(note) + `}`
	}
	carriers := map[string]func(string) string{
		"esc_attr attribute": func(s string) string {
			return `<div data-acf="` + escAttrNoDouble(s) + `">x</div>`
		},
		"esc_textarea": func(s string) string {
			return `<textarea>` + escAttrNoDouble(s) + `</textarea>`
		},
		"esc_attr(wp_json_encode())": func(s string) string {
			j, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			return `<div data-settings="` + escAttrNoDouble(string(j)) + `">x</div>`
		},
	}
	for cname, carry := range carriers {
		for _, note := range []string{"R&D", "A & B", "a<b", "Genero's", "plain", "Åäö"} {
			in, want := carry(blob(canon, note)), carry(blob(variant, note))
			if got := string(RepairSerialized([]byte(in), rw)); got != want {
				t.Errorf("%s / %q:\n got  %s\n want %s", cname, note, got, want)
			}
			if n := BrokenSerialized([]byte(want)); n != 0 {
				t.Errorf("%s / %q: the detector called %d value(s) broken on bytes PHP accepts",
					cname, note, n)
			}
		}
	}
}

// Every spelling of the quote delimiter, re-emitted as it arrived.
//
// Two faults, one test. `&#x22;` was in neither the matcher nor entityRun, so a
// value delimited with it was not a value at all — it declined, and a decline
// rewrites the host and re-emits nothing. And `&#34;` was matched but re-emitted
// as `&quot;`, one byte wider with the same decoded content: the length in this
// spelling is a *source* delta, so it counted the extra byte and overshot by one
// per quote.
//
// The fix for the second is general — every delimiter now comes back exactly as
// it arrived and only the digits are replaced. That also stops the percent
// spelling normalising `%3a` to `%3A`, which changed a client's bytes for no
// reason at all.
func TestEverySpellingOfTheQuoteDelimiter(t *testing.T) {
	canon, variant := "https://mz33a.ddev.site", "https://wt-a--mz33a.ddev.site"
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	// A nested payload, because that is where a per-quote error accumulates.
	blob := func(host string) string {
		inner := `a:1:{` + str("u") + str(host+"/x/") + `}`
		return `a:1:{` + str("a") + str(inner) + `}`
	}
	for _, q := range []string{"&quot;", "&#34;", "&#034;", "&#x22;", "&#X22;", "&#x022;"} {
		enc := func(s string) string { return strings.ReplaceAll(s, `"`, q) }
		in, want := `<div data-x="`+enc(blob(canon))+`">y</div>`,
			`<div data-x="`+enc(blob(variant))+`">y</div>`
		got := string(RepairSerialized([]byte(in), rw))
		if got != want {
			t.Errorf("%s:\n got  %s\n want %s", q, got, want)
		}
		if n := BrokenSerialized([]byte(got)); n != 0 {
			t.Errorf("%s: served %d value(s) PHP refuses:\n %s", q, n, got)
		}
	}
}

// The percent spelling keeps the client's hex case.
//
// syn.emit wrote `%3A` whatever arrived, so a client encoding in lowercase had
// its body altered by a proxy asked only to map an origin — and in the identity
// map that is test 24 outright. The early return for unchanged data hid it;
// this asserts the path where the value *is* rewritten.
func TestThePercentSpellingKeepsItsHexCase(t *testing.T) {
	canon, variant := "https://mz33b.ddev.site", "https://wt-a--mz33b.ddev.site"
	// The bare host: the percent spelling encodes `:` so a full origin never
	// appears literally, and a stub matching on one would fire on nothing and
	// let this pass on untouched bytes.
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b),
			"mz33b.ddev.site", "wt-a--mz33b.ddev.site"))
	}
	enc := func(host string) string {
		v := host + "/x"
		s := `s:` + strconv.Itoa(len(v)) + `:"` + v + `";`
		// Lowercase hex, and every delimiter encoded — a terminator left
		// literal is not a terminator in this spelling, and the value would
		// decline for that rather than for the reason under test.
		return strings.NewReplacer(
			":", "%3a", `"`, "%22", ";", "%3b", "/", "%2f").Replace(s)
	}
	got := string(RepairSerializedFields([]byte("opt="+enc(canon)), rw))
	if want := "opt=" + enc(variant); got != want {
		t.Errorf("\n got  %s\n want %s", got, want)
	}
}

// PHP writes a large float as `d:1.0E+17;` — anything from 1e17 up, and every
// integer past PHP_INT_MAX — and a form encoder writes that `+` as `%2B`.
//
// scanScalar reads a `d:` value's sign, point and exponent as raw bytes, so in
// the percent spelling the exponent's sign is invisible: the scalar fails to
// parse, the array holding it fails with it, and the field declines. `+` is the
// only byte of that grammar a urlencoder touches, which is why the other two
// scalars here pass.
//
// A decline is not neutral. The generic rewrite still replaces the host and
// re-emits no length, so `options.php` hands `update_option` the variant's byte
// count over the canonical's data and `unserialize` returns false — the row is
// lost. Nothing scores it: `hostshift diff` never looks at a request, and the
// decline is host-independent, so the detector counts it identically on both
// sides of the diff.
func TestAPercentEncodedFloatExponentDoesNotDeclineItsField(t *testing.T) {
	canon, variant := "mz34a.ddev.site", "wt-a--mz34a.ddev.site"
	// The bare host: the percent spelling encodes `:` and `/`, so a full origin
	// never appears literally and a stub matching on one would rewrite nothing.
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), variant, canon))
	}
	// Every delimiter encoded, as `urlencode` writes them — one left literal is
	// not a delimiter in this spelling and the value would decline for that
	// rather than for the reason under test.
	enc := strings.NewReplacer(
		":", "%3A", `"`, "%22", ";", "%3B", "{", "%7B", "}", "%7D",
		"/", "%2F", "+", "%2B").Replace
	ser := func(host, scalar string) string {
		v := "https://" + host + "/a.png"
		return `a:2:{s:1:"n";` + scalar +
			`s:1:"u";s:` + strconv.Itoa(len(v)) + `:"` + v + `";}`
	}
	for _, scalar := range []string{"i:12;", "d:1.0E-5;", "d:1.0E+17;"} {
		t.Run(scalar, func(t *testing.T) {
			got := string(RepairSerializedFields([]byte("opt="+enc(ser(variant, scalar))), rw))
			if want := "opt=" + enc(ser(canon, scalar)); got != want {
				t.Errorf("the length was not re-emitted, so PHP refuses the whole option:"+
					"\n got  %s\n want %s", got, want)
			}
		})
	}
}

// The whole product of transport and escaping: raw or percent-encoded, crossed
// with every spelling of the quote delimiter.
//
// The four spellings before this composed two layers at most, and percent over
// entity was the pair nothing covered — percentSyntax matches `%3A` for the
// colon and wants `%22` for the quote, htmlSyntax matches `&#34;` and wants a
// literal colon, so neither parsed it. The value was skipped, and a skip is not
// neutral: the host is rewritten and no length re-emitted. Three of eight cells
// were served as bytes PHP refuses, in both directions, with the detector
// reporting GREEN because it walks the same list of spellings.
//
// The matrix is the test because the bug is exactly a hole in a product: any
// single cell passes for reasons that say nothing about its neighbours.
func TestTheProductOfTransportAndEscaping(t *testing.T) {
	canon, variant := "https://mz34a.ddev.site", "https://wt-b--mz34a.ddev.site"
	// The bare host: percent-encoding hides the `://`, so a stub matching a
	// full origin would fire on nothing and pass on untouched bytes.
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), "mz34a.ddev.site", "wt-b--mz34a.ddev.site"))
	}
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	blob := func(host string) string {
		inner := `a:1:{` + str("url") + str(host+"/sv/") + `}`
		return `a:2:{` + str("home") + str(host) + str("items") + str(inner) + `}`
	}
	// rawurlencode: every reserved byte escaped, upper hex, letters left alone.
	pct := func(s string) string {
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
				c == '-' || c == '_' || c == '.' || c == '~' {
				b.WriteByte(c)
				continue
			}
			fmt.Fprintf(&b, "%%%02X", c)
		}
		return b.String()
	}
	for _, q := range []string{`"`, "&#34;", "&#x22;", "&quot;"} {
		for _, transport := range []string{"raw", "percent"} {
			enc := func(host string) string {
				s := strings.ReplaceAll(blob(host), `"`, q)
				if transport == "percent" {
					return pct(s)
				}
				return s
			}
			in, want := enc(canon), enc(variant)
			got := string(RepairSerialized([]byte(in), rw))
			if got != want {
				t.Errorf("%s / %s:\n got  %s\n want %s", transport, q, got, want)
			}
			if n := BrokenSerialized([]byte(got)); n != 0 {
				t.Errorf("%s / %s: served %d value(s) PHP refuses:\n %s", transport, q, n, got)
			}
		}
	}
}

// The detector reads every spelling the repair does.
//
// They walk the same list on purpose, so a spelling added to one and not the
// other is a page served with a length PHP refuses and reported GREEN — the
// cancellation is automatic, since a value neither side can read scores zero on
// the canonical page too. This asserts the list from the detector's end, which
// no test did: the repair tests all measure served bytes, and a spelling
// missing only from `BrokenSerialized` leaves those passing.
func TestTheDetectorReadsEverySpellingTheRepairDoes(t *testing.T) {
	host := "https://mz35a.ddev.site/x"
	// A bare string with a length that overruns its data — not wrapped in an
	// array, because an array's own header commits under the plain percent
	// spelling whatever the string inside it is written in, and the count would
	// then be the same with the spelling removed.
	// Padded, because readLen refuses a length larger than the buffer — without
	// the padding the header never commits and nothing counts it, for a reason
	// that has nothing to do with the spelling under test.
	stale := func(v string) string {
		return `s:` + strconv.Itoa(len(v)+9) + `:"` + v + `";` + strings.Repeat("z", 32)
	}
	pct := func(s string) string {
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
				c == '-' || c == '_' || c == '.' || c == '~' {
				b.WriteByte(c)
				continue
			}
			fmt.Fprintf(&b, "%%%02X", c)
		}
		return b.String()
	}
	jsonEsc := func(s string) string {
		return strings.NewReplacer(`"`, `\"`, "/", `\/`).Replace(s)
	}
	for name, enc := range map[string]func(string) string{
		"literal":                      func(s string) string { return s },
		"esc_attr":                     escAttrNoDouble,
		"wp_json_encode":               jsonEsc,
		"esc_attr(wp_json_encode)":     func(s string) string { return escAttrNoDouble(jsonEsc(s)) },
		"rawurlencode":                 pct,
		"rawurlencode(esc_attr)":       func(s string) string { return pct(escAttrNoDouble(s)) },
		"rawurlencode(wp_json_encode)": func(s string) string { return pct(jsonEsc(s)) },
		// JSON_HEX_QUOT, which is what wp_interactivity_data_wp_context()
		// writes so a single-quoted attribute needs no second escaping pass.
		"wp_json_encode(JSON_HEX_QUOT)": func(s string) string {
			return strings.ReplaceAll(jsonEsc(s), `\"`, `\u0022`)
		},
		// The mirror of esc_attr(wp_json_encode()): whichever encoder ran last
		// owns the quotes.
		"wp_json_encode(esc_attr)": func(s string) string { return jsonEsc(escAttrNoDouble(s)) },
		// JSON carried as a string inside JSON, which is a nested block
		// attribute — the same encoder twice, which the product missed because
		// it was enumerated over *kinds*.
		"wp_json_encode twice": func(s string) string {
			return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(jsonEsc(s))
		},
	} {
		if n := BrokenSerialized([]byte(enc(stale(host)))); n == 0 {
			t.Errorf("%s: a length overrunning its data was reported clean:\n %s",
				name, enc(stale(host)))
		}
	}
}

// A character reference other than the quote, in the percent-over-entity
// spelling.
//
// Round 34 taught that spelling to read a percent-encoded `&quot;` and left
// every other reference falling through to a reader that wants a literal `&`.
// So `&amp;` — which reaches this spelling as `%26amp%3B` — was read as five
// separate bytes, the value mis-measured, and the length was not re-emitted
// while the host was rewritten anyway. `&`, `<`, `>` and `'` are near-universal
// in real content: a client name, an `esc_textarea`'d fragment, any label with
// an apostrophe in it.
func TestANonQuoteReferenceInThePercentEntitySpelling(t *testing.T) {
	canon, variant := "https://mz35b.example", "https://wt-a--mz35b.ddev.site"
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), "mz35b.example", "wt-a--mz35b.ddev.site"))
	}
	pct := func(s string) string {
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
				c == '-' || c == '_' || c == '.' || c == '~' {
				b.WriteByte(c)
				continue
			}
			fmt.Fprintf(&b, "%%%02X", c)
		}
		return b.String()
	}
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	for _, text := range []string{"x", "&", "<", ">", "'", "Snellman & Söner", "a<b>c"} {
		blob := func(host string) string {
			return `a:2:{` + str("u") + str(host) + str("a") + str(text) + `}`
		}
		in, want := pct(escAttrNoDouble(blob(canon))), pct(escAttrNoDouble(blob(variant)))
		got := string(RepairSerialized([]byte(in), rw))
		if got != want {
			t.Errorf("%q:\n got  %s\n want %s", text, got, want)
		}
		if n := BrokenSerialized([]byte(got)); n != 0 {
			t.Errorf("%q: served %d value(s) PHP refuses:\n %s", text, n, got)
		}
	}
}

// A spelling the walk cannot read is counted when the rewrite touches it, and
// counted on the variant side only.
//
// That asymmetry is the whole point. `BrokenSerialized` asks whether the served
// bytes parse, and a spelling this build cannot read does not parse on the
// canonical page either — so the corpus diff's baseline subtraction cancels it
// to zero, which is how four consecutive rounds of real corruption were
// reported GREEN. This asks whether the rewrite changed bytes it could not
// account for, which is a no-op on the canonical side by construction.
//
// It exists because enumerating the spellings does not terminate: five real
// encoders give twenty-five ordered pairs, and each one added multiplies them.
// There will always be a composition outside the walk; what must not happen
// again is that it is edited silently.
func TestASpellingTheWalkCannotReadIsReportedWhenRewritten(t *testing.T) {
	canon, variant := "www.mz37a.test", "v.ddev.site"
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}
	id := func(b []byte) []byte { return b }
	url := "https://" + canon + "/a.png"
	blob := `a:1:{i:0;s:` + strconv.Itoa(len(url)) + `:"` + url + `";}`

	for name, wire := range map[string]string{
		// JSON_HEX_QUOT composed with percent-encoding: two encoders the walk
		// reads separately and not together.
		"percent over hex-quoted JSON": strings.ReplaceAll(blob, `"`, `%5Cu0022`),
		"hex-quoted JSON twice":        strings.ReplaceAll(blob, `"`, `\\u0022`),
	} {
		if n := UnreadRewrites([]byte(wire), rw); n == 0 {
			t.Errorf("%s: rewritten without being read, and not reported:\n %s", name, wire)
		}
		if n := UnreadRewrites([]byte(wire), id); n != 0 {
			t.Errorf("%s: counted %d on the canonical side, so it cancels", name, n)
		}
	}

	// And it stays quiet on everything it should: a spelling the walk does read,
	// and content with nothing serialized in it at all.
	for name, wire := range map[string]string{
		"a spelling the walk reads": blob,
		"esc_attr":                  escAttrNoDouble(blob),
		"no serialized content":     `<a href="https://` + canon + `/a">x</a>`,
		"prose naming the host":     `see https://` + canon + ` for details`,
	} {
		if n := UnreadRewrites([]byte(wire), rw); n != 0 {
			t.Errorf("%s: reported %d on content that is fine:\n %s", name, n, wire)
		}
	}
}
