package rewrite

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 45, on 8fabead. Settling PLAN.md's "known gap, measured and not yet
// closed: /", and looking for a producer.

const (
	r45Canon   = "www.example.fi"
	r45Variant = "wt-a--example.ddev.site"
)

func r45Fwd(t *testing.T) *origin.Matcher {
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://" + r45Canon),
		Variant:   origin.MustParse("https://" + r45Variant),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func r45Rev(t *testing.T) *origin.Matcher {
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://" + r45Variant),
		Variant:   origin.MustParse("https://" + r45Canon),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// u is a \uXXXX escape, assembled from bytes so no editor or tool in the
// pipeline can quietly interpret it.
func u(hex string) string { return "\\" + "u" + hex }

// TestR45UnicodeEscapesLeakOnEveryHTMLSurface confirms PLAN §"known gap" and
// maps its true extent.
//
// PLAN records the gap for `/` (the slash) and names three surfaces. It is
// wider than that in one axis and narrower in another:
//
//   - Wider: any byte of the *authority* spelled `\uXXXX` defeats the scan the
//     same way. `.` for the dots in the host is the same hole, and so is
//     `-` for a hyphen — which is the one with a producer (see the next
//     test).
//   - The surfaces are every non-JSON surface, not three: href, data-*, inline
//     script, `<script type="application/json">`, text nodes. Only a body served
//     as top-level `application/json` decodes them, because RewriteJSON runs
//     jsontext.AppendUnquote in decodeJSONLeak.
//
// Each candidate below is a shape a browser resolves to https://www.example.fi/x
// once the consuming parser (JSON.parse, or JS's own string literal grammar)
// has run its escapes, so each is test 28.
func TestR45UnicodeEscapesLeakOnEveryHTMLSurface(t *testing.T) {
	m := r45Fwd(t)
	// Every one resolves to https://www.example.fi/x once the consuming parser
	// has run its escapes, so every one must be rewritten by the forward map.
	cands := []struct{ name, url string }{
		{"slash", "https:" + u("002F") + u("002F") + r45Canon + "/x"},
		{"slash, lowercase hex", "https:" + u("002f") + u("002f") + r45Canon + "/x"},
		{"one slash escaped, one not", "https:" + u("002F") + "/" + r45Canon + "/x"},
		{"backslash-slash then escaped", "https:\\/" + u("002F") + r45Canon + "/x"},
		{"scheme-relative", u("002F") + u("002F") + r45Canon + "/x"},
		{"colon", "https" + u("003A") + "//" + r45Canon + "/x"},
		{"dot in the host", "https://www" + u("002E") + "example" + u("002E") + "fi/x"},
	}
	surfaces := []struct {
		name string
		wrap func(string) string
	}{
		{"script type=application/json", func(s string) string {
			return `<script type="application/json">{"k":"` + s + `"}</script>`
		}},
		{"data- attribute", func(s string) string { return `<div data-cfg='{"k":"` + s + `"}'>y</div>` }},
		{"inline script", func(s string) string { return `<script>var a="` + s + `";</script>` }},
		{"href", func(s string) string { return `<a href="` + s + `">z</a>` }},
		{"text node", func(s string) string { return `<p>{"k":"` + s + `"}</p>` }},
	}
	for _, c := range cands {
		for _, s := range surfaces {
			in := s.wrap(c.url)
			out := rewriteHTML(t, m, in, NewStats(false))
			if out == in {
				t.Errorf("%s / %s: went out byte-identical, dereferenceable:\n%s",
					s.name, c.name, out)
			}
		}
	}
}

// TestR45TopLevelJSONDoesDecodeThem is the other half of the map: the surface
// PLAN says is covered really is covered, so the gap is exactly "everything but
// a body whose Content-Type is application/json".
func TestR45TopLevelJSONDoesDecodeThem(t *testing.T) {
	m := r45Fwd(t)
	body := `{"k":"https:` + u("002F") + u("002F") + r45Canon + `/x"}`
	out := string(RewriteJSON([]byte(body), m, NewStats(false), slog.Default(), false))
	if strings.Contains(out, r45Canon) {
		t.Fatalf("the JSON arm was expected to decode \\uXXXX; it did not:\n%s", out)
	}
}

// -----------------------------------------------------------------------------
// The producer.
// -----------------------------------------------------------------------------

// TestR45BlockAttributesDoNotComeHome is the reachable half.
//
// PLAN says the gap is "a real shape with no known producer", having looked for
// an emitter of `/`. There is a stock WordPress emitter of the *family* —
// it just does not escape the slash. Gutenberg's block serializer, in
// @wordpress/blocks/src/api/serializer.js:
//
//	export function serializeAttributes( attributes ) {
//		return JSON.stringify( attributes )
//			.replace( /--/g, '\\u002d\\u002d' )   // Don't break HTML comments.
//			.replace( /</g,  '\\u003c' )
//			.replace( />/g,  '\\u003e' )
//			.replace( /&/g,  '\\u0026' )
//			.replace( /\\"/g, '\\u0022' );
//	}
//
// and its PHP twin serialize_block_attributes() in wp-includes/blocks.php,
// which runs the same four preg_replace calls over wp_json_encode's output.
// Every block comment delimiter in post_content is written by one of those two.
//
// The `--` rule is not incidental here. hostshift's own DefaultVariantPattern is
// `{slug}--{leftmost-label}` (internal/config/config.go), so **every variant
// hostname hostshift can generate contains `--` by construction**. The forward
// pass splices that host into a block attribute; the editor re-serialises the
// block on save; and what comes back is
//
//	{"url":"https://wt-a--example.ddev.site/…"}
//
// which the reverse direction cannot read. `-` is not one of the escapes
// the byte matcher carries, the locator walks the raw bytes, and the four
// urlobf views decode references, percent-escapes and CSS escapes — not JSON's
// \uXXXX. So the variant hostname goes upstream and is written into the shared
// production database, and production then serves
// `https://wt-a--example.ddev.site/…` to real visitors.
//
// That is not a leak, it is the failure PLAN §4.3 says the whole design exists
// to prevent, and unlike `/` it fires on ordinary use of the block editor.
func TestR45BlockAttributesDoNotComeHome(t *testing.T) {
	rev := r45Rev(t)

	// What Gutenberg serialises for a core/cover block whose background image
	// the forward pass rewrote to the variant. `url` has no `source`, so it
	// lives in the comment delimiter — the same is true of core/navigation-link,
	// core/social-link and core/media-text's mediaLink.
	esc := "https://wt-a" + u("002d") + u("002d") + "example.ddev.site/wp-content/uploads/bg.jpg"
	raw := "https://" + r45Variant + "/wp-content/uploads/bg.jpg"
	blockOf := func(url string) string {
		return `<!-- wp:cover {"url":"` + url + `","dimRatio":50} -->` +
			`<div class="wp-block-cover"></div><!-- /wp:cover -->`
	}

	t.Run("REST save, application/json", func(t *testing.T) {
		// wp.apiFetch POST /wp/v2/posts/1 — the block comment is a JSON string,
		// so its own `"` are `\"` and its `-` is `\\u002d` on the wire.
		wire := func(block string) string {
			s := strings.ReplaceAll(block, `\`, `\\`)
			s = strings.ReplaceAll(s, `"`, `\"`)
			return `{"content":"` + s + `"}`
		}
		// Control: the same save with the hyphens unescaped comes home.
		ctl := string(RewriteJSON([]byte(wire(blockOf(raw))), rev, NewStats(false), slog.Default(), false))
		if !strings.Contains(ctl, r45Canon) {
			t.Fatalf("control: the plain spelling did not map back either:\n%s", ctl)
		}
		// The whole of proxy.go's bodyJSON arm, straggler sweep included, so the
		// claim is about what actually goes upstream rather than about one pass.
		out := string(RepairSerialized(
			RewriteJSON([]byte(wire(blockOf(esc))), rev, NewStats(false), slog.Default(), false),
			func(b []byte) []byte { return SweepBytes(b, rev, NewStats(false), slog.Default()) }))
		if strings.Contains(out, "wt-a") {
			t.Errorf("the variant hostname went upstream into the shared database:\n%s", out)
		}
	})

	t.Run("classic post.php, urlencoded", func(t *testing.T) {
		pct := func(s string) string {
			return strings.NewReplacer(
				":", "%3A", "/", "%2F", " ", "+", `"`, "%22",
				"<", "%3C", ">", "%3E", "!", "%21", `\`, "%5C",
				"{", "%7B", "}", "%7D", ",", "%2C", "=", "%3D", "&", "%26",
			).Replace(s)
		}
		rw := func(b []byte) []byte {
			nv, _ := rev.Rewrite(b, SurfaceRequestBody, false)
			return HostLeaksBack(rev, nv)
		}
		ctl := string(RepairSerializedFields([]byte("content="+pct(blockOf(raw))), rw))
		if !strings.Contains(ctl, "www.example.fi") && !strings.Contains(ctl, "www%2Eexample") {
			t.Fatalf("control: the plain spelling did not map back either:\n%s", ctl)
		}
		out := string(RepairSerializedFields([]byte("content="+pct(blockOf(esc))), rw))
		if strings.Contains(out, "wt-a") {
			t.Errorf("the variant hostname went upstream into the shared database:\n%s", out)
		}
	})

	t.Run("forward direction, an IDN canonical", func(t *testing.T) {
		// The mirror, and the one case where the escape hits the *canonical*
		// side and so is a test-28 leak rather than a write.
		//
		// Punycode's ACE prefix is `xn--`. origin.MustParse("https://hämeen.fi")
		// stores `xn--hmeen-gra.fi`, and PLAN §5.5 calls IDN "real for .fi
		// client domains" — so for every IDN site whose database holds the
		// punycode spelling, a block attribute carrying it is written by
		// serializeAttributes as `xn--hmeen-gra.fi` and the forward
		// pass cannot see it. The block editor's own JSON.parse and PHP's
		// parse_blocks both give the browser back a live production origin.
		m, err := origin.NewMatcher([]origin.Pair{{
			Canonical: origin.MustParse("https://hämeen.fi"),
			Variant:   origin.MustParse("https://wt-a--hameen.ddev.site"),
		}})
		if err != nil {
			t.Fatal(err)
		}
		in := `<!-- wp:cover {"url":"https://xn` + u("002d") + u("002d") +
			`hmeen-gra.fi/bg.jpg"} -->`
		if out := rewriteHTML(t, m, in, NewStats(false)); out == in {
			t.Errorf("a production origin reached the browser byte-identical:\n%s", out)
		}
	})
}

// -----------------------------------------------------------------------------
// What the fix looks like, prototyped here so the claim "a composed view closes
// it" is demonstrated rather than asserted.
// -----------------------------------------------------------------------------

// stripForJSONUnicodeR45 is the fifth decoder urlobf.go would need: stripForURL
// with JSON's \uXXXX escapes decoded into the view.
//
// Deliberately narrow — only escapes that decode to an ASCII byte legal in an
// authority or in the `scheme://` run ahead of it, which is what keeps it from
// ever *emitting* a control character (the rule stripForURL's comment sets) and
// keeps a surrogate pair out of the picture entirely. Everything else is copied
// through as the six literal bytes it is, so the view stays position-mapped and
// nothing is re-serialised.
func stripForJSONUnicodeR45(v []byte) normalised {
	if !hasJSONUnicodeR45(v) {
		return stripForURL(v)
	}
	dec := make([]byte, 0, len(v))
	pos := make([]int, 0, len(v))
	end := make([]int, 0, len(v))
	for i := 0; i < len(v); {
		if c, k := jsonUnicodeAtR45(v[i:]); k > 0 {
			dec = append(dec, c)
			pos = append(pos, i)
			end = append(end, i+k)
			i += k
			continue
		}
		dec = append(dec, v[i])
		pos = append(pos, i)
		end = append(end, i+1)
		i++
	}
	return stripRemovals(dec, pos, end)
}

func hasJSONUnicodeR45(v []byte) bool {
	for i := 0; i+1 < len(v); i++ {
		if v[i] == '\\' && (v[i+1] == 'u' || v[i+1] == 'U') {
			return true
		}
	}
	return false
}

// jsonUnicodeAtR45 returns the byte a \uXXXX escape at b decodes to, and its
// length, or 0. Only 0x20..0x7E, so no control character can ever be emitted.
func jsonUnicodeAtR45(b []byte) (byte, int) {
	if len(b) < 6 || b[0] != '\\' || (b[1] != 'u' && b[1] != 'U') {
		return 0, 0
	}
	var n int
	for _, c := range b[2:6] {
		switch {
		case c >= '0' && c <= '9':
			n = n<<4 | int(c-'0')
		case c >= 'a' && c <= 'f':
			n = n<<4 | int(c-'a'+10)
		case c >= 'A' && c <= 'F':
			n = n<<4 | int(c-'A'+10)
		default:
			return 0, 0
		}
	}
	if n < 0x20 || n > 0x7E {
		return 0, 0
	}
	return byte(n), 6
}

// TestR45TheComposedViewClosesIt drives the prototype through the engine's own
// splice, on the engine's own hostReplacer. Every shape the first two tests
// showed going out byte-identical is rewritten, and — the half that keeps a
// widening honest — nothing that already resolved elsewhere is touched.
func TestR45TheComposedViewClosesIt(t *testing.T) {
	h := hostsFor(r45Fwd(t))
	splice := func(s string) string {
		return string(h.spliceHostsIn(stripForJSONUnicodeR45([]byte(s)), []byte(s), urlTokenStarts, true, nil))
	}
	for _, c := range []string{
		"https:" + u("002F") + u("002F") + r45Canon + "/x",
		"https:" + u("002f") + u("002f") + r45Canon + "/x",
		"https" + u("003A") + "//" + r45Canon + "/x",
		"https://www" + u("002E") + "example" + u("002E") + "fi/x",
		u("002F") + u("002F") + r45Canon + "/x",
	} {
		if out := splice(c); strings.Contains(out, r45Canon) {
			t.Errorf("still leaks with the composed view: %s -> %s", c, out)
		}
	}
	// And the reverse map, which is where the producer lives.
	hr := hostsFor(r45Rev(t))
	esc := "https://wt-a" + u("002d") + u("002d") + "example.ddev.site/bg.jpg"
	got := string(hr.spliceHostsIn(stripForJSONUnicodeR45([]byte(esc)), []byte(esc), urlTokenStarts, true, nil))
	if !strings.Contains(got, r45Canon) {
		t.Errorf("the block-serializer spelling still does not come home: %s", got)
	}
	// No false positives: a host that only *looks* adjacent, and an escape that
	// is not an authority byte, must both be left exactly as written.
	for _, c := range []string{
		"https:" + u("002F") + u("002F") + "www.example.fi.evil.test/x",
		"https:" + u("002F") + u("002F") + "notexample.fi/x",
		"a string with " + u("0022") + " in it and no origin",
	} {
		if out := splice(c); out != c {
			t.Errorf("false positive: %s -> %s", c, out)
		}
	}
}

// TestR45TheJSONViewNeverEmitsAControlCharacter: the rule stripForURL's comment
// sets, applied to the new view.
//
// A decoder that can emit a control character was one of the XSS holes this file
// sits next to, and here it would do a second kind of damage. The view's bytes
// are what the authority scanner reads, so decoding an escaped LF into the view
// invents a byte the URL parser strips: the scan would then locate a host across
// a separator that is not there and splice over the wrong range.
//
// Escapes outside printable ASCII are therefore left as written. That is not a
// leak — a control character in a hostname is not one a browser resolves, and
// stripForURL removes the literal spellings before matching anyway.
func TestR45TheJSONViewNeverEmitsAControlCharacter(t *testing.T) {
	for _, esc := range []string{
		"\\u0000", "\\u0009", "\\u000A", "\\u000D", "\\u001F", "\\u007F", "\\u00e4",
	} {
		v := []byte("https://www.example" + esc + ".fi/x")
		n := stripForJSONEsc(v)
		for i, c := range n.b {
			if c < 0x20 || c == 0x7F {
				t.Errorf("%s: the view emitted control byte %#02x at %d: %q", esc, c, i, n.b)
				break
			}
		}
		// And the buffer is left alone, since nothing matched through it.
		if got := string(HostLeaks(r45Matcher(t), v, true)); got != string(v) {
			t.Errorf("%s: buffer changed: %q", esc, got)
		}
	}
}

func r45Matcher(t *testing.T) *origin.Matcher {
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt-a--example.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}
