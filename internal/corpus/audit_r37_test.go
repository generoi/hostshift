package corpus

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// r37serve publishes a body over HTTP as text/html.
func r37serve(t *testing.T, body string) *url.URL {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// r37map is the two-origin map the tests below share. The hosts differ in byte
// length (14 against 11) so a stale length is distinguishable from a correct one.
func r37map(t *testing.T) *origin.Map {
	t.Helper()
	m, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://www.canon.test"),
		Variant:   origin.MustParse("https://v.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// An `&` inside a serialized string hides the unread rewrite that follows it.
//
// `UnreadRewrites` splits the body with `fieldBreak` before asking anything —
// the same `&`-splitting `RepairSerializedFields` uses for an
// application/x-www-form-urlencoded body, where a raw `&` really is a separator.
// An HTML body is not a form. `RepairSerialized`, which is what the proxy runs
// for this page, does not split, so the metric is measuring a different buffer
// from the one the rewriter edited.
//
// The consequence is exact and reachable. A serialized string holding an
// ordinary ampersand followed by a URL — `Terms & conditions at https://host/…`,
// which is what a widget title, a menu item description or any prose option
// looks like — is cut in two:
//
//	field 1  a:1:{i:0;s:47:\\u0022Terms       — has `s:47:`, has no host, so rw
//	                                            changes nothing and it is skipped
//	field 2   conditions at https://…\\u0022;} — has the host, but no type letter
//	                                            followed by `:` and a digit, so
//	                                            mayHoldSerialized says no
//
// Neither field satisfies both halves of the test, so the count is 0. Meanwhile
// the unsplit rewriter substitutes the host in a spelling it cannot read, does
// not re-emit `s:47:`, and PHP 8.4 `unserialize()` returns false on the bytes
// the browser is served (verified directly: the served string is 44 bytes behind
// a `s:47:` header).
//
// `BrokenSerialized` cannot report it either — that is the structural
// cancellation `UnreadRewrites` was added to survive — so every column reads
// clean and the run prints GREEN over a page carrying a blob PHP refuses.
func TestAnAmpersandBeforeTheHostHidesAnUnreadRewrite(t *testing.T) {
	m := r37map(t)

	// Built, never typed, so no hardcoded `s:N:` can be off by a byte. The `&`
	// sits before the host and after the `s:N:` header, which is what puts the
	// two halves of the test in different fields.
	canonURL := "https://www.canon.test/terms"
	variantURL := "https://v.ddev.site/terms"
	if len(canonURL) == len(variantURL) {
		t.Fatalf("the fixture's two URLs are the same length, so this asserts nothing")
	}
	data := "Terms & conditions at " + canonURL
	blob := fmt.Sprintf(`a:1:{i:0;s:%d:"%s";}`, len(data), data)

	// A spelling no syntax in repairAt reads: the JSON hex quote encoded a
	// second time, which is what a JSON document carrying a JSON string
	// carrying the blob spells it as. `\\u0022` assembled from bytes.
	q := string([]byte{'\\', '\\', 'u', '0', '0', '2', '2'})
	wire := strings.ReplaceAll(blob, `"`, q)
	page := `<div data-wp-context='{"b":"` + wire + `"}'>x</div>`

	served, err := io.ReadAll(rewrite.NewResponseBody(
		strings.NewReader(page), m.Forward(), nil, rewrite.Options{}))
	if err != nil {
		t.Fatal(err)
	}

	// The premises, asserted so a fix cannot make this test pass vacuously.
	if !bytes.Contains(served, []byte("v.ddev.site")) {
		t.Fatalf("the engine did not rewrite the fixture at all, so this asserts nothing:\n%s", served)
	}
	if !bytes.Contains(served, []byte(fmt.Sprintf("s:%d:", len(data)))) {
		t.Skip("the walk now re-emits this length; nothing broken is served")
	}
	// PHP refuses the served value: the header still declares the canonical
	// length over data that is three bytes shorter.
	servedData := strings.Replace(data, canonURL, variantURL, 1)
	if len(servedData) == len(data) {
		t.Fatalf("the fixture's rewrite does not change the byte count")
	}

	results, err := Run(context.Background(), Options{
		Canonical: r37serve(t, page), Variant: r37serve(t, string(served)),
		Map: m, Paths: []string{"/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	green := WriteReport(&buf, results)
	if green {
		t.Errorf("the run was GREEN over a page whose serialized value PHP refuses "+
			"(s:%d: over %d bytes of data):\n  canonical %s\n  served    %s\n"+
			"  unread=%d broken=%d\n%s",
			len(data), len(servedData), page, served,
			results[0].UnreadRewrites, results[0].BrokenSerialized, buf.String())
	}
}

// An ordinary healthy page turns the run RED.
//
// `UnreadRewrites` gates "this looks serialized" on `mayHoldSerialized`, which
// is documented as *the cheap gate before the walk* and built to be wrong in
// one direction on purpose: "It must never say no to something the walk would
// have repaired." All it asks is whether some `b i d s a O R r E C` is followed
// by `:` and a digit anywhere in the buffer.
//
// Minified CSS is full of that. `border:1px` is `r` `:` `1`. So is
// `background:0 0`, `order:2`, and the `{a:1,b:2}` of any minified script. None
// of them parses as a serialized value, so `repairField` reports `found=false`
// — and `found=false` is the metric's entire evidence that something was
// "declined". The remaining condition is only that the rewrite changed *some*
// byte in the same field, and a `<link rel="canonical">` in the same `<head>`
// does that.
//
// So the page below — which round-trips byte-for-byte, leaks nothing, and
// contains no serialized data at all — is reported as
//
//	1 serialized-shaped value(s) rewritten in a spelling this build cannot read
//
// and `WriteReport` returns false. Every real WordPress page has inline
// minified CSS and a canonical link, so this is not a corner: it is the
// ordinary case, and a check that is RED on every page is one nobody reads —
// which is the failure mode this project rates worse than a false GREEN.
func TestAnOrdinaryPageWithMinifiedCSSIsNotAnUnreadRewrite(t *testing.T) {
	m := r37map(t)

	page := `<!DOCTYPE html><html><head>` +
		`<link rel="canonical" href="https://www.canon.test/hello-world/">` +
		`<style>.wp-block-image img{border:1px solid #e0e0e0}</style>` +
		`</head><body><p>Hello</p></body></html>`

	served, err := io.ReadAll(rewrite.NewResponseBody(
		strings.NewReader(page), m.Forward(), nil, rewrite.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(served, []byte("v.ddev.site")) {
		t.Fatalf("the engine did not rewrite the fixture at all, so this asserts nothing:\n%s", served)
	}
	// Nothing serialized anywhere in it, by construction.
	if rewrite.BrokenSerialized([]byte(page)) != 0 || rewrite.BrokenSerialized(served) != 0 {
		t.Fatalf("the fixture is not the healthy page this test needs")
	}

	results, err := Run(context.Background(), Options{
		Canonical: r37serve(t, page), Variant: r37serve(t, string(served)),
		Map: m, Paths: []string{"/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	green := WriteReport(&buf, results)
	if results[0].UnreadRewrites != 0 {
		t.Errorf("%d unread rewrite(s) reported on a page whose only serialized-shaped "+
			"bytes are `border:1px` in a stylesheet:\n  page   %s\n  served %s\n%s",
			results[0].UnreadRewrites, page, served, buf.String())
	}
	if !green {
		t.Errorf("the run was RED over a healthy page that round-trips exactly:\n%s", buf.String())
	}
}

// The metric asks the byte matcher, but the proxy runs the whole engine — so a
// host the engine rewrites through one of its decoder views is edited without
// being counted.
//
// diff.go feeds `UnreadRewrites` this rewriter:
//
//	out, _ := o.Map.Forward().Rewrite(b, rewrite.SurfaceText, false)
//
// `origin.Matcher.Rewrite` is the byte matcher alone. The proxy is not the byte
// matcher alone: `urlobf.go` adds the URL-parser view, the IDNA fold, the CSS
// view and the character-reference views, and those are what rewrite a host
// spelled `https:&#47;&#47;www.canon.test/terms`. `countLeaks` in this same file
// records having learned exactly this — "every leak class found since —
// obfuscated separators, folded hosts, CSS escapes, character references — was
// invisible by construction" — and was changed to push bytes back through the
// real pipeline. `UnreadRewrites` reintroduces the same blindness one column
// over, and here it hides corruption rather than a leak.
//
// The fixture is a serialized value in a spelling repairAt cannot read, whose
// URL separators are written as numeric references — the form §"obfuscated
// separators" exists for, and which the canonical page decodes to exactly the
// 28 bytes `s:28:` declares. The engine rewrites it (and decodes the
// references while it is there), the value is served as `s:28:` over 25 bytes,
// and PHP 8.4 `unserialize()` returns false. `BrokenSerialized` counts 1 on
// each side and cancels; `UnreadRewrites` reports 0 because its rewriter cannot
// see the host at all. GREEN.
func TestAHostBehindCharacterReferencesIsRewrittenWithoutBeingCounted(t *testing.T) {
	m := r37map(t)

	canonURL := "https://www.canon.test/terms"
	variantURL := "https://v.ddev.site/terms"
	if len(canonURL) == len(variantURL) {
		t.Fatalf("the fixture's two URLs are the same length, so this asserts nothing")
	}
	// Length from the *decoded* data, which is what PHP counts: the references
	// below decode back to these same 28 bytes.
	blob := fmt.Sprintf(`a:1:{i:0;s:%d:"%s";}`, len(canonURL), canonURL)
	q := string([]byte{'\\', '\\', 'u', '0', '0', '2', '2'})
	wire := strings.ReplaceAll(strings.ReplaceAll(blob, `"`, q), "//", "&#47;&#47;")
	if !strings.Contains(wire, "https:&#47;&#47;www.canon.test") {
		t.Fatalf("fixture does not carry the reference-obfuscated origin: %s", wire)
	}
	// Single-quoted attribute, and the value carries no `'` or `"` to close it.
	if strings.ContainsAny(wire, "'\"") {
		t.Fatalf("fixture would close its own attribute: %s", wire)
	}
	page := `<div data-x='` + wire + `'>x</div>`

	served, err := io.ReadAll(rewrite.NewResponseBody(
		strings.NewReader(page), m.Forward(), nil, rewrite.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(served, []byte("v.ddev.site")) {
		t.Fatalf("the engine did not rewrite the fixture at all, so this asserts nothing:\n%s", served)
	}
	if !bytes.Contains(served, []byte(fmt.Sprintf("s:%d:", len(canonURL)))) {
		t.Skip("the walk now re-emits this length; nothing broken is served")
	}
	if !bytes.Contains(served, []byte(variantURL)) {
		t.Skip("the engine no longer decodes the references here; the fixture needs rebuilding")
	}

	results, err := Run(context.Background(), Options{
		Canonical: r37serve(t, page), Variant: r37serve(t, string(served)),
		Map: m, Paths: []string{"/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if green := WriteReport(&buf, results); green {
		t.Errorf("the run was GREEN over a page served with s:%d: in front of %d bytes "+
			"(PHP unserialize() returns false):\n  canonical %s\n  served    %s\n"+
			"  unread=%d broken=%d\n%s",
			len(canonURL), len(variantURL), page, served,
			results[0].UnreadRewrites, results[0].BrokenSerialized, buf.String())
	}
}

// A Tier 2 body the proxy never touches turns the run RED.
//
// diff.go computes `UnreadRewrites` at line 259, before it has looked at the
// content type at all:
//
//	r.UnreadRewrites = rewrite.UnreadRewrites(canon.body, …)
//	r.ContentType = variant.contentType
//	r.Leaks, r.Tier2 = countLeaks(o.Map.Forward(), variant)
//
// `countLeaks` has the guards — `if r.attachment { return 0, 0 }` and the
// Tier 2 arm — because scoring a body the proxy is documented not to rewrite
// answers the wrong question, and `WriteReport` deliberately does not let a
// Tier 2 count turn the run red: "It does not turn the run RED, because the
// proxy is doing what it says it does". `UnreadRewrites` has no such guard and
// does turn the run red.
//
// So this stylesheet — which the proxy passes through byte for byte, which is
// therefore identical on both sides, and whose only serialized-shaped bytes are
// the `r:1` inside `border:1px` — is reported as an unread rewrite and the run
// is RED. The same is true of an attachment: §5 skips those by design, whatever
// bytes they contain.
func TestATier2BodyTheProxyNeverRewritesIsNotAnUnreadRewrite(t *testing.T) {
	m := r37map(t)

	css := `.a{border:1px solid #eee;background:url(https://www.canon.test/x.png)}`
	serve := func(body string) *url.URL {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/css")
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(s.Close)
		u, err := url.Parse(s.URL)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}

	// Both sides identical: the proxy does not rewrite text/css, so this is
	// exactly what it serves.
	results, err := Run(context.Background(), Options{
		Canonical: serve(css), Variant: serve(css),
		Map: m, Paths: []string{"/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	green := WriteReport(&buf, results)
	if results[0].UnreadRewrites != 0 {
		t.Errorf("%d unread rewrite(s) on a text/css body the proxy passes through "+
			"untouched:\n  %s\n%s", results[0].UnreadRewrites, css, buf.String())
	}
	if !green {
		t.Errorf("the run was RED over a Tier 2 body, which WriteReport's own Tier 2 "+
			"arm says must not happen:\n%s", buf.String())
	}
}
