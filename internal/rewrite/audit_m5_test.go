package rewrite

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestOversizeTokenKeepsEveryByte is test 24 for the token cap, and it is the
// case the cap exists for.
//
// x/net/html's readByte advances raw.end *before* it tests maxBuf, so at
// ErrBufferExceeded the oversized token sits in Raw() and Buffered() holds only
// read-ahead. A text token is handed back as a partial TextToken first, so text,
// <script> and comments survived; a *tag* errors from inside readStartTag with
// its bytes still in Raw(), and the passthrough emitted Buffered() alone —
// deleting exactly MaxToken bytes, every time.
//
// At the shipped 4 MiB default that is a 5 MB page arriving 4 MiB short with
// status 200 and no Content-Length to check it against: the opening
// <img src="data:image/png;base64, gone and the rest of its value rendered as
// visible page text. An inlined LCP image or a multi-MB Elementor
// data-settings attribute is all it takes.
func TestOversizeTokenKeepsEveryByte(t *testing.T) {
	const cap = 4096
	m := identityMatcher(t)
	filler := strings.Repeat("x", cap+1000)

	for _, c := range []struct{ name, in string }{
		{"attr value", `<p>a</p><img alt="` + filler + `" src="/a.png"><p>b</p>`},
		{"attr value as the first token", `<img alt="` + filler + `" src="/a.png"><p>b</p>`},
		{"many small attrs in one tag", `<p>a</p><div ` + strings.Repeat(`d="v" `, cap/3) + `>x</div><p>b</p>`},
		{"long tag name", `<p>a</p><` + filler + `><p>b</p>`},
		{"oversized text", `<p>a</p>` + filler + `<p>b</p>`},
		{"oversized script", `<p>a</p><script>` + filler + `</script><p>b</p>`},
		{"oversized comment", `<p>a</p><!--` + filler + `--><p>b</p>`},
		{"oversized token at the very end", `<p>a</p><img alt="` + filler + `"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := []byte(c.in)
			out := runHTML(t, in, m, Options{MaxToken: cap, Log: quiet()})
			if !bytes.Equal(out, in) {
				t.Errorf("identity map is not byte-identical: in=%d out=%d lost=%d",
					len(in), len(out), len(in)-len(out))
			}
		})
	}
}

// TestOversizeTokenAtManyCapBoundaries sweeps the filler length across the cap,
// because the loss was exactly MaxToken bytes and a single size could have hit
// a lucky alignment.
func TestOversizeTokenAtManyCapBoundaries(t *testing.T) {
	const cap = 4096
	m := identityMatcher(t)
	for _, n := range []int{cap - 96, cap - 1, cap, cap + 1, cap + 900, 2 * cap, 5 * cap} {
		in := []byte(`<p>a</p><img alt="` + strings.Repeat("x", n) + `" src="/a.png"><p>b</p>`)
		for _, chunk := range []int{1, 37, 4096} {
			out, err := io.ReadAll(NewHTML(&chunked{b: append([]byte(nil), in...), n: chunk}, m, nil,
				Options{MaxToken: cap, Log: quiet()}))
			if err != nil {
				t.Fatalf("filler=%d chunk=%d: %v", n, chunk, err)
			}
			if !bytes.Equal(out, in) {
				t.Errorf("filler=%d chunk=%d: lost %d bytes", n, chunk, len(in)-len(out))
			}
		}
	}
}

// TestStragglerOffsetsSurviveThePassthrough. The tail bypasses write(), so no
// mark was recorded and the two streams stopped advancing together —
// InputOffset then returned output offsets for everything after the cap, which
// on the 5 MB page above put a reported straggler 4 MiB into the wrong part of
// the document.
func TestStragglerOffsetsSurviveThePassthrough(t *testing.T) {
	const cap = 4096
	m := realMatcher(t)
	// A rewrite first, so the streams are out of step; then an oversized tag;
	// then an origin only the sweep can see, inside the passthrough tail.
	head := `<a href="https://www.canon.test/"></a>`
	big := `<img alt="` + strings.Repeat("x", cap+500) + `" src="/a.png">`
	tail := `<!-- https://www.canon.test/z -->`
	in := head + big + tail
	want := strings.Index(in, "https://www.canon.test/z")

	st := NewStats(true)
	if _, err := io.ReadAll(NewResponseBody(strings.NewReader(in), m, nil,
		Options{MaxToken: cap, Stats: st, Log: quiet()})); err != nil {
		t.Fatal(err)
	}

	var found []int
	for _, e := range st.Events() {
		if e.Surface == SurfaceStraggler && e.Action == "rewrote" {
			found = append(found, e.Offset)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one straggler in the passthrough tail, got %v", found)
	}
	if found[0] != want {
		t.Errorf("straggler reported at %d, its input offset is %d (drift %+d)",
			found[0], want, found[0]-want)
	}
}
