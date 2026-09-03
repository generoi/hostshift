package rewrite

import (
	"io"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 55. The surface *names*, not the surface question.
//
// Round 54 answered the right question — a backslash is a path separator in a
// buffer some decoder has already unquoted, and JavaScript's escape alphabet in
// one that still carries its source escapes — and wrote the answer down in one
// table, origin.escapeAlphabetFor. What it did not do is enumerate the names.
// escPath is a positive list of seven literals and this package hands
// Matcher.rewrite and locateHostIn fourteen surface names, so seven fall to the
// default, which is escJS. TestSurfaceNamesAreKnownHere, whose whole job is to
// pin the names across the package boundary, lists twelve of the fourteen — and
// the two it does not name are two of the three that leak.
//
// Three of the seven name buffers that are already unquoted:
//
//   - html-obfuscated is an attribute value that reached normaliseURLLeak,
//   - html-entity is an attribute value that reached decodeEntityLeak or
//     refsLeak — in the second case with its character references already
//     decoded,
//   - raw-text is the markup inside <noscript>, <iframe>, <textarea> and
//     <title>, where the HTML tokenizer decodes references and no string
//     decoder runs at all.
//
// So the same `<a href>` is read as a path in one pass and as JavaScript in the
// next, and the pass that reads it as JavaScript is the fallback — the one that
// only runs when the first declined. That is round 53's finding 4 exactly, which
// round 54's commit message opens with, alive again one surface name over.
//
// The enumeration behind this file is 7,714 expressible cells out of 8,520:
// 5 separator spellings x 2 host spellings x 142 boundary tokens x 6 surfaces
// (href, prose, inline script, a JSON body, a CSS url(), a bare header value),
// each asserting what a browser resolves rather than what the code emits, and
// each run through the forward
// direction, back through the surface's own decoder, and through the request
// direction. Against 0f74c2d it reports 90 leaks; against fc2dfdd, its parent,
// 48. 82 of the 90 are new in 0f74c2d and 40 of the parent's 48 are gone, so the
// round is a net loss of 42 on this corpus.
//
// Every expectation below is ada's, the parser Chrome ships, asked as
//
//	new URL(decode_surface(value), "https://wt-a--example.ddev.site/dir/page").host
//
// where decode_surface is the surface's own parser: character-reference decoding
// for an attribute value or a text node, a JavaScript string literal for an
// inline <script>. The decoded form and the host are quoted in each case.

const (
	r55Canonical = "www.example.fi"
	r55Variant   = "wt-a--example.ddev.site"
)

func r55Matcher(t *testing.T) *origin.Matcher {
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://" + r55Canonical),
		Variant:   origin.MustParse("https://" + r55Variant),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// r55Reverse is the request direction of the same map.
func r55Reverse(t *testing.T) *origin.Matcher {
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://" + r55Variant),
		Variant:   origin.MustParse("https://" + r55Canonical),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// r55Back is what the proxy does to a request body: the byte matcher, then the
// URL-parser locator, both in the reverse direction (proxy.go's rewriteBody).
func r55Back(m *origin.Matcher, s string) string {
	nv, _ := m.Rewrite([]byte(s), SurfaceRequestBody, false)
	return string(HostLeaksBackCounted(m, nv, NewStats(false), SurfaceRequestBody, 0))
}

// ---------------------------------------------------------------------------
// 1. An attribute value, read by the fallback passes as if it were a script.
// ---------------------------------------------------------------------------

// A backslash in an `href` is a path separator — WHATWG folds it to '/' — so
// every value below is the production origin with a path, and every one of them
// is a live production origin in the developer's authenticated browser (test
// 28). fc2dfdd rewrites all six. 0f74c2d rewrites none of them, because the
// only pass that can see them is normaliseURLLeak or refsLeak, and those two
// name their surface "html-obfuscated" and "html-entity", which
// escapeAlphabetFor does not list.
//
// The separator has to be one the raw byte scan cannot anchor on, because that
// scan runs first under "html-attr" and gets the answer right. `https:\\host`
// and `https:///host` are two of the spellings TestObfuscatedOriginsAreRewritten
// already pins as reaching production, and `https:&#47;&#47;host` is the
// character-reference spelling §5.3 calls out; a Windows-authored link or a
// double-escaped JSON string produces the backslash path in the same value.
func TestR55AnAttributeIsNotAScriptWhateverTheSurfaceIsCalled(t *testing.T) {
	m := r55Matcher(t)
	for _, c := range []struct{ name, in string }{
		// decoded "https:\\www.example.fi\wp-admin/" -> www.example.fi
		{"backslash separator, backslash path",
			`<a href="https:\\www.example.fi\wp-admin/">`},
		// decoded "https:\\www.example.fi\A" -> www.example.fi
		{"backslash separator, an escape JavaScript would fold",
			`<a href="https:\\www.example.fi\A">`},
		// decoded "https:///www.example.fi\wp-admin/" -> www.example.fi
		{"three slashes, backslash path",
			`<a href="https:///www.example.fi\wp-admin/">`},
		// decoded "https://www.example.fi\wp-admin/" -> www.example.fi
		{"reference-encoded separator, backslash path",
			`<a href="https:&#47;&#47;www.example.fi\wp-admin/">`},
		// decoded "https://www.example.fi\wp-admin/" -> www.example.fi
		{"reference-encoded separator and backslash",
			`<a href="https:&#47;&#47;www.example.fi&bsol;wp-admin/">`},
		// decoded "https:\\www.example.fi\-/p" -> www.example.fi. This is
		// round 53's finding 4, the value 0f74c2d's own commit message opens
		// with, one obfuscated separator away from the surface it was fixed on.
		{"the value round 54's commit message opens with",
			`<a href="https:\\www.example.fi\-/p">`},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := rewriteHTML(t, m, c.in, NewStats(false))
			if !strings.Contains(out, r55Variant) {
				t.Errorf("a browser resolves this to %s and it was served live:\n in  %s\n out %s",
					r55Canonical, c.in, out)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2. A raw-text element runs no string decoder either.
// ---------------------------------------------------------------------------

// html.go's own comment says why every raw-text element is scanned: "the
// tokenizer hands back the *markup* inside <noscript>, <textarea>, <iframe> and
// <svg><title> as opaque text, so a URL in an <a href> there is invisible to the
// attribute scan … the corpus turned up a real <noscript> case." That markup is
// markup: the tokenizer decodes character references in it and nothing decodes a
// string escape. A backslash there is a path separator, exactly as in the
// attribute the same bytes would form outside the element.
//
// This one needs no obfuscated separator at all — a plain `https://` with a
// Windows-style path is enough, because the raw-text token never reaches the
// html-attr pass.
func TestR55ARawTextElementRunsNoStringDecoder(t *testing.T) {
	m := r55Matcher(t)
	for _, c := range []struct{ name, in string }{
		// decoded "https://www.example.fi\wp-admin/" -> www.example.fi
		{"noscript", `<noscript><a href="https://www.example.fi\wp-admin/">x</a></noscript>`},
		{"iframe", `<iframe><a href="https://www.example.fi\wp-admin/">x</a></iframe>`},
		// decoded "https://www.example.fi\-/p" -> www.example.fi
		{"noscript, an escape JavaScript would fold",
			`<noscript><a href="https://www.example.fi\-/p">x</a></noscript>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := rewriteHTML(t, m, c.in, NewStats(false))
			if !strings.Contains(out, r55Variant) {
				t.Errorf("a browser resolves this to %s and it was served live:\n in  %s\n out %s",
					r55Canonical, c.in, out)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 3. Legacy octal spells a removed control too.
// ---------------------------------------------------------------------------

// origin.escTerminates reads legacy octal — round 54 added the arm, and PLAN
// §5.2 records why octal came back. origin.removedEscLen does not: its cases are
// the literal byte, `\n`/`\r`/`\t`, `\uXXXX`, `\u{...}` and `\xNN`. So `\011`,
// which is a tab, is answered by escTerminates as "not a delimiter, so the host
// continues" and never skipped over by hostTerminated — and the URL parser
// *deletes* a tab, so the host does not continue at all: it ends there.
//
// `<script>fetch("https://www.example.fi\011")</script>` is the production
// origin to a browser and 0f74c2d serves it live. fc2dfdd rewrote it. The same
// value one character longer, `\011x`, is the host www.example.fix and must
// stay untouched — that is the half round 54's own commit message says the
// breaker's tests kept missing, and it is asserted here alongside.
func TestR55LegacyOctalSpellsARemovedControlToo(t *testing.T) {
	m := r55Matcher(t)
	// decoded "https://www.example.fi\t" -> www.example.fi
	leak := `<script>fetch("https://www.example.fi\011")</script>`
	if out := rewriteHTML(t, m, leak, NewStats(false)); !strings.Contains(out, r55Variant) {
		t.Errorf("a browser resolves this to %s and it was served live:\n in  %s\n out %s",
			r55Canonical, leak, out)
	}
	// decoded "https://www.example.fi\tx" -> www.example.fix, a different host
	keep := `<script>fetch("https://www.example.fi\011x")</script>`
	if out := rewriteHTML(t, m, keep, NewStats(false)); out != keep {
		t.Errorf("a browser resolves this to www.example.fix, so it must not change:\n in  %s\n out %s",
			keep, out)
	}
}

// ---------------------------------------------------------------------------
// 4. The ampersand rule is read at one layer out of two.
// ---------------------------------------------------------------------------

// 0f74c2d added a rule to origin.delimAt that an ampersand never ends a host —
// correct against ada, since a host ends at `/ \ ? #` and `&` is not a forbidden
// host code point, so `https://www.example.fi&x` names `www.example.fi&x`. The
// commit message does not mention the change and neither does PLAN, and it was
// added to the byte matcher only. The locator's hostRange still stops at the
// ampersand, so the very next pass rewrites what delimAt has just declined, and
// the two layers disagree about one document — which is the failure round 54's
// own commit message says the one-table design exists to prevent.
//
// The prose case is the one with teeth. `&period;` is a character reference for
// a dot, so the text node reads `https://www.example.fi.x`: a different host,
// which the page must not touch. It rewrites it, the browser then displays
// `https://wt-a--example.ddev.site.x`, and when that text is posted back the
// request direction cannot read it — the variant hostname is written into the
// database §4.3 says stays byte-identical to production, and stays there.
func TestR55TheAmpersandRuleIsReadAtOneLayerOnly(t *testing.T) {
	m := r55Matcher(t)
	rev := r55Reverse(t)

	// decoded "https://www.example.fi&x" -> host www.example.fi&x
	for _, c := range []struct{ name, in string }{
		{"attribute", `<a href="https://www.example.fi&x">`},
		{"prose", `<p>https://www.example.fi&x</p>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if out := rewriteHTML(t, m, c.in, NewStats(false)); out != c.in {
				t.Errorf("a browser resolves this to www.example.fi&x, so it must not change:\n in  %s\n out %s",
					c.in, out)
			}
		})
	}

	// decoded "https://www.example.fi.x" -> host www.example.fi.x
	t.Run("the round trip a reference walks into", func(t *testing.T) {
		in := `<p>https://www.example.fi&period;x</p>`
		out := rewriteHTML(t, m, in, NewStats(false))
		if out != in {
			t.Errorf("a browser resolves this to www.example.fi.x, so it must not change:\n in  %s\n out %s",
				in, out)
		}
		// What the browser holds after decoding what was served, and what an
		// editor or a form posts back.
		posted := strings.TrimSuffix(strings.TrimPrefix(out, "<p>"), "</p>")
		posted = strings.ReplaceAll(posted, "&period;", ".")
		if back := r55Back(rev, posted); strings.Contains(back, r55Variant) {
			t.Errorf("a variant hostname survives the request direction:\n served %s\n sent   %s\n back   %s",
				out, posted, back)
		}
	})
}

// ---------------------------------------------------------------------------
// 5. The carry-over window is sized for a terminator that no longer exists.
// ---------------------------------------------------------------------------

// MaxMatchLen is "the longest pattern, plus room for a trailing root dot,
// ':port' and the delimiter that terminates it" — maxPat + 16 — and the
// straggler sweep uses it as its carry-over window "so that no match can
// straddle a chunk boundary". Round 54 replaced the one-byte delimiter with a
// *run*: hostTerminated now walks forward over every escape that spells a
// character the URL parser removes, and `\u{...}` has no width limit at all.
// Sixteen bytes of slack no longer bounds it.
//
// So the same body is rewritten or not depending on where the 32 KiB read
// boundary happened to fall — which is the failure RewritePrefix's own comment
// on prev calls "not optional", on the other side. The value below is
// www.example.fix to a browser, so both answers cannot be right and the
// streamed one is the wrong one: it puts a variant hostname into a value that
// never pointed at production, and the request direction cannot read
// `wt-a--example.ddev.sitex` back — PLAN §4.3.
//
//	new URL("https://www.example.fi\u{0…09}x") -> www.example.fix
//
// A `\u{...}` that long is not ordinary content; the *window* being unbounded
// is, and this is the shortest input that shows it.
func TestR55TheCarryOverWindowStillBoundsTheTerminator(t *testing.T) {
	m := r55Matcher(t)
	const chunk = 32 * 1024
	canon := "https://" + r55Canonical
	tail := `\u{` + strings.Repeat("0", 40) + `9}x`

	for gap := 20; gap <= 60; gap++ {
		pad := chunk - gap - len(canon)
		if pad < 0 {
			continue
		}
		body := strings.Repeat("z", pad) + canon + tail + strings.Repeat("z", 4096)

		streamed, err := io.ReadAll(NewSweep(strings.NewReader(body), m, nil, Options{Stats: NewStats(false)}))
		if err != nil {
			t.Fatal(err)
		}
		whole, _ := m.Rewrite([]byte(body), SurfaceStraggler, false)
		if strings.Contains(string(streamed), r55Variant) != strings.Contains(string(whole), r55Variant) {
			t.Fatalf("the answer depends on where the read boundary fell (host ends %d bytes "+
				"before the boundary, window is %d): streamed rewrote=%v, whole buffer rewrote=%v",
				gap, m.MaxMatchLen(),
				strings.Contains(string(streamed), r55Variant),
				strings.Contains(string(whole), r55Variant))
		}
		if strings.Contains(string(streamed), r55Variant) {
			t.Fatalf("a browser resolves this to www.example.fix, so nothing may change "+
				"(host ends %d bytes before the boundary)", gap)
		}
	}
}

// ---------------------------------------------------------------------------
// Fix-side coverage for round 55: the mutations the fixes survived on arrival.
// ---------------------------------------------------------------------------

// An escaped colon ends the host, and the port after it has to be read.
//
// Mutation M17 — dropping `:` from escTerminates' delimiter set — survived the
// whole suite, because nothing exercised an escaped colon at all. escTerminates
// answered "delimiter" and the port scan only ever knew `:` and `%3A`, so the
// digits were never taken: `www.example.fi\x3a8443` matched the bare canonical
// and a URL naming a *different* origin under §5.4 was rewritten.
//
// Declining the whole family would have been wrong in the other direction, and
// the `:443` row is why: 443 is https's default, so a browser drops it and the
// origin *is* the canonical. Every expectation here is ada's.
func TestR55AnEscapedColonCarriesAPort(t *testing.T) {
	m := r55Matcher(t)
	for _, c := range []struct {
		url      string
		resolves string
		rewrite  bool
	}{
		// A different origin: the port is not https's default.
		{`https://www.example.fi\x3a8443/x`, "www.example.fi:8443", false},
		{`https://www.example.fi:8443/x`, "www.example.fi:8443", false},
		// The canonical: a browser drops the default port.
		{`https://www.example.fi\x3a443/x`, "www.example.fi", true},
		{`https://www.example.fi\u003a443/x`, "www.example.fi", true},
		{`https://www.example.fi\0723/x`, "www.example.fi", false},
		// Not a URL at all — a colon with no digits and no delimiter after it.
		{`https://www.example.fi\072x`, "(parse error)", false},
		{`https://www.example.fi:x`, "(parse error)", false},
		{`https://www.example.fi:8443x/y`, "(parse error)", false},
		// ...but a colon the authority ends at is the plain host.
		{`https://www.example.fi:/x`, "www.example.fi", true},
	} {
		in := `<script>fetch("` + c.url + `")</script>`
		out := rewriteHTML(t, m, in, NewStats(false))
		if got := strings.Contains(out, r55Variant); got != c.rewrite {
			verb := "was rewritten"
			if !got {
				verb = "was left live"
			}
			t.Errorf("a browser resolves this to %s, and it %s:\n  in  %s\n  out %s",
				c.resolves, verb, in, out)
		}
	}
}

// The carry-over window, isolated.
//
// Three separate mechanisms bound the terminator — maxRemovedRun caps the walk,
// MaxMatchLen adds it to the window, and the `\u{...}` scan stops after six hex
// digits — and the streaming test above is caught by any one of them alone, so
// it pins none of them. These two runs sit either side of the cap: 60 bytes of
// removable escapes must be walked *and* covered by the window, and 100 bytes
// must make both paths give up at the same byte.
func TestR55TheRemovedRunIsBoundedAndCovered(t *testing.T) {
	m := r55Matcher(t)
	const chunk = 32 * 1024
	canon := "https://" + r55Canonical

	// 60 bytes (inside the cap), 100 (still inside the window) and 200 (past
	// both). The last one is the only one that pins the *cap*: below it the
	// window alone is enough lookahead, so an unbounded walk still agrees with
	// itself and the bound is unobserved.
	for _, esc := range []int{30, 50, 100} {
		tail := strings.Repeat(`\t`, esc) + "x"
		for gap := 8; gap <= 140; gap += 4 {
			pad := chunk - gap - len(canon)
			if pad < 0 {
				continue
			}
			body := strings.Repeat("z", pad) + canon + tail + strings.Repeat("z", 4096)
			streamed, err := io.ReadAll(NewSweep(strings.NewReader(body), m, nil, Options{Stats: NewStats(false)}))
			if err != nil {
				t.Fatal(err)
			}
			whole, _ := m.Rewrite([]byte(body), SurfaceStraggler, false)
			if strings.Contains(string(streamed), r55Variant) != strings.Contains(string(whole), r55Variant) {
				t.Fatalf("%d escapes, host ends %d bytes before the boundary: streamed rewrote=%v, "+
					"whole buffer rewrote=%v — the answer depends on where the read boundary fell",
					esc, gap,
					strings.Contains(string(streamed), r55Variant),
					strings.Contains(string(whole), r55Variant))
			}
		}
	}
}

// The escPath guard in removedEscLen, reached through the byte matcher.
//
// Mutation g4 — dropping it — survives every surface test, because on an
// attribute the locator picks the view and never asks removedEscLen at all. The
// matcher does, on a header, where the value is a URL and nothing has decoded
// it: `Location: https://www.example.fi\tx` is the canonical with the path
// `/tx` to a browser, and removing the escape would make it www.example.fix and
// leave a live production origin in the redirect.
func TestR55TheMatcherKnowsAHeaderHasNoEscapes(t *testing.T) {
	m := r55Matcher(t)
	in := `https://` + r55Canonical + `\tx`
	out, _ := m.Rewrite([]byte(in), SurfaceHeader, false)
	if !strings.Contains(string(out), r55Variant) {
		t.Errorf("a browser resolves this to %s and it was left live:\n  in  %s\n  out %s",
			r55Canonical, in, string(out))
	}
	// And the same bytes where a decoder does run resolve elsewhere, so the
	// guard has to be a guard and not a deletion.
	script := `<script>fetch("` + in + `")</script>`
	if got := rewriteHTML(t, m, script, NewStats(false)); strings.Contains(got, r55Variant) {
		t.Errorf("a browser resolves this to %sx, so nothing may change:\n  %s",
			r55Canonical[:len(r55Canonical)-1]+"x", got)
	}
}
