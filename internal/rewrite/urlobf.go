package rewrite

import "bytes"

// The second gap between the matcher's model and the browser's: the matcher
// models *bytes*, and the browser runs a URL parser over them first.
//
// §5.3's three encodings all assume the origin is a contiguous run reading
// `scheme` `://` `host`. The WHATWG URL parser requires neither the run to be
// contiguous nor the separator to be two forward slashes, and every shape below
// was served unrewritten — and, worse, uncounted, so `--json` reported a clean
// page and the straggler sweep saw nothing either:
//
//	href="https://www.example.fi/x"    with a tab, LF or CR anywhere in it
//	href="https:\\www.example.fi/x"
//	href="https:///www.example.fi/x"   (or ////, /\, \/, //\ …)
//	href="//www.example.fi/x"          with a tab, LF or CR in the host
//	href="https://www.example&#9;.fi/x"
//
// Verified against the WHATWG parser: every one resolves to
// https://www.example.fi/x. That is test 28 — a production origin the browser
// dereferences — and it is reachable from any attacker-influenced content in the
// database, pointing the developer's authenticated browser at the live site.
//
// Two mechanisms in the URL spec produce all of it:
//
//   - Tab, LF and CR are removed from the whole URL before parsing. Nothing
//     else is: a space, a form feed or a NBSP makes the URL fail to parse
//     instead, so only these three are removed here.
//   - After `scheme:`, the parser skips a run of any length of `/` and `\`
//     before reading the authority ("special authority ignore slashes state").
//     The same applies at the start of a scheme-relative reference.
//
// So the value is normalised the way the parser would and re-matched, exactly
// as decodeEntityLeak does for character references, and for the same reason:
// pattern variants cannot close it, because the runs are unbounded. When the
// normalised form carries an origin the raw form did not, it replaces the value
// — which drops the obfuscation from the page, and is confined to values that
// would otherwise leak, so it never runs on a page that is already correct.

// removableRef reports the length of a character reference at b that spells a
// character the URL parser removes, or 0.
//
// These are handled here rather than in decodeURLRefs because that decoder must
// never *emit* a control character — doing so was one of the XSS holes this file
// sits next to. Removing one is not emitting one, so the same characters that
// are unsafe to decode are safe to delete.
func removableRef(b []byte) int {
	if len(b) < 4 || b[0] != '&' {
		return 0
	}
	if b[1] != '#' {
		lim := min(len(b), 10)
		end := bytes.IndexByte(b[1:lim], ';')
		if end < 0 {
			return 0
		}
		switch string(b[1 : 1+end]) {
		case "Tab", "NewLine":
			return end + 2
		}
		return 0
	}
	j, base := 2, 10
	if j < len(b) && (b[j] == 'x' || b[j] == 'X') {
		base, j = 16, j+1
	}
	start := j
	val := 0
	for j < len(b) {
		d, ok := digitVal(b[j], base)
		if !ok {
			break
		}
		val = val*base + d
		if val > 0x10FFFF {
			return 0
		}
		j++
	}
	if j == start {
		return 0
	}
	if j < len(b) && b[j] == ';' {
		j++
	}
	if val == '\t' || val == '\n' || val == '\r' {
		return j
	}
	return 0
}

func isURLStripped(c byte) bool { return c == '\t' || c == '\n' || c == '\r' }

func isSlashish(c byte) bool { return c == '/' || c == '\\' }

// normaliseURL applies the two parser rules to v, returning the normalised
// bytes and whether anything changed.
func normaliseURL(v []byte) ([]byte, bool) {
	// The ordinary case, and the one that must stay cheap: no removable
	// character and no backslash means neither rule can fire. A lone extra '/'
	// still has to be looked at, so the slash run is checked below rather than
	// here.
	fast := true
	for _, c := range v {
		if isURLStripped(c) || c == '\\' || c == '&' || c == '/' {
			fast = false
			break
		}
	}
	if fast {
		return v, false
	}

	// Rule one: delete tab, LF and CR, in raw and character-reference form.
	stripped := make([]byte, 0, len(v))
	changed := false
	for i := 0; i < len(v); {
		if isURLStripped(v[i]) {
			i++
			changed = true
			continue
		}
		if v[i] == '&' {
			if n := removableRef(v[i:]); n > 0 {
				i += n
				changed = true
				continue
			}
		}
		stripped = append(stripped, v[i])
		i++
	}

	// Rule two: the authority separator. It is a run of '/' and '\' of length
	// two or more, either at the very start of the value or immediately after
	// `http:` / `https:`. Anywhere else a slash is a path and must not be
	// touched — `/a//b` is a path with an empty segment, not an authority.
	at := 0
	if n := schemeLen(stripped); n > 0 {
		at = n
	} else if len(stripped) > 0 && isSlashish(stripped[0]) {
		at = 0
	} else {
		return finish(v, stripped, changed)
	}
	end := at
	for end < len(stripped) && isSlashish(stripped[end]) {
		end++
	}
	if end-at < 2 {
		// One slash is a path relative to the base, not an authority, and zero
		// is not a separator at all.
		return finish(v, stripped, changed)
	}
	if end-at != 2 || stripped[at] != '/' || stripped[at+1] != '/' {
		out := make([]byte, 0, len(stripped))
		out = append(out, stripped[:at]...)
		out = append(out, '/', '/')
		out = append(out, stripped[end:]...)
		return out, true
	}
	return finish(v, stripped, changed)
}

func finish(v, stripped []byte, changed bool) ([]byte, bool) {
	if !changed {
		return v, false
	}
	return stripped, true
}

// schemeLen returns the length of a leading "http:" or "https:", or 0.
//
// Case-insensitive, because the URL parser lowercases the scheme, and only
// these two because they are the only schemes hostshift maps.
func schemeLen(b []byte) int {
	for _, s := range [][]byte{[]byte("https:"), []byte("http:")} {
		if len(b) >= len(s) && hasFoldPrefixASCII(b[:len(s)], s) {
			return len(s)
		}
	}
	return 0
}

func hasFoldPrefixASCII(b, want []byte) bool {
	for i := range want {
		c := b[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != want[i] {
			return false
		}
	}
	return true
}

// normaliseURLLeak is the seam, alongside decodeEntityLeak and with the same
// contract: it returns v untouched unless the normalised form carries an origin
// that the value as written did not.
func (w *HTML) normaliseURLLeak(base int, v []byte) []byte {
	norm, ok := normaliseURL(v)
	if !ok {
		return v
	}
	out, events := w.m.Rewrite(norm, SurfaceHTMLObfuscated, w.stats.Explain())
	if bytes.Equal(out, norm) {
		// The normalised form holds no origin either. Leave the value exactly as
		// its author wrote it — this pass exists to stop a leak, not to tidy
		// anyone's markup.
		return v
	}
	w.stats.Record(SurfaceHTMLObfuscated, base, events)
	return out
}
