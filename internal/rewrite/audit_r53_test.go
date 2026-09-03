package rewrite

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 53. The cross product over the serialized repair, and what it found
// one level below itself.
//
// PLAN §5.2 records that enumerating this surface's spellings "does not
// terminate": five encoders give twenty-five ordered pairs, ten named spellings
// read twenty of them, and every round that closed one cell closed one cell. So
// this enumerates the product — encoder stack x transport x value type x
// nesting depth x content bytes x surrounding context x direction — and asserts
// against a model of what PHP's `unserialize` accepts rather than against what
// the walk emits. A declared length that does not describe its data is the
// failure, and that is a fact about the bytes.
//
// The product itself is clean: no cell writes a length it cannot justify, and
// the arm-pair sweep — out through one surface, back through another, which is
// the only shape where a symmetric decline stops self-healing — comes home in
// every cell.
//
// What it found instead is one level down, and it is the mirror of test 28: a
// `\uXXXX` escape is read as a host boundary. `wp_json_encode` writes every
// non-ASCII character that way, so an ordinary non-breaking space after the
// site's own URL makes the origin match and be rewritten to the variant. The
// browser's JSON parser then decodes the escape, and the byte after the host is
// a letter, so the *reverse* direction cannot match it: the variant hostname
// goes upstream into the shared production database and stays there.
//
// The rewrite should not have fired at all. ada — the parser Chrome ships, and
// the authority testdata/url-shapes.tsv.gz is generated from — resolves
// `https://www.acme.fi<U+00E4>x` to `www.acme.xn--fix-rla` and
// `https://www.acme.fi<U+00A0>x` to a parse error. Neither is the canonical
// origin, and oracle_test.go's own contract is then "hostshift must not touch
// it". The corpus contains no row with an escape immediately after the host,
// which is why 253,680 cells of round 52 and the whole oracle are green on it.

// ---------------------------------------------------------------------------
// The oracle: PHP's unserialize, as a grammar.
// ---------------------------------------------------------------------------

// r53Parse parses one serialized value at i and returns the offset just past
// it. It is `unserialize`'s acceptance, reduced to the shapes WordPress writes:
// null, bool, int, float, string, array, object, enum and the custom
// (Serializable) form.
//
// The one rule this whole file exists for: `s:N:"` counts N *bytes* of data and
// must be followed by exactly N of them, then `";`. PHP does not search for the
// close — a stale N lands mid-data and the value is refused outright.
func r53Parse(b []byte, i, depth int) (int, bool) {
	if depth > 64 || i >= len(b) {
		return 0, false
	}
	switch b[i] {
	case 'N':
		if i+1 < len(b) && b[i+1] == ';' {
			return i + 2, true
		}
		return 0, false
	case 'b':
		if i+3 < len(b) && b[i+1] == ':' && (b[i+2] == '0' || b[i+2] == '1') && b[i+3] == ';' {
			return i + 4, true
		}
		return 0, false
	case 'i', 'R', 'r':
		return r53Scalar(b, i, "0123456789+-")
	case 'd':
		return r53Scalar(b, i, "0123456789+-.eEINFAN")
	case 's', 'E':
		return r53ParseString(b, i)
	case 'a':
		n, j, ok := r53Len(b, i+1)
		if !ok || n < 0 || j >= len(b) || b[j] != '{' {
			return 0, false
		}
		j++
		for k := 0; k < n; k++ {
			// A key is an int or a string; PHP accepts nothing else.
			if j >= len(b) || (b[j] != 'i' && b[j] != 's') {
				return 0, false
			}
			e, ok := r53Parse(b, j, depth+1)
			if !ok {
				return 0, false
			}
			if e, ok = r53Parse(b, e, depth+1); !ok {
				return 0, false
			}
			j = e
		}
		if j >= len(b) || b[j] != '}' {
			return 0, false
		}
		return j + 1, true
	case 'O':
		// O:<namelen>:"<name>":<count>:{ <key><value> ... }
		j, ok := r53ClassHeader(b, i)
		if !ok {
			return 0, false
		}
		n, j2, ok := r53Len(b, j)
		if !ok || n < 0 || j2 >= len(b) || b[j2] != '{' {
			return 0, false
		}
		j = j2 + 1
		for k := 0; k < n; k++ {
			e, ok := r53Parse(b, j, depth+1)
			if !ok {
				return 0, false
			}
			if e, ok = r53Parse(b, e, depth+1); !ok {
				return 0, false
			}
			j = e
		}
		if j >= len(b) || b[j] != '}' {
			return 0, false
		}
		return j + 1, true
	case 'C':
		// C:<namelen>:"<name>":<datalen>:{<datalen bytes>}
		j, ok := r53ClassHeader(b, i)
		if !ok {
			return 0, false
		}
		n, j2, ok := r53Len(b, j)
		if !ok || n < 0 || j2 >= len(b) || b[j2] != '{' {
			return 0, false
		}
		j = j2 + 1
		if j+n+1 > len(b) || b[j+n] != '}' {
			return 0, false
		}
		return j + n + 1, true
	}
	return 0, false
}

// r53ClassHeader reads `X:<len>:"<len bytes>"` at i and returns the offset of
// the colon that follows it.
func r53ClassHeader(b []byte, i int) (int, bool) {
	nl, j, ok := r53Len(b, i+1)
	if !ok || nl < 0 || j >= len(b) || b[j] != '"' {
		return 0, false
	}
	j++
	if j+nl+2 > len(b) || b[j+nl] != '"' || b[j+nl+1] != ':' {
		return 0, false
	}
	return j + nl + 1, true
}

