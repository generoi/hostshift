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

// scanAttrs returns the attribute spans of a start or self-closing tag.
// raw must be exactly what Tokenizer.Raw() returned.
func scanAttrs(raw []byte) []Attr {
	var out []Attr
	i := 1 // skip '<'
	for i < len(raw) && !isSpace(raw[i]) && raw[i] != '>' && raw[i] != '/' {
		i++
	}
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
