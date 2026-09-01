package rewrite

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 52. The cross product over locateHostIn's replacement construction.
//
// Six of the last eight LARGE findings were one facet of this one function, and
// each round closed the facet it found. This is the whole surface at once:
// every combination of separator encoding, slash-run length, scheme agreement,
// port on either side, IDN declaration, userinfo, IPv6 literal, root dot, case
// and surrounding bytes, in both directions and on both surfaces — 253,680
// cells.
//
// Every cell writes one URL naming the `from` origin in one encoding, runs the
// locator over it, decodes the result the way the *consuming* parser would, and
// asks a model of the WHATWG URL parser which origin a browser would resolve.
// The assertion is never "the bytes are what the code emits"; it is "the browser
// lands on the variant, on the same path, with the encoding still intact, and
// with every byte this pass had no business touching still there".
//
// Four classes fail at 7ecb95c, and the whole existing suite is green on all
// four:
//
//   - the userinfo was deleted (8,372 cells) — the needScheme arm splices from
//     the scheme through the authority end and writes back scheme + separator +
//     host, so the credentials in between are dropped. The other two arms keep
//     them. Through HostLeaksBack that destroys a value in the shared database.
//   - resolves to the wrong origin (8,260) — an IPv6 literal behind userinfo is
//     not located at all (hostRange's bracket branch is gated on the authority
//     starting with `[`, and userinfo precedes it), and a root dot on a text
//     surface with a ported target emits `host:port.`, whose port no browser
//     will parse.
//   - a literal `://` over an encoded separator (5,271) — schemeSepAt knows
//     `%3A` and a bare backslash and answers raw to everything else, so the
//     percent-then-JSON view loses its encoding. Round 50's harm, one view over.
//
// Each class goes to zero under a fix, and all four together leave the rest of
// the suite green; that is what says the oracle here is not itself the bug.

// ---------------------------------------------------------------------------
// A model of the parser, for the oracle.
// ---------------------------------------------------------------------------