// r53ParseString is `s:N:"<N bytes>";` and `E:N:"<N bytes>";`.
func r53ParseString(b []byte, i int) (int, bool) {
	n, j, ok := r53Len(b, i+1)
	if !ok || n < 0 || j >= len(b) || b[j] != '"' {
		return 0, false
	}
	j++
	if j+n+2 > len(b) || b[j+n] != '"' || b[j+n+1] != ';' {
		return 0, false
	}
	return j + n + 2, true
}

// r53Len reads `:<digits>:` at i, where i points at the colon after the type
// letter, and returns the number and the offset past the second colon.
func r53Len(b []byte, i int) (int, int, bool) {
	if i >= len(b) || b[i] != ':' {
		return 0, 0, false
	}
	j := i + 1
	for j < len(b) && b[j] >= '0' && b[j] <= '9' {
		j++
	}
	if j == i+1 || j >= len(b) || b[j] != ':' {
		return 0, 0, false
	}
	n, err := strconv.Atoi(string(b[i+1 : j]))
	if err != nil {
		return 0, 0, false
	}
	return n, j + 1, true
}

// r53Scalar is `X:<chars>;`.
func r53Scalar(b []byte, i int, set string) (int, bool) {
	j := i + 1
	if j >= len(b) || b[j] != ':' {
		return 0, false
	}
	j++
	start := j
	for j < len(b) && strings.ContainsRune(set, rune(b[j])) {
		j++
	}
	if j == start || j >= len(b) || b[j] != ';' {
		return 0, false
	}
	return j + 1, true
}

// r53Unserialize is `unserialize($s) !== false`: one complete value at offset
// zero. PHP ignores whatever follows it.
func r53Unserialize(b []byte) bool {
	_, ok := r53Parse(b, 0, 0)
	return ok
}

