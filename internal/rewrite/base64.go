package rewrite

import (
	"bytes"
	"encoding/base64"
)

// HiddenInBase64 reports an origin carried inside a base64 blob, which no
// spelling can reach and which must not be rewritten anyway.
//
// The Customizer posts a widget instance as base64 of a PHP-serialized array,
// and validates it with `wp_hash()` over exactly those bytes: rewrite the host
// inside and re-emit the length, and WordPress answers `invalid_value` and
// discards the save. So this is not a spelling to add to §4.4's list — the
// correct handling is the one §4.3 already argues for everywhere else, which is
// to stop being silent.
//
// Measured, a widget link saved through the worktree put
// `https://wt-a--host/promo/` into production's `wp_options`, the canonical
// front page then served it to the public, `ddev logs -s hostshift` had nothing
// for the request, and `hostshift diff` printed GREEN. The blind spot is
// bidirectional: `customize.php` served through the variant carries the same
// blobs the other way.
//
// rw is the direction's rewriter. A blob counts only when decoding succeeds and
// the decoded bytes actually change under it, so a base64-looking run that holds
// no mapped origin — a nonce, an image, a hash — is not reported.
func HiddenInBase64(b []byte, rw func([]byte) []byte) (n int, sample []byte) {
	n, sample = hiddenInBase64(b, rw)
	// And again with the transport layer off. A blob in a form body is
	// percent-encoded — `+` becomes `%2B`, `/` becomes `%2F`, the padding `%3D` —
	// so a scan of the raw bytes is cut at every escape, and the escape's own hex
	// digits are themselves base64 characters, which shifts the alignment of what
	// is left. Round 66 shipped this detector with fixtures that spliced the blob
	// in raw: the one spelling that worked, and the one no `<form>`,
	// `URLSearchParams`, jQuery or `wp.customize` produces.
	//
	// `+` decodes to a space here, which is right: a literal `+` inside base64
	// arrives as `%2B`, and a raw one really was a space.
	if dec, ok := formDecodeOnly(string(b)); ok && dec != string(b) {
		if dn, ds := hiddenInBase64([]byte(dec), rw); dn > n {
			n, sample = dn, ds
		}
	}
	return n, sample
}

func hiddenInBase64(b []byte, rw func([]byte) []byte) (n int, sample []byte) {
	for i := 0; i < len(b); {
		if !isB64(b[i]) {
			i++
			continue
		}
		// Whitespace inside the run is part of it. RFC 2045 wraps base64 at 76
		// columns, so a `Content-Transfer-Encoding: base64` part splits the blob
		// across lines and a hostname straddling a break sits in no single run.
		// The alphabet check below still bounds what is decoded.
		j := i
		for j < len(b) && (isB64(b[j]) || isB64Space(b[j])) {
			j++
		}
		// Padding belongs to the run.
		for j < len(b) && b[j] == '=' {
			j++
		}
		// Short runs are words, hex digests and ids, not payloads. A serialized
		// array holding a URL cannot encode to fewer bytes than this.
		if j-i >= 32 {
			run := b[i:j]
			if hasB64Space(run) {
				run = stripB64Space(run)
			}
			if dec, err := base64.StdEncoding.DecodeString(string(run)); err == nil {
				if !bytes.Equal(rw(dec), dec) {
					n++
					if sample == nil {
						sample = dec
					}
				}
			}
		}
		i = j
	}
	return n, sample
}

func isB64Space(c byte) bool { return c == '\r' || c == '\n' || c == ' ' || c == '\t' }

func hasB64Space(b []byte) bool {
	for _, c := range b {
		if isB64Space(c) {
			return true
		}
	}
	return false
}

func stripB64Space(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if !isB64Space(c) {
			out = append(out, c)
		}
	}
	return out
}

func isB64(c byte) bool {
	return ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z') ||
		('0' <= c && c <= '9') || c == '+' || c == '/'
}
