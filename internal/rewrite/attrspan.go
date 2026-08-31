package rewrite

// Intra-tag attribute-value span scanner.
//
// x/net/html's Tokenizer gives framing and Raw(), but does not report where an
// attribute's *value* sits inside the raw tag bytes. Splicing needs that, so
// this supplies it: the HTML5 tag-open / attribute-name / before-value /
// quoted-and-unquoted-value states, and nothing else.
//
// Lifted from spike/go/attrspan/main.go, which was validated against the
// tokenizer's own output over the corpus: 19,953 start tags, 37,280 attributes,
// 9 divergences across 6 files — all duplicate attribute names (rel, media,
// defer, class, data-wp-on-window--resize), where this reports every physical
// position. Dropping duplicates would lose a splice site, so reporting them all
// is the correct behaviour, not a defect. attrspan_test.go carries that
// validation, extended to assert ValueStart/ValueEnd, which the spike's
// validator never checked (PLAN §5.2).

// Attr is one attribute's position within a start tag's raw bytes.
type Attr struct {
	NameStart, NameEnd   int
	ValueStart, ValueEnd int  // both -1 for a valueless attribute
	Quote                byte // '"', '\'', or 0 when unquoted
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

// tagNameOf returns the element name of a start or self-closing tag, as a slice
// of raw rather than a copy.
//
// It stops exactly where scanAttrs starts, on the same terminator set, which is
// also x/net/html's readTagName. Deriving it here is what lets the tokenizer's
// TagName() go unused: that function is bytes.ReplaceAll over the tag name, and
// Go's bytes.Replace copies even when it replaces nothing — so every start tag
// paid a copy of its name purely to be compared against ten constants.
//
// It also removes the *second* copy that comparison forced. TagName() may
// invalidate Raw(), an undocumented lifetime the code could only hedge against
// by cloning every raw tag first. With TagName() and TagAttr() never called,
// the aliasing hazard at spike/go/full/main.go:100-105 cannot recur at all.
func tagNameOf(raw []byte) []byte {
	i := 1 // skip '<'
	for i < len(raw) && !isSpace(raw[i]) && raw[i] != '>' && raw[i] != '/' {
		i++
	}
	return raw[1:i]
}

// endTagNameOf is tagNameOf for `</name>`. tagNameOf stops at the first `/`, so
// on an end tag it returns nothing — which silently left a foreign-content depth
// counter never decrementing.
func endTagNameOf(raw []byte) []byte {
	i := 1
	if i < len(raw) && raw[i] == '/' {
		i++
	}
	j := i
	for j < len(raw) && !isSpace(raw[j]) && raw[j] != '>' && raw[j] != '/' {
		j++
	}
	return raw[i:j]
}

// scanAttrs returns the attribute spans of a start or self-closing tag.
// raw must be exactly what Tokenizer.Raw() returned.
func scanAttrs(raw []byte) []Attr { return scanAttrsInto(nil, raw) }

// scanAttrsInto is scanAttrs with a caller-supplied buffer, so one page's worth
// of tags reuses a single slice instead of allocating per tag. The spans are
// offsets into raw and are consumed before the next token, so nothing outlives
// the reuse.
func scanAttrsInto(out []Attr, raw []byte) []Attr {
	i := len(tagNameOf(raw)) + 1
	for i < len(raw) {
		for i < len(raw) && isSpace(raw[i]) {
			i++
		}
		if i >= len(raw) || raw[i] == '>' {
			break
		}
		if raw[i] == '/' { // self-closing slash, or a stray one
			i++
			continue
		}
		a := Attr{NameStart: i, ValueStart: -1, ValueEnd: -1}
		for i < len(raw) && !isSpace(raw[i]) && raw[i] != '=' && raw[i] != '>' && raw[i] != '/' {
			i++
		}
		a.NameEnd = i
		for i < len(raw) && isSpace(raw[i]) {
			i++
		}
		if i < len(raw) && raw[i] == '=' {
			i++
			for i < len(raw) && isSpace(raw[i]) {
				i++
			}
			if i < len(raw) && (raw[i] == '"' || raw[i] == '\'') {
				q := raw[i]
				i++
				a.Quote, a.ValueStart = q, i
				for i < len(raw) && raw[i] != q {
					i++
				}
				a.ValueEnd = i
				if i < len(raw) {
					i++
				}
			} else {
				a.ValueStart = i
				for i < len(raw) && !isSpace(raw[i]) && raw[i] != '>' {
					i++
				}
				a.ValueEnd = i
			}
		}
		out = append(out, a)
	}
	return out
}