// r52Resolve is the WHATWG URL parser reduced to what this file needs: given a
// reference and the scheme the document is served on, the origin the browser
// resolves and the path that follows it. ok is false when the reference is
// relative (no authority), which is a different answer from any origin.
func r52Resolve(ref, docScheme string) (originStr, rest string, ok bool) {
	// Leading and trailing C0 controls and spaces are stripped, then tab, LF
	// and CR are removed from anywhere.
	s := strings.Trim(ref, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\v\f\r\x0e\x0f \x1f")
	s = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(s)

	scheme := ""
	i := 0
	for _, cand := range []string{"https:", "http:"} {
		if len(s) >= len(cand) && strings.EqualFold(s[:len(cand)], cand) {
			scheme = strings.TrimSuffix(cand, ":")
			i = len(cand)
			break
		}
	}
	slashes := 0
	for i+slashes < len(s) && (s[i+slashes] == '/' || s[i+slashes] == '\\') {
		slashes++
	}
	switch {
	case scheme != "" && scheme != docScheme:
		// special authority slashes -> ignore-slashes: any run, zero included.
		i += slashes
	case scheme != "" && slashes >= 2:
		i += slashes
	case scheme != "":
		return "", "", false // special-relative: a path
	case slashes >= 2:
		scheme = docScheme
		i += slashes
	default:
		return "", "", false
	}

	// The authority runs to the first / \ ? #.
	j := i
	for j < len(s) && s[j] != '/' && s[j] != '\\' && s[j] != '?' && s[j] != '#' {
		j++
	}
	auth, rest := s[i:j], s[j:]
	if k := strings.LastIndexByte(auth, '@'); k >= 0 {
		auth = auth[k+1:]
	}
	host, port := auth, ""
	if strings.HasPrefix(auth, "[") {
		if k := strings.IndexByte(auth, ']'); k >= 0 {
			host = auth[:k+1]
			if k+1 < len(auth) && auth[k+1] == ':' {
				port = auth[k+2:]
			}
		}
	} else if k := strings.IndexByte(auth, ':'); k >= 0 {
		host, port = auth[:k], auth[k+1:]
	}
	if host == "" {
		return "", "", false
	}
	// The parser percent-decodes, then runs domain-to-ASCII.
	if !strings.HasPrefix(host, "[") {
		host = string(percentDecode([]byte(host)))
		host = strings.ToLower(host)
		if a, err := origin.HostFold(host); err == nil {
			host = a
		}
		host = strings.TrimSuffix(host, ".")
	} else {
		host = strings.ToLower(host)
	}
	port = origin.NormalisePort(scheme, port)
	// Backslashes in the path are slashes to the parser.
	rest = strings.ReplaceAll(rest, "\\", "/")
	if port != "" {
		return scheme + "://" + host + ":" + port, rest, true
	}
	return scheme + "://" + host, rest, true
}

func r52OriginOf(o origin.Origin) string { return o.Scheme + "://" + o.HostPort() }

// ---------------------------------------------------------------------------
// The encoding axis: how the `://` between scheme and host is spelled, and what
// decodes it.
// ---------------------------------------------------------------------------

type r52Enc struct {
	name string
	sep  string // written between the scheme name and the host
	// dec is the decode the consuming parser performs before a URL parser runs.
	dec func(string) string
	// rawSlashIsStructural marks a context where turning the encoded separator
	// into a literal `://` changes what the surrounding string means — a
	// percent-encoded origin inside one path segment.
	rawSlashIsStructural bool
	// omitScheme drops the scheme name, for the scheme-relative shapes.
	omitScheme bool
}

func r52Dec(f func([]byte) normalised) func(string) string {
	return func(s string) string { return string(f([]byte(s)).b) }
}

func r52Compose(outer, inner func([]byte) normalised) func(string) string {
	return func(s string) string { return string(inner(outer([]byte(s)).b).b) }
}

// r52BS is a single backslash, spelled without one.
var r52BS = string(rune(92))

func r52Encodings() []r52Enc {
	id := func(s string) string { return s }
	return []r52Enc{
		{name: "raw", sep: "://", dec: id},
		{name: "backslash", sep: `:\\`, dec: id},
		{name: "percent", sep: "%3A%2F%2F", dec: r52Dec(stripForPercent), rawSlashIsStructural: true},
		{name: "percent-lc", sep: "%3a%2f%2f", dec: r52Dec(stripForPercent), rawSlashIsStructural: true},
		{name: "json-slash", sep: `:\/\/`, dec: r52Dec(stripForJSONEsc)},
		{name: "json-u", sep: r52BS + "u003a" + r52BS + "u002f" + r52BS + "u002f", dec: r52Dec(stripForJSONEsc)},
		{name: "css", sep: `\3a\2f\2f`, dec: r52Dec(stripForCSS)},
		{name: "css-space", sep: `\3a \2f \2f `, dec: r52Dec(stripForCSS)},
		{name: "refs", sep: "&#58;&#47;&#47;", dec: r52Dec(stripForRefs)},
		{name: "refs-hex", sep: "&#x3A;&#x2F;&#x2F;", dec: r52Dec(stripForRefs)},
		{name: "refs+css", sep: `&#92;3a&#92;2f&#92;2f`, dec: r52Compose(stripForRefs, stripForCSS)},
		{name: "refs+json", sep: `&#92;u003a&#92;u002f&#92;u002f`, dec: r52Compose(stripForRefs, stripForJSONEsc)},
		{name: "percent+json", sep: `%5Cu003a%5Cu002f%5Cu002f`, dec: r52Compose(stripForPercent, stripForJSONEsc), rawSlashIsStructural: true},

		// The slash-run axis. The parser's special-authority-ignore-slashes
		// state accepts a run of any length — zero included — when the
		// reference's scheme differs from the document's, and needs two when it
		// does not. Both halves are exercised: the first is an authority the
		// locator must rewrite, the second a *path* it must leave alone.
		{name: "slash0", sep: ":", dec: id},
		{name: "slash1", sep: ":/", dec: id},
		{name: "slash3", sep: ":///", dec: id},
		{name: "slash5", sep: "://///", dec: id},
		{name: "slash-fb", sep: `:/\`, dec: id},
		{name: "slash-bf", sep: `:\/`, dec: id},
		{name: "slash-tab", sep: ":/\t/", dec: id},
		{name: "slash-cr", sep: ":/\r\n/", dec: id},
		{name: "slash0-pct", sep: "%3A", dec: r52Dec(stripForPercent), rawSlashIsStructural: true},
		{name: "slash0-css", sep: `\3a`, dec: r52Dec(stripForCSS)},
		{name: "slash0-refs", sep: "&#58;", dec: r52Dec(stripForRefs)},
		{name: "protorel", sep: "//", dec: id, omitScheme: true},
		{name: "protorel3", sep: "///", dec: id, omitScheme: true},
		{name: "protorel-back", sep: `\\`, dec: id, omitScheme: true},
		{name: "protorel-pct", sep: "%2F%2F", dec: r52Dec(stripForPercent), rawSlashIsStructural: true, omitScheme: true},
		{name: "protorel-css", sep: `\2f\2f`, dec: r52Dec(stripForCSS), omitScheme: true},
		{name: "protorel-refs", sep: "&#47;&#47;", dec: r52Dec(stripForRefs), omitScheme: true},
	}
}

// ---------------------------------------------------------------------------
// One cell.
// ---------------------------------------------------------------------------

type r52Cell struct {
	label string
	from  string // declared origin the content names
	to    string // declared origin it must become
	// schemeText is the scheme actually written at the match site, which may
	// differ from `from`'s.
	schemeText string
	hostText   string // the authority as written: [userinfo@]host[:port]
	value      bool   // an attribute value (true) or a text node (false)
	enc        r52Enc
	// pre and post surround the reference, because the locator's entry offset
	// is where a token starts and the needScheme arm splices from it.
	pre, post string
	// user is the userinfo written in hostText, which must survive.
	user string
}

func (c r52Cell) ref() string {
	s := c.schemeText
	if c.enc.omitScheme {
		s = ""
	}
	return s + c.enc.sep + c.hostText + "/x?q=1"
}

func (c r52Cell) input() string { return c.pre + c.ref() + c.post }

func r52Run(t *testing.T, c r52Cell) (in, out string, err string) {
	t.Helper()
	from := origin.MustParse(c.from)
	to := origin.MustParse(c.to)
	m, mErr := origin.NewMatcher([]origin.Pair{{Canonical: from, Variant: to, Name: "s"}})
	if mErr != nil {
		t.Fatalf("%s: %v", c.label, mErr)
	}
	in = c.input()
	out = string(hostsFor(m).rewriteAllRefs([]byte(in), c.value, nil))

	// Only the reference may change. Anything else is damage to a value this
	// pass had no business touching.
	if !strings.HasPrefix(out, c.pre) || !strings.HasSuffix(out, c.post) ||
		len(out) < len(c.pre)+len(c.post) {
		return in, out, "the bytes surrounding the reference were changed"
	}
	inRef := c.ref()
	outRef := out[len(c.pre) : len(out)-len(c.post)]

	// The document is served at the variant, which is `to` in the forward
	// direction and `from` in the reverse one. Both directions are exercised by
	// swapping the pair, so the document scheme is named per cell by the caller
	// through `to` — the locator itself uses to.Scheme, so the oracle does too.
	doc := to.Scheme

	wantOrigin := r52OriginOf(to)
	gotOrigin, gotRest, gotOK := r52Resolve(c.enc.dec(outRef), doc)
	inOrigin, inRest, inOK := r52Resolve(c.enc.dec(inRef), doc)

	// A reference that names no origin, or names one this map does not hold, is
	// not this pass's business: the only correct output is the input.
	if !inOK || inOrigin != r52OriginOf(from) {
		if out != in {
			what := "no origin at all"
			if inOK {
				what = inOrigin
			}
			return in, out, fmt.Sprintf("rewrote a reference that names %s (false positive)", what)
		}
		return in, out, ""
	}

	switch {
	case !gotOK:
		return in, out, "the output resolves to no origin at all (a relative reference)"
	case gotOrigin != wantOrigin:
		return in, out, fmt.Sprintf("resolves to %s, want %s", gotOrigin, wantOrigin)
	case gotRest != inRest:
		return in, out, fmt.Sprintf("path became %q, was %q", gotRest, inRest)
	}
	if c.enc.rawSlashIsStructural && strings.Contains(outRef, "://") {
		return in, out, "a literal `://` was written over an encoded separator"
	}
	// normaliseURLLeak's own contract: "Only the hosts' byte ranges change.
	// Everything else — the scheme as written, the separator however it was
	// spelled, userinfo, port, path, query, fragment … is copied through, so
	// this cannot damage a value it does not need to fix."
	if c.user != "" && !strings.Contains(c.enc.dec(outRef), c.user) {
		return in, out, "the userinfo was deleted"
	}
	return in, out, ""
}

// ---------------------------------------------------------------------------
// The cross product.
// ---------------------------------------------------------------------------

// r52Surrounds is what the reference is embedded in. It matters because the
// locator's entry offset is a token start, and the needScheme arm splices from
// that offset rather than from the host — so what precedes the scheme decides
// whether the splice lands where the caller thinks it does.
var r52Surrounds = []struct{ name, pre, post string }{
	{"bare", "", ""},
	{"quoted", `"`, `"`},
	{"css-url", "url(", ")"},
	{"prose", "See ", " thanks"},
	{"bracket", "]", "["},
	{"srcset", "img.png 1x, ", " 2x"},
	{"leading-space", "  ", ""},
}

func r52Cells() []r52Cell {
	var cells []r52Cell
	// Host spellings: how the canonical is declared, and how the content spells
	// it. The variant is ASCII by construction, so the interesting IDN axis is
	// the reverse direction, where the canonical is the target.
	type hostPair struct {
		name    string
		canon   string // declared canonical origin, scheme filled in below
		written string // how the content spells the canonical host
		user    string // the userinfo inside `written`, which must survive
	}
	hosts := []hostPair{
		{"ascii", "www.acme.fi", "www.acme.fi", ""},
		{"ascii-upper", "www.acme.fi", "WWW.ACME.FI", ""},
		{"ulabel", "www.äcme.fi", "www.äcme.fi", ""},
		{"alabel", "www.xn--cme-pla.fi", "www.xn--cme-pla.fi", ""},
		{"ulabel-decl-alabel-written", "www.äcme.fi", "www.xn--cme-pla.fi", ""},
		{"userinfo", "www.acme.fi", "user:pw@www.acme.fi", "user:pw@"},
		{"userinfo-2at", "www.acme.fi", "u@ser@www.acme.fi", "u@ser@"},
		{"ipv6", "[2001:db8::1]", "[2001:db8::1]", ""},
		{"ipv6-userinfo", "[2001:db8::1]", "user@[2001:db8::1]", "user@"},
		{"rootdot", "www.acme.fi", "www.acme.fi.", ""},
		// UTS46 spellings: the same host to a browser, no bytes in common.
		{"fullwidth", "www.acme.fi", "ｗｗｗ.acme.fi", ""},
		{"softhyphen", "www.acme.fi", "www.ac­me.fi", ""},
		{"ideographic-dot", "www.acme.fi", "www。acme.fi", ""},
		{"pct-in-host", "www.acme.fi", "www.ac%6De.fi", ""},
		{"tab-in-host", "www.acme.fi", "www.ac\tme.fi", ""},
		// A host this map does not hold: nothing may touch it.
		{"other-host", "www.acme.fi", "www.other.fi", ""},
	}
	ports := []struct {
		name     string
		canonSfx string // on the declared canonical
		written  string // on the written authority
		variant  string // on the variant
	}{
		{"noport", "", "", ""},
		{"src-default", "", ":443", ""},
		{"src-port", ":8080", ":8080", ""},
		{"dst-port", "", "", ":8443"},
		{"both-port", ":8080", ":8080", ":8443"},
	}
	schemes := []struct{ name, declared, written, variant string }{
		{"same", "https", "https", "https"},
		{"differs", "http", "http", "https"},
		{"differs-rev", "https", "https", "http"},
		{"upper-same", "https", "HTTPS", "https"},
		{"upper-differs", "http", "HtTp", "https"},
	}
	for _, e := range r52Encodings() {
		for _, h := range hosts {
			for _, p := range ports {
				for _, s := range schemes {
					// A default port only makes sense with the matching scheme.
					if p.name == "src-default" && !strings.EqualFold(s.written, "https") {
						continue
					}
					if h.name == "ipv6" && p.canonSfx != "" {
						continue
					}
					// Without a scheme written, the scheme axis has nothing to
					// vary: the reference resolves against the document.
					if e.omitScheme && s.name != "same" {
						continue
					}
					canon := s.declared + "://" + h.canon + p.canonSfx
					variant := s.variant + "://wt-a--acme.ddev.site" + p.variant
					for _, val := range []bool{true, false} {
						vname := "attr"
						if !val {
							vname = "text"
						}
						for _, sur := range r52Surrounds {
							cells = append(cells, r52Cell{
								label: strings.Join([]string{
									"fwd", e.name, h.name, p.name, s.name, vname, sur.name}, "/"),
								from:       canon,
								to:         variant,
								schemeText: s.written,
								hostText:   h.written + p.written,
								user:       h.user,
								value:      val,
								enc:        e,
								pre:        sur.pre,
								post:       sur.post,
							})
							// And the reverse direction, which is where the
							// declared spelling of an IDN has to come back.
							cells = append(cells, r52Cell{
								label: strings.Join([]string{
									"rev", e.name, h.name, p.name, s.name, vname, sur.name}, "/"),
								from:       variant,
								to:         canon,
								schemeText: s.variant,
								hostText:   "wt-a--acme.ddev.site" + p.variant,
								value:      val,
								enc:        e,
								pre:        sur.pre,
								post:       sur.post,
							})
						}
					}
				}
			}
		}
	}
	return cells
}

// TestR52CrossProduct is the table. It reports every failing cell grouped by
// the shape of the failure, so one restructuring can be judged against the
// whole class rather than against one example.
func TestR52CrossProduct(t *testing.T) {
	cells := r52Cells()
	fails := map[string][]string{}
	pass := 0
	for _, c := range cells {
		in, out, msg := r52Run(t, c)
		if msg == "" {
			pass++
			continue
		}
		key := msg
		// Collapse the varying origin out of the key so the classes group.
		if i := strings.Index(key, " to "); i > 0 && strings.HasPrefix(key, "resolves") {
			key = "resolves to the wrong origin"
		}
		if strings.HasPrefix(key, "path became") {
			key = "the path was corrupted"
		}
		fails[key] = append(fails[key], fmt.Sprintf("%-52s in=%q out=%q", c.label, in, out))
	}
	keys := make([]string, 0, len(fails))
	for k := range fails {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("cells: %d, passing: %d, failing: %d", len(cells), pass, len(cells)-pass)
	for _, k := range keys {
		t.Logf("=== %s (%d cells)", k, len(fails[k]))
		for i, line := range fails[k] {
			if i >= 8 {
				t.Logf("    … and %d more", len(fails[k])-8)
				break
			}
			t.Logf("    %s", line)
		}
	}
	if len(fails) > 0 {
		t.Fail()
	}
}

// TestR52ASchemeRelativePortIsJudgedByAnotherSitesScheme.
//
// `schemeAt` ends with a whole-map guess: with no scheme written at or before
// the candidate, it answers `h.schemeList[0]` — the alphabetically first
// *variant* scheme anywhere in the map. That answer decides which port is the
// default, and §5.4 matches on exact origin equality, so on a map with one http
// variant and one https variant every scheme-relative reference in the whole
// document is judged against "http".
//
// `//www.acme.fi:443/x` on a page served at an https variant is
// `https://www.acme.fi:443` to a browser, which is the canonical origin. Judged
// under http, 443 is not the default, `www.acme.fi:443` is not in the table, and
// the reference is declined — a live production origin in an `href`, which is
// test 28. The byte matcher covers the plain spelling; it does not cover the
// spellings this file exists for, and each of the six below reaches the browser
// naming production.
//
// `authorityStart`'s own comment already records this exact mistake on the
// scheme half — "Guessing from the map was wrong on a mixed-scheme map … so the
// caller is told to confirm it, rather than a whole-map guess being made here."
// The port half kept the guess. The answer is available at the same place the
// scheme answer is: after the lookup, `h.to[host].Scheme` is the scheme *this
// host's* variant is served on, which is the document's.
func TestR52ASchemeRelativePortIsJudgedByAnotherSitesScheme(t *testing.T) {
	mp, err := origin.NewMap([]origin.Site{
		{Name: "a",
			Canonical: origin.MustParse("https://www.acme.fi"),
			Variant:   origin.MustParse("https://wt-a--acme.ddev.site")},
		// A second site whose variant is a plain-HTTP listener. Nothing about
		// this site appears in any fixture below.
		{Name: "b",
			Canonical: origin.MustParse("http://www.beta.fi"),
			Variant:   origin.MustParse("http://wt-a--beta.ddev.site:8080")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{
		`<a href="///www.acme.fi:443/x">p</a>`,
		`<a href="\\www.acme.fi:443/x">p</a>`,
		`<a href="//user@www.acme.fi:443/x">p</a>`,
		`<a href="//www.ac%6De.fi:443/x">p</a>`,
		`<a href="//ｗｗｗ.acme.fi:443/x">p</a>`,
		`<div style="background:url(\2f\2fwww.acme.fi:443/a.png)">x</div>`,
	} {
		out := r52HTML(t, mp.Forward(), in)
		if strings.Contains(out, "acme.fi") || strings.Contains(out, "ac%6De.fi") {
			t.Errorf("a production origin reached the browser:\n  in  %s\n  out %s", in, out)
		}
	}
	// The same map with one site — so schemeList[0] happens to be right — gets
	// every one of them. Nothing about the reference changed; only another
	// site's variant scheme did.
	one, err := origin.NewMap([]origin.Site{{Name: "a",
		Canonical: origin.MustParse("https://www.acme.fi"),
		Variant:   origin.MustParse("https://wt-a--acme.ddev.site")}})
	if err != nil {
		t.Fatal(err)
	}
	in := `<a href="///www.acme.fi:443/x">p</a>`
	if out := r52HTML(t, one.Forward(), in); strings.Contains(out, "acme.fi") {
		t.Fatalf("the one-site map no longer rewrites this, so the contrast above "+
			"is not what this test says it is:\n  out %s", out)
	}
}

// TestR52TheSchemeArmDeletesTheUserinfoItSplicesOver.
//
// `normaliseURLLeak`'s contract is stated in its own doc comment: "Only the
// hosts' byte ranges change. Everything else — the scheme as written, the
// separator however it was spelled, userinfo, port, path, query, fragment … is
// copied through, so this cannot damage a value it does not need to fix."
//
// The `needScheme` arm breaks it. It widens the replaced range from the scheme
// to the end of the authority and writes back `scheme + separator + host`, and
// the userinfo lives between the separator and the host — so it is spliced over
// and never written back. The other two arms start at `n.pos[hs]`, which is
// past the `@`, and keep it. One URL, two spellings, two answers.
//
// The direction that matters is the request one. A canonical declared `http://`
// — an ordinary staging shape, and what fe69d42's own message calls out — makes
// every URL in the document take the differing-scheme arm, so a stored
// `https://u:p@wt-a--acme.ddev.site/x` posted back through the editor is written
// into the *shared production database* as `http://www.acme.fi/x`. The
// credentials are gone, from a database canonical and CI share, with no undo and
// nothing in the census to show it happened.
func TestR52TheSchemeArmDeletesTheUserinfoItSplicesOver(t *testing.T) {
	mp, err := origin.NewMap([]origin.Site{{Name: "main",
		Canonical: origin.MustParse("http://www.acme.fi"),
		Variant:   origin.MustParse("https://wt-a--acme.ddev.site")}})
	if err != nil {
		t.Fatal(err)
	}
	rev := mp.Reverse()

	// The request direction, which is the one that writes.
	const body = `{"feed":"https://u:p@wt-a--acme.ddev.site/x"}`
	nv, _ := rev.Rewrite([]byte(body), SurfaceRequestBody, false)
	back := string(HostLeaksBack(rev, nv))
	if !strings.Contains(back, "www.acme.fi") {
		t.Fatalf("not reversed at all, so this fixture no longer tests the arm:\n  %s", back)
	}
	if !strings.Contains(back, "u:p@") {
		t.Errorf("the credentials were deleted on the way into the shared database:\n"+
			"  in   %s\n  out  %s", body, back)
	}

	// And the forward direction, where the same deletion breaks the link the
	// preview serves — while the matching-scheme spelling keeps it.
	fwd := mp.Forward()
	const page = `<a href="http://u:p@www.acme.fi/x">p</a>`
	if out := r52HTML(t, fwd, page); !strings.Contains(out, "u:p@") {
		t.Errorf("the credentials were deleted before the browser saw them:\n"+
			"  in   %s\n  out  %s", page, out)
	}
}

// TestR52AnIPv6LiteralBehindUserinfoIsNotAHostAtAll.
//
// `hostRange` settles a bracketed literal first, "within a fixed window", and
// gates that on `b[at] == '['` — `at` being the *authority* start. When the
// authority has userinfo, `at` is the userinfo, the general branch runs instead,
// and that branch treats `[` as a boundary, so the scan stops at the bracket and
// the host comes out empty. The function's own comment says why the bracket
// branch needs no userinfo search — "userinfo would precede the bracket" — which
// is exactly the case it then cannot read.
//
// Both directions are affected, and the reverse one is the §4.3 failure:
// `http://[::1]:8080` is the plain-HTTP-listener variant `maps_test.go` already
// enumerates, and a request body naming it with credentials goes upstream
// unreversed, putting the variant hostname in the shared database.
func TestR52AnIPv6LiteralBehindUserinfoIsNotAHostAtAll(t *testing.T) {
	m, err := origin.NewMatcher([]origin.Pair{{Name: "s",
		Canonical: origin.MustParse("http://[::1]:8080"),
		Variant:   origin.MustParse("https://www.acme.fi")}})
	if err != nil {
		t.Fatal(err)
	}
	const withUser = `http://u@[::1]:8080/x`
	const without = `http://[::1]:8080/x`
	if out := string(HostLeaksBack(m, []byte(without))); strings.Contains(out, "::1") {
		t.Fatalf("the literal is not read even without userinfo, so this fixture "+
			"no longer isolates the userinfo:\n  %s", out)
	}
	if out := string(HostLeaksBack(m, []byte(withUser))); strings.Contains(out, "::1") {
		t.Errorf("an IPv6 authority behind userinfo is not located, so the variant "+
			"hostname goes upstream:\n  in  %s\n  out %s", withUser, out)
	}
}

// r52HTML runs the response pipeline the proxy runs, so a finding is stated
// against what a browser receives rather than against an internal helper.
func r52HTML(t *testing.T, m *origin.Matcher, in string) string {
	t.Helper()
	out, err := io.ReadAll(NewResponseBody(bytes.NewReader([]byte(in)), m, nil, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