// TestTheUnserializeModelMatchesPHP keeps the oracle honest against the real
// thing, on the boundary cases the sweep turns on.
func TestTheUnserializeModelMatchesPHP(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("no php on this machine")
	}
	cases := []string{
		`s:3:"abc";`, `s:4:"abc";`, `s:2:"abc";`, `s:3:"ab";`,
		`a:1:{s:3:"url";s:5:"12345";}`, `a:1:{s:3:"url";s:6:"12345";}`,
		`a:2:{i:0;i:42;i:1;d:1.0E+17;}`, `a:1:{i:0;b:1;}`, `N;`, `a:1:{i:0;N;}`,
		`O:8:"stdClass":1:{s:3:"url";s:1:"x";}`,
		`O:8:"stdClass":1:{s:3:"url";s:2:"x";}`,
		`a:1:{s:4:"data";s:26:"a:1:{s:3:"url";s:1:"x";}";}`,
		`a:1:{s:4:"data";s:24:"a:1:{s:3:"url";s:1:"x";}";}`,
		`a:5:{i:0;i:42;i:1;d:1.0E+17;i:2;b:1;i:3;N;i:4;s:1:"x";}`,
		`d:-1.5;`, `i:-3;`, `b:2;`, `a:1:{d:1.0;s:1:"x";}`, `a:0:{}`,
		`a:1:{i:0;a:1:{i:0;s:2:"hi";}}`, `s:5:"a"b"c";`,
		`s:3:"abc"; trailing prose`, `a:2:{s:3:"url";s:1:"x";}`,
		`a:1:{s:3:"url";s:1:"x";s:1:"y";i:1;}`,
	}
	for _, s := range cases {
		// `false` is a legitimate value too, so the sentinel has to be
		// something unserialize cannot return: it reports the error instead.
		script := `$s = file_get_contents("php://stdin"); $v = @unserialize($s);` +
			` echo ($v === false && substr($s, 0, 4) !== "b:0;") ? "no" : "yes";`
		cmd := exec.Command(php, "-r", script)
		cmd.Stdin = strings.NewReader(s)
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("php on %q: %v", s, err)
		}
		want := string(out) == "yes"
		if got := r53Unserialize([]byte(s)); got != want {
			t.Errorf("model disagrees with PHP on %q: model=%v php=%v", s, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// The encoders, and their inverses.
// ---------------------------------------------------------------------------

type r53Coder struct {
	name string
	enc  func(string) string
	dec  func(string) (string, bool)
}

func r53Pct(s string) string {
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

func r53PctDec(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '%' {
			if i+2 >= len(s) {
				return "", false
			}
			v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return "", false
			}
			b.WriteByte(byte(v))
			i += 3
			continue
		}
		if s[i] == '+' {
			b.WriteByte(' ')
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String(), true
}

// r53JSONEsc is json_encode's string body, without the quotes.
func r53JSONEsc(s string) string {
	q := phpJSONEncode(s)
	return q[1 : len(q)-1]
}

// r53JSONHexEsc is json_encode with JSON_HEX_QUOT|JSON_HEX_APOS|JSON_HEX_AMP|
// JSON_HEX_TAG, which is what WordPress core's Interactivity API writes so a
// `data-wp-context` attribute can be single-quoted.
func r53JSONHexEsc(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"', '\'', '&', '<', '>':
			b.WriteString(r53U(r))
		default:
			b.WriteString(r53JSONEsc(string(r)))
		}
	}
	return b.String()
}

// r53Backslash is one backslash and r53U is json_encode's `\uXXXX`. Both are
// built rather than written, so no literal escape appears in this file's source
// where a reader would have to count backslashes to know what is meant.
func r53Backslash() string { return string([]byte{'\\'}) }

func r53U(r rune) string { return r53Backslash() + fmt.Sprintf("u%04X", r) }

func r53JSONDec(s string) (string, bool) {
	var out string
	if err := json.Unmarshal([]byte(`"`+s+`"`), &out); err != nil {
		return "", false
	}
	return out, true
}

// r53AttrDec decodes the references esc_attr writes, and the numeric spellings
// of the quote a template engine may write instead.
func r53AttrDec(s string) (string, bool) {
	rep := strings.NewReplacer(
		"&quot;", `"`, "&#34;", `"`, "&#034;", `"`, "&#x22;", `"`, "&#X22;", `"`, "&#x022;", `"`,
		"&#039;", "'", "&#39;", "'", "&apos;", "'",
		"&lt;", "<", "&gt;", ">", "&amp;", "&",
	)
	return rep.Replace(s), true
}

var (
	r53ID   = r53Coder{"id", func(s string) string { return s }, func(s string) (string, bool) { return s, true }}
	r53Attr = r53Coder{"attr", escAttrNoDouble, r53AttrDec}
	r53JSON = r53Coder{"json", r53JSONEsc, r53JSONDec}
	r53Hex  = r53Coder{"jsonhex", r53JSONHexEsc, r53JSONDec}
	r53Enc  = r53Coder{"pct", r53Pct, r53PctDec}
)

var r53Coders = []r53Coder{r53ID, r53Attr, r53JSON, r53Hex, r53Enc}

// ---------------------------------------------------------------------------
// The values.
// ---------------------------------------------------------------------------

func r53Str(v string) string { return "s:" + strconv.Itoa(len(v)) + `:"` + v + `";` }

// r53Value builds a serialized value of the named shape carrying host, with
// `extra` bytes of ordinary content beside the URL.
func r53Value(kind, host, extra string, depth int) string {
	u := host + "/sv/sida" + extra
	var v string
	switch kind {
	case "string":
		v = r53Str(u)
	case "array":
		v = "a:2:{" + r53Str("home") + r53Str(u) + r53Str("n") + "i:7;}"
	case "object":
		v = `O:8:"stdClass":1:{` + r53Str("url") + r53Str(u) + "}"
	case "mixed":
		v = "a:5:{i:0;i:42;i:1;d:1.0E+17;i:2;b:1;i:3;N;i:4;" + r53Str(u) + "}"
	case "nested":
		inner := "a:1:{" + r53Str("url") + r53Str(u) + "}"
		v = "a:1:{" + r53Str("data") + r53Str(inner) + "}"
	default:
		panic(kind)
	}
	for d := 1; d < depth; d++ {
		v = "a:1:{i:0;" + v + "}"
	}
	return v
}

var r53Kinds = []string{"string", "array", "object", "mixed", "nested"}

// The content beside the URL. Every one of these is a byte some layer of the
// stack spells differently from itself, which is where a length is miscounted.
var r53Extras = []struct{ name, text string }{
	{"plain", ""},
	{"amp", " & Co"},
	{"quote", ` "x" `},
	{"nonascii", " Läs mer"},
	// An `&amp;` already in the data: esc_attr runs with $double_encode=false,
	// so it passes through as five literal bytes counting five, while a bare `&`
	// becomes `&amp;` counting one. PLAN records that as the ambiguity that
	// served "Snellman & Co" broken.
	{"literalref", " &amp; Co"},
	// A false close: `";` is what the end of a string looks like, so a stale
	// length landing here reads as a perfect header.
	{"falseclose", ` a";b `},
	// The bytes a urlencoder touches and a JSON escaper does not, and the
	// reverse.
	{"plus", " a+b "},
	{"pctlike", " %3A%2F "},
	{"markup", " <b>x</b> "},
}

var r53Contexts = []struct{ name, pre, post string }{
	{"none", "", ""},
	{"label", "opt: ", ""},
	{"glued", "Atgard", ""},
	{"indent", "\n  ", "\n"},
	{"trailing", "", " (cachad)"},
}

// ---------------------------------------------------------------------------
// The cells.
// ---------------------------------------------------------------------------

type r53Cell struct {
	transport string
	outer     r53Coder
	inner     r53Coder
	kind      string
	depth     int
	extra     string
	ctx       int
	reverse   bool
}

func (c r53Cell) extraName() string {
	for _, e := range r53Extras {
		if e.text == c.extra {
			return e.name
		}
	}
	return "?"
}

func (c r53Cell) label() string {
	dir := "fwd"
	if c.reverse {
		dir = "rev"
	}
	return fmt.Sprintf("%s/%s+%s/%s/d%d/%s/%s/%s", c.transport, c.outer.name, c.inner.name,
		c.kind, c.depth, c.extraName(), r53Contexts[c.ctx].name, dir)
}

func (c r53Cell) combo() string {
	return fmt.Sprintf("%s/%s+%s", c.transport, c.outer.name, c.inner.name)
}

// ambiguous reports a cell whose encoding has no inverse, so no expectation can
// be stated. `esc_attr` runs with $double_encode = false, so a literal `&amp;`
// in the data and a bare `&` in the data are the same five bytes in the
// attribute, and the serialized length counts five for one and one for the
// other. PLAN settles that by offering both readings and letting the closing
// delimiter pick; a test that decodes cannot, and asserting either reading here
// would assert the coin flip. TestAnAmpersandUnderEscAttr covers it directly.
func (c r53Cell) ambiguous() bool {
	if !strings.Contains(c.extra, "&amp;") {
		return false
	}
	return c.outer.name == "attr" || c.inner.name == "attr"
}

const (
	r53Canon   = "https://www.acme.fi"
	r53Variant = "https://wt-a--acme.ddev.site"
)

func r53Matcher(t *testing.T, reverse bool) *origin.Matcher {
	t.Helper()
	canon, variant := origin.MustParse(r53Canon), origin.MustParse(r53Variant)
	if reverse {
		canon, variant = variant, canon
	}
	m, err := origin.NewMatcher([]origin.Pair{{Canonical: canon, Variant: variant, Name: "acme"}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func r53Host(o string) string {
	return strings.TrimPrefix(strings.TrimPrefix(o, "https://"), "http://")
}

// r53Build assembles the buffer the arm is handed.
//
// The container encodes the whole field, context and all: a urlencoded form body
// percent-encodes the newline in front of a textarea's blob, and a JSON string
// escapes it. Putting the context outside the container instead builds a body no
// client sends.
func (c r53Cell) build(payload string) string {
	ctx := r53Contexts[c.ctx]
	field := c.outer.enc(ctx.pre + c.inner.enc(payload) + ctx.post)
	switch c.transport {
	case "raw":
		return field
	case "form":
		return "option_page=general&opt=" + field + "&_wpnonce=abc123"
	case "json":
		return `{"opt":"` + field + `"}`
	case "html":
		return `<input name="opt" value="` + field + `">`
	}
	panic(c.transport)
}

// r53Extract undoes build: it recovers the serialized bytes from the arm's
// output, through the same stack, so what is asserted is what PHP would see.
func (c r53Cell) extract(out string) (string, bool) {
	ctx := r53Contexts[c.ctx]
	var field string
	switch c.transport {
	case "raw":
		field = out
	case "form":
		s, ok := strings.CutPrefix(out, "option_page=general&opt=")
		if !ok {
			return "", false
		}
		if s, ok = strings.CutSuffix(s, "&_wpnonce=abc123"); !ok {
			return "", false
		}
		field = s
	case "json":
		var m map[string]string
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			return "", false
		}
		v, ok := m["opt"]
		if !ok {
			return "", false
		}
		field = v
	case "html":
		s, ok := strings.CutPrefix(out, `<input name="opt" value="`)
		if !ok {
			return "", false
		}
		if s, ok = strings.CutSuffix(s, `">`); !ok {
			return "", false
		}
		field = s
	}
	// The container's own layer is the outer coder, and for JSON encoding/json
	// has already removed it.
	inner := field
	if c.transport != "json" {
		d, ok := c.outer.dec(field)
		if !ok {
			return "", false
		}
		inner = d
	}
	s, ok := strings.CutPrefix(inner, ctx.pre)
	if !ok {
		return "", false
	}
	if s, ok = strings.CutSuffix(s, ctx.post); !ok {
		return "", false
	}
	return c.inner.dec(s)
}

func r53RW(m *origin.Matcher) func([]byte) []byte {
	return func(b []byte) []byte {
		nv, _ := m.Rewrite(b, SurfaceRequestBody, false)
		return HostLeaksBack(m, nv)
	}
}

// r53Apply runs the arm this cell names.
func (c r53Cell) apply(t *testing.T, in string, m *origin.Matcher) string {
	t.Helper()
	switch c.transport {
	case "raw":
		return string(RepairSerialized([]byte(in), r53RW(m)))
	case "form":
		return string(RepairSerializedFields([]byte(in), r53RW(m)))
	case "json":
		return string(RewriteJSON([]byte(in), m, NewStats(false), quiet(), false))
	case "html":
		out, err := io.ReadAll(NewResponseBody(strings.NewReader(in), m, nil,
			Options{Stats: NewStats(false), Log: quiet()}))
		if err != nil {
			t.Fatalf("%s: %v", c.label(), err)
		}
		return string(out)
	}
	panic(c.transport)
}

// r53Transports pairs each arm with the outer encodings its container really
// imposes. Two layers is the documented bound for the matching walk, so the
// container counts as one of them: a urlencoded form body is percent over
// whatever the value already was, and an attribute is esc_attr over it.
//
// The multipart arm is not a separate cell: internal/proxy/multipart.go runs
// RepairSerialized over the raw part body, which is raw/id+X exactly.
var r53Transports = []struct {
	name  string
	outer []r53Coder
}{
	{"raw", r53Coders},
	{"form", []r53Coder{r53Enc}},
	{"json", []r53Coder{r53JSON, r53Hex}},
	{"html", []r53Coder{r53Attr}},
}

type r53Fail struct{ class, combo, label, detail string }

// r53Sweep walks the whole product and returns every failing cell.
//
// Two assertions, and the first is the contract this file's subject states
// outright:
//
//   - **parses, or comes home unchanged.** A repaired value must be one PHP
//     accepts. A *declined* one need not be — a decline re-emits no length and
//     PLAN records that cost — but only because the other direction declines
//     identically and unwinds it. A cell that neither parses nor round-trips is
//     a length written into the shared database over data that does not fit it.
//   - **no production origin survives**, which is test 28.
//
// The round trip is a question about the value, not about the container's
// spelling: RewriteJSON re-quotes a string it changed, so `\/` comes back as
// `/`. That is the same string to every JSON parser and to PHP, and the identity
// map — test 24, asserted separately below — is what pins the untouched case.
func r53Sweep(t *testing.T) (fails []r53Fail, cells int) {
	t.Helper()
	for _, tr := range r53Transports {
		for _, outer := range tr.outer {
			for _, inner := range r53Coders {
				for _, kind := range r53Kinds {
					for depth := 1; depth <= 3; depth++ {
						for _, extra := range r53Extras {
							for ci := range r53Contexts {
								for _, rev := range []bool{false, true} {
									c := r53Cell{tr.name, outer, inner, kind, depth, extra.text, ci, rev}
									if c.ambiguous() {
										continue
									}
									cells++
									fails = append(fails, c.check(t)...)
								}
							}
						}
					}
				}
			}
		}
	}
	return fails, cells
}

func (c r53Cell) check(t *testing.T) (fails []r53Fail) {
	t.Helper()
	from, to := r53Canon, r53Variant
	if c.reverse {
		from, to = to, from
	}
	_ = to
	in := c.build(r53Value(c.kind, from, c.extra, c.depth))
	out := c.apply(t, in, r53Matcher(t, c.reverse))
	home := c.apply(t, out, r53Matcher(t, !c.reverse))

	add := func(class, detail string) {
		fails = append(fails, r53Fail{class, c.combo(), c.label(), detail})
	}

	dec, ok := c.extract(out)
	if !ok {
		add("unreadable", "the field could not be recovered:\n  in  "+in+"\n  out "+out)
		return
	}
	decIn, okIn := c.extract(in)
	decHome, okHome := c.extract(home)
	homeSame := okIn && okHome && decIn == decHome
	parses := r53Unserialize([]byte(dec))
	switch {
	case !parses && !homeSame:
		add("corrupt", "unserialize() refuses the output, and it does not come home:\n"+
			"  in   "+in+"\n  out  "+out+"\n  dec  "+dec+"\n  home "+home)
	case !parses:
		add("declined", "unserialize() refuses the output (it round-trips):\n"+
			"  in   "+in+"\n  out  "+out+"\n  dec  "+dec)
	case !homeSame:
		add("asymmetric", "it parses but does not come home:\n"+
			"  in   "+in+"\n  out  "+out+"\n  home "+home)
	case home != in:
		add("respelled", "it comes home as the same value, spelled differently:\n"+
			"  in   "+in+"\n  home "+home)
	}
	if strings.Contains(dec, r53Host(from)) {
		add("leak", "the origin survived the rewrite:\n  in  "+in+"\n  dec "+dec)
	}
	return
}

func TestSerializedRepairAgainstUnserialize(t *testing.T) {
	fails, cells := r53Sweep(t)
	byClass := map[string]int{}
	byCombo := map[string]map[string]int{}
	byCtx := map[string]map[string]int{}
	first := map[string]r53Fail{}
	for _, f := range fails {
		byClass[f.class]++
		if byCombo[f.class] == nil {
			byCombo[f.class] = map[string]int{}
			byCtx[f.class] = map[string]int{}
		}
		byCombo[f.class][f.combo]++
		byCtx[f.class][strings.Split(f.label, "/")[5]]++
		if _, ok := first[f.class]; !ok {
			first[f.class] = f
		}
	}
	keys := make([]string, 0, len(byClass))
	for k := range byClass {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("%d cells", cells)
	for _, k := range keys {
		f := first[k]
		combos := make([]string, 0, len(byCombo[k]))
		for c, n := range byCombo[k] {
			combos = append(combos, fmt.Sprintf("%s=%d", c, n))
		}
		sort.Strings(combos)
		ctxs := make([]string, 0, len(byCtx[k]))
		for c, n := range byCtx[k] {
			ctxs = append(ctxs, fmt.Sprintf("%s=%d", c, n))
		}
		sort.Strings(ctxs)
		msg := fmt.Sprintf("%s: %d/%d cells\n  combos: %s\n  contexts: %s\n  first: %s\n  %s",
			k, byClass[k], cells, strings.Join(combos, " "), strings.Join(ctxs, " "),
			f.label, f.detail)
		switch k {
		case "corrupt", "asymmetric", "unreadable":
			// The classes that are not a documented cost. A value that neither
			// parses nor comes home is a length written over data that does not
			// fit it, into the shared database, with no undo.
			t.Errorf("%s", msg)
		default:
			// `declined` is PLAN's stated cost of a spelling the walk cannot
			// read — every flush-context one composes JSON_HEX_* with something
			// else, which is exactly the residue §5.2 names — or of a value
			// that does not occupy its whole field. `leak` is the
			// double-percent composition nothing reads. `respelled` is
			// RewriteJSON re-quoting a string it changed. All three round-trip,
			// so they are logged with counts rather than failed, and a change
			// in their size is visible.
			t.Logf("%s", msg)
		}
	}
}

// The identity map is test 24: every byte out equals every byte in.
func TestSerializedRepairIsByteIdenticalUnderIdentity(t *testing.T) {
	id, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse(r53Canon), Variant: origin.MustParse(r53Canon), Name: "acme"}})
	if err != nil {
		t.Fatal(err)
	}
	n, bad := 0, 0
	for _, tr := range r53Transports {
		for _, outer := range tr.outer {
			for _, inner := range r53Coders {
				for _, kind := range r53Kinds {
					for depth := 1; depth <= 3; depth++ {
						for _, extra := range r53Extras {
							for ci := range r53Contexts {
								c := r53Cell{tr.name, outer, inner, kind, depth, extra.text, ci, false}
								in := c.build(r53Value(kind, r53Canon, extra.text, depth))
								n++
								if out := c.apply(t, in, id); out != in {
									bad++
									if bad < 4 {
										t.Errorf("%s: identity changed bytes\n  in  %s\n  out %s",
											c.label(), in, out)
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d/%d cells changed under an identity map", bad, n)
	}
	t.Logf("%d cells", n)
}

// ---------------------------------------------------------------------------
// The cross-arm sweep: out through one surface, back through another.
// ---------------------------------------------------------------------------

// A decline is only recoverable because "the other direction declines
// identically and unwinds it". But the two directions are not the same arm: an
// option leaves through `esc_attr` in a streamed `<input>` and comes back
// percent-encoded through the form splitter; a block attribute leaves inside a
// `wp_localize_script` line and comes back in a JSON request body. Where those
// two arms disagree about whether to repair, the length one broke the other does
// not restore, and the write lands in the shared database with no undo.
//
// So this asserts the only property that matters end to end: what leaves the
// database comes back to it unchanged.

type r53Arm struct {
	name string
	// out wraps the field for the surface, runs the response pipeline and
	// returns the field as the browser reads it back out.
	out func(t *testing.T, field string, m *origin.Matcher) string
	// in wraps what the browser posts and runs the request pipeline.
	in func(t *testing.T, field string, m *origin.Matcher) string
}

func r53HTML(t *testing.T, page string, m *origin.Matcher) string {
	t.Helper()
	out, err := io.ReadAll(NewResponseBody(strings.NewReader(page), m, nil,
		Options{Stats: NewStats(false), Log: quiet()}))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func r53Between(t *testing.T, s, pre, post string) string {
	t.Helper()
	v, ok := strings.CutPrefix(s, pre)
	if !ok {
		t.Fatalf("prefix %q missing from %q", pre, s)
	}
	if v, ok = strings.CutSuffix(v, post); !ok {
		t.Fatalf("suffix %q missing from %q", post, s)
	}
	return v
}

func r53JSONField(t *testing.T, s string) string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not JSON: %q: %v", s, err)
	}
	return m["opt"]
}

func r53FormRoundTrip(t *testing.T, f string, m *origin.Matcher) string {
	t.Helper()
	b := string(RepairSerializedFields(
		[]byte("option_page=general&opt="+r53Pct(f)+"&_wpnonce=a"), r53RW(m)))
	d, _ := r53PctDec(r53Between(t, b, "option_page=general&opt=", "&_wpnonce=a"))
	return d
}

func r53JSONBody(t *testing.T, f string, m *origin.Matcher) string {
	t.Helper()
	return r53JSONField(t, string(RewriteJSON([]byte(`{"opt":`+phpJSONEncode(f)+`}`), m,
		NewStats(false), quiet(), false)))
}

var r53Arms = []r53Arm{
	{
		// options.php: esc_attr into an input value; the browser posts the form.
		name: "attr",
		out: func(t *testing.T, f string, m *origin.Matcher) string {
			o := r53HTML(t, `<input name="opt" value="`+escAttrNoDouble(f)+`">`, m)
			d, _ := r53AttrDec(r53Between(t, o, `<input name="opt" value="`, `">`))
			return d
		},
		in: r53FormRoundTrip,
	},
	{
		// A settings export textarea: esc_textarea into a text node.
		name: "textarea",
		out: func(t *testing.T, f string, m *origin.Matcher) string {
			o := r53HTML(t, `<textarea name="opt">`+escAttrNoDouble(f)+`</textarea>`, m)
			d, _ := r53AttrDec(r53Between(t, o, `<textarea name="opt">`, `</textarea>`))
			return d
		},
		in: r53FormRoundTrip,
	},
	{
		// wp_localize_script: the option inside a JSON object in an inline
		// script; the block editor posts it back as a JSON request body.
		name: "script",
		out: func(t *testing.T, f string, m *origin.Matcher) string {
			o := r53HTML(t, `<script>var w = {"opt":`+phpJSONEncode(f)+`};</script>`, m)
			return r53JSONField(t, r53Between(t, o, `<script>var w = `, `;</script>`))
		},
		in: r53JSONBody,
	},
	{
		// The REST API: application/json out, application/json back.
		name: "rest",
		out:  r53JSONBody,
		in:   r53JSONBody,
	},
	{
		// text/plain out (async-upload.php), a multipart part back — the arm a
		// media-library or Gravity Forms POST uses, which is RepairSerialized on
		// the raw part body.
		name: "plain",
		out: func(t *testing.T, f string, m *origin.Matcher) string {
			return string(RepairSerialized([]byte(f), r53RW(m)))
		},
		in: func(t *testing.T, f string, m *origin.Matcher) string {
			return string(RepairSerialized([]byte(f), r53RW(m)))
		},
	},
}

func TestTheOptionComesHomeThroughEveryArmPair(t *testing.T) {
	var fails []string
	cells := 0
	byCombo := map[string]int{}

	for _, resp := range r53Arms {
		for _, req := range r53Arms {
			escapes := resp.name == "attr" || resp.name == "textarea" ||
				req.name == "attr" || req.name == "textarea"
			for _, kind := range r53Kinds {
				for depth := 1; depth <= 3; depth++ {
					for _, extra := range r53Extras {
						// esc_attr with $double_encode = false has no inverse
						// where the data already holds `&amp;`; see
						// r53Cell.ambiguous.
						if escapes && strings.Contains(extra.text, "&amp;") {
							continue
						}
						for _, ctx := range r53Contexts {
							cells++
							db := ctx.pre + r53Value(kind, r53Canon, extra.text, depth) + ctx.post
							served := resp.out(t, db, r53Matcher(t, false))
							home := req.in(t, served, r53Matcher(t, true))
							combo := resp.name + "->" + req.name
							if home != db {
								byCombo[combo]++
								if len(fails) < 12 {
									fails = append(fails, fmt.Sprintf(
										"%s %s/d%d/%s/%s\n  db     %q\n  served %q\n  home   %q",
										combo, kind, depth, extra.name, ctx.name, db, served, home))
								}
							}
						}
					}
				}
			}
		}
	}
	t.Logf("%d cells", cells)
	if len(byCombo) == 0 {
		return
	}
	n := 0
	combos := make([]string, 0, len(byCombo))
	for c, k := range byCombo {
		combos = append(combos, fmt.Sprintf("%s=%d", c, k))
		n += k
	}
	sort.Strings(combos)
	t.Errorf("%d/%d option values do not come home\n  combos: %s", n, cells, strings.Join(combos, " "))
	for _, f := range fails {
		t.Errorf("%s", f)
	}
}

// ---------------------------------------------------------------------------
// What the sweep found, one level below itself.
// ---------------------------------------------------------------------------

// r53Escaped is a URL naming the canonical host, followed by one character,
// inside a JSON string — so the character reaches the matcher as `\uXXXX`, which
// is how `wp_json_encode` writes every non-ASCII rune.
func r53Escaped(r rune) string {
	return `{"u":` + phpJSONEncode("https://www.acme.fi"+string(r)+"x") + `}`
}

// A `\uXXXX` escape is not a host boundary.
//
// The matcher reads the backslash as one — which is right for a *literal*
// backslash, since the WHATWG parser treats `\` like `/` in a special URL, and
// several tests pin that. In a JSON or JavaScript string the backslash is never
// literal: it introduces an escape whose decoded character usually continues the
// host. ada, asked directly:
//
//	https://www.acme.fi<U+00A0>x   parse error — not a URL at all
//	https://www.acme.fi<U+00E4>x   www.acme.xn--fix-rla
//	https://www.acme.fi<U+2013>x   www.acme.xn--fix-3n0a
//	https://www.acme.fi<U+2026>x   parse error
//	https://www.acme.fi<U+00BB>x   www.acme.xn--fix-2ga
//	https://www.acme.fi<U+200B>x   www.acme.fix
//
// Not one is the canonical origin, so oracle_test.go's own contract — "the
// browser resolves it anywhere else ⇒ hostshift must not touch it" — forbids the
// rewrite. testdata/url-shapes.tsv.gz has no row with an escape immediately
// after the host, which is why the corpus is green on all six.
//
// The literal spellings of the same content are already correct:
// `https://www.acme.fia` is left alone, because there the byte after the host is
// plainly a host byte. Only the escaped spelling is wrong.
func TestAJSONEscapeIsNotAHostBoundary(t *testing.T) {
	m := r53Matcher(t, false)
	for _, r := range []rune{0x00a0, 0x00e4, 0x2013, 0x2026, 0x00bb, 0x200b} {
		in := r53Escaped(r)
		out := string(RewriteJSON([]byte(in), m, NewStats(false), quiet(), false))
		if out != in {
			t.Errorf("U+%04X: a reference the browser resolves elsewhere was rewritten\n  in  %s\n  out %s",
				r, in, out)
		}
	}
	// The same bytes in an inline script, which is where wp_localize_script
	// writes them and where §5.2 says the JS URLs actually are.
	page := `<script type="application/json">` + r53Escaped(0x00a0) + `</script>`
	if got := r53HTML(t, page, m); got != page {
		t.Errorf("inline script:\n  in  %s\n  out %s", page, got)
	}
	// And the literal spelling, which must keep working: `\` really is a host
	// terminator when it is a byte rather than an escape.
	lit := "https://www.acme.fi" + r53Backslash() + "x"
	if got := string(HostLeaks(m, []byte(lit), true)); !strings.Contains(got, "wt-a--acme.ddev.site") {
		t.Errorf("a literal backslash stopped being a host boundary: %q -> %q", lit, got)
	}
}

// The escape asymmetry strands a variant hostname in the shared database.
//
// Forward, the escape is read as a boundary and the origin is rewritten to the
// variant. The browser's JSON parser then decodes the escape, so what the page
// holds — and posts back — is the variant host followed by a *letter*. The
// reverse direction reads that correctly and finds no origin there, so nothing
// maps it back: `wt-a--acme.ddev.site` is written into the client's production
// `wp_options` and stays.
//
// It is the mirror of test 28, and it is silent. `BrokenSerialized` is zero on
// both pages, because nothing about a length is wrong; `UnreadSerialized` is
// false, because the walk read every spelling it was given; and `hostshift diff`
// compares the proxy against the engine, which agree.
func TestAnEscapedNonASCIIByteDoesNotStrandAVariantHostUpstream(t *testing.T) {
	fwd, rev := r53Matcher(t, false), r53Matcher(t, true)
	for _, r := range []rune{0x00a0, 0x00e4, 0x2013, 0x2026, 0x00bb} {
		out := string(RewriteJSON([]byte(r53Escaped(r)), fwd, NewStats(false), quiet(), false))
		var page map[string]string
		if err := json.Unmarshal([]byte(out), &page); err != nil {
			t.Fatal(err)
		}
		held := page["u"] // what the browser's JSON parser hands the page

		// Every arm the browser can post it back through.
		back := map[string]string{
			"json": r53JSONPost(t, held, rev),
			"form": r53FormPost(t, held, rev),
			"text": string(RepairSerialized([]byte(held), r53RW(rev))),
		}
		names := []string{"form", "json", "text"}
		for _, arm := range names {
			if strings.Contains(back[arm], "wt-a--acme.ddev.site") {
				t.Errorf("U+%04X via %s: the variant hostname goes upstream\n"+
					"  served   %s\n  browser  %q\n  upstream %q", r, arm, out, held, back[arm])
			}
		}
	}
}

// r53JSONPost is what JSON.stringify sends: the decoded rune, not an escape.
func r53JSONPost(t *testing.T, f string, m *origin.Matcher) string {
	t.Helper()
	q, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	out := string(RewriteJSON([]byte(`{"u":`+string(q)+`}`), m, NewStats(false), quiet(), false))
	var mm map[string]string
	if err := json.Unmarshal([]byte(out), &mm); err != nil {
		t.Fatal(err)
	}
	return mm["u"]
}

func r53FormPost(t *testing.T, f string, m *origin.Matcher) string {
	t.Helper()
	b := string(RepairSerializedFields([]byte("u="+url.QueryEscape(f)+"&x=1"), r53RW(m)))
	d, err := url.QueryUnescape(r53Between(t, b, "u=", "&x=1"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// ---------------------------------------------------------------------------
// Three mutations the suite survived, in what 85b1525 and 7ecb95c changed.
// ---------------------------------------------------------------------------

// The root-dot carve-out knows four delimiters and only one of them was pinned.
//
// 85b1525 added `isURLDelim` so that a root dot is kept out of the matched range
// only when it really ends the authority — "a full stop is never followed by
// :80". Narrowing that function to `/` alone, or dropping any one of `\`, `?`
// and `#` from it, left the whole suite green.
//
// It is not a cosmetic branch. ada resolves every one of these to host
// `www.example.fi.`, which is the canonical origin, so the reference must be
// rewritten — and with the dot dropped from the matched range it survives *after
// the port*, giving `v.ddev.site:8443.`, a port no parser accepts. That is
// exactly the `8443.` failure 85b1525's own message describes, one delimiter
// over.
func TestTheRootDotCarveOutKnowsEveryDelimiter(t *testing.T) {
	m, err := origin.NewMatcher([]origin.Pair{{Name: "s",
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("http://v.ddev.site:8443")}})
	if err != nil {
		t.Fatal(err)
	}
	bs := r53Backslash()
	for _, c := range []struct{ in, want string }{
		{"See https:www.example.fi./x", "See http://v.ddev.site:8443/x"},
		{"See https:www.example.fi.?q=1", "See http://v.ddev.site:8443?q=1"},
		{"See https:www.example.fi.#f", "See http://v.ddev.site:8443#f"},
		{"See https:www.example.fi." + bs + "p", "See http://v.ddev.site:8443" + bs + "p"},
		// And the half that must not change: in prose with nothing URL-shaped
		// after it, the dot is a full stop and survives.
		{"See https:www.example.fi. Thanks", "See http://v.ddev.site:8443. Thanks"},
		{"See https:www.example.fi.", "See http://v.ddev.site:8443."},
	} {
		// value=false: prose, which is the only surface the carve-out runs on.
		if got := string(HostLeaks(m, []byte(c.in), false)); got != c.want {
			t.Errorf("in   %q\ngot  %q\nwant %q", c.in, got, c.want)
		}
	}
}

// The written-scheme arm's guard decides a whole class on a mixed-scheme map.
//
// 85b1525 split the port lookup in two: a scheme-relative reference asks this
// host's own target for the scheme, and only a reference that *wrote* its scheme
// falls back to `schemeAt`. Dropping `schemeWritten` from the second arm left the
// suite green, because every map in it has one scheme.
//
// On a mixed-scheme map it decides real bytes. The document is served at
// `https://wt-a--acme.ddev.site:8443`, so a browser resolves `//www.acme.fi:80/x`
// to `https://www.acme.fi:80` — a different origin from the canonical
// `https://www.acme.fi`, and one hostshift must not touch. Without the guard,
// `schemeAt` answers with some other site's `http`, 80 is default there, and a
// live production origin is replaced. It is the same failure 85b1525's message
// records finding, in the arm the fix did not gate.
func TestASchemeRelativePortIsNeverJudgedByAWrittenScheme(t *testing.T) {
	mp, err := origin.NewMap([]origin.Site{
		{Name: "a", Canonical: origin.MustParse("https://www.acme.fi"),
			Variant: origin.MustParse("https://wt-a--acme.ddev.site:8443")},
		{Name: "b", Canonical: origin.MustParse("http://www.beta.fi"),
			Variant: origin.MustParse("http://wt-b--beta.ddev.site")},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := mp.Forward()
	for _, c := range []struct{ in, want string }{
		// 80 is not default for the https this host is served on.
		{"//www.acme.fi:80/x", "//www.acme.fi:80/x"},
		// 443 is, so this one is the canonical origin and must be rewritten.
		// The scheme is written out because the target's scheme is not the
		// one the reference would have resolved under; the browser lands on
		// the same origin either way.
		{"//www.acme.fi:443/x", "https://wt-a--acme.ddev.site:8443/x"},
		{"//www.acme.fi/x", "https://wt-a--acme.ddev.site:8443/x"},
		// The mirror, on the site served over http.
		{"//www.beta.fi:443/x", "//www.beta.fi:443/x"},
		{"//www.beta.fi:80/x", "//wt-b--beta.ddev.site/x"},
	} {
		if got := string(HostLeaks(m, []byte(c.in), true)); got != c.want {
			t.Errorf("in   %q\ngot  %q\nwant %q", c.in, got, c.want)
		}
	}
}

// userinfoAt starts after the separator, not at the colon.
//
// It is reached only where verbatimSep declined — fewer than two slashes — so
// `https:/user@host` is the shape that exercises it. Removing the loop that
// skips the slash left the suite green and emits `http:///user@…`: three
// slashes, which every parser still resolves the same way, but bytes the proxy
// invented. Under an identity map that is test 24, and this arm is one splice
// away from it.
func TestUserinfoIsCopiedFromPastTheSeparator(t *testing.T) {
	m, err := origin.NewMatcher([]origin.Pair{{Name: "s",
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("http://v.ddev.site:8443")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ in, want string }{
		{"https:/user@www.example.fi/x", "http://user@v.ddev.site:8443/x"},
		{"https:/user:pw@www.example.fi/x", "http://user:pw@v.ddev.site:8443/x"},
		{"https:user@www.example.fi/x", "http://user@v.ddev.site:8443/x"},
		{"https://user@www.example.fi/x", "http://user@v.ddev.site:8443/x"},
	} {
		if got := string(HostLeaks(m, []byte(c.in), true)); got != c.want {
			t.Errorf("in   %q\ngot  %q\nwant %q", c.in, got, c.want)
		}
	}
}
