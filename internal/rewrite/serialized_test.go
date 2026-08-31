package rewrite

import (
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

// Prose that merely resembles a span must not be treated as one, or everything
// after it is copied out without being rewritten.
func TestProseIsNotASpan(t *testing.T) {
	in := `The value is s:6:"a.test"; and see https://a.test/x`
	out := string(RepairSerialized([]byte(in), lengthen))
	if strings.Contains(out, "//a.test/") {
		t.Errorf("an origin after a span-shaped quotation was not rewritten:\n%s", out)
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
