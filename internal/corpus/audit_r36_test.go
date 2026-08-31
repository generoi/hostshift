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

// The baseline subtraction cancels a blind spot the proxy and the detector
// share, which is the false GREEN this file already records twice.
//
// BrokenSerialized walks the same seven spellings repairAt does, and neither
// reads the JSON quote WordPress core's Interactivity API writes —
// `wp_json_encode($ctx, JSON_HEX_TAG|JSON_HEX_APOS|JSON_HEX_QUOT|JSON_HEX_AMP)`
// spells `"` as `"` inside the string. So:
//
//   - the proxy declines the value, rewrites the host anyway, and re-emits no
//     length: PHP 8.4 unserialize() returns false on the served bytes;
//   - the detector counts one broken value on the canonical page and one on the
//     variant page, and `variant - canonical` is zero;
//   - `want` is computed by applyLikeTheProxy, which is blind the same way, so
//     the bodies are byte-identical and no line count moved.
//
// Every column reads clean and the run prints "corpus diff GREEN: no canonical
// origin reached the browser, no page re-serialised" over a page carrying a
// blob PHP refuses. See TestAJSONHexQuotedSerializedValueKeepsItsLength in
// internal/rewrite for the served bytes.
func TestAJSONHexQuotedBlobIsNotAGreenRun(t *testing.T) {
	// Not testMap: c.example and v.example are the same byte length, so a stale
	// length would be indistinguishable from a correct one.
	canon := origin.MustParse("https://www.canon.test")
	variant := origin.MustParse("https://v.ddev.site")
	m, err := origin.NewMap([]origin.Site{{Name: "main", Canonical: canon, Variant: variant}})
	if err != nil {
		t.Fatal(err)
	}

	canonURL := "https://www.canon.test/a.png"
	if len(canonURL) == len("https://v.ddev.site/a.png") {
		t.Fatalf("the fixture's two URLs are the same length, so this asserts nothing")
	}
	// `"` from bytes; a Go escape would be a real quote.
	q := string([]byte{'\\', 'u', '0', '0', '2', '2'})
	blob := fmt.Sprintf(`a:1:{i:0;s:%d:%s%s%s;}`,
		len(canonURL), q, strings.ReplaceAll(canonURL, "/", `\/`), q)
	page := `<div data-wp-context='{"b":"` + blob + `"}'>x</div>`

	// The variant side is the canonical page through the real engine — what the
	// proxy would serve.
	served, err := io.ReadAll(rewrite.NewResponseBody(
		strings.NewReader(page), m.Forward(), nil, rewrite.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(served, []byte("v.ddev.site")) {
		t.Fatalf("the engine did not rewrite the fixture at all, so this asserts nothing:\n%s", served)
	}
	// The premise: the bytes the browser is handed do not parse. Asserted here
	// so that a fix to the walk turns this test green for the right reason.
	if !bytes.Contains(served, []byte(fmt.Sprintf("s:%d:", len(canonURL)))) {
		t.Skip("the walk now re-emits this length; nothing broken is served, so there is nothing to detect")
	}

	serve := func(body string) *url.URL {
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

	results, err := Run(context.Background(), Options{
		Canonical: serve(page), Variant: serve(string(served)),
		Map: m, Paths: []string{"/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	green := WriteReport(&buf, results)
	if green {
		t.Errorf("the run was GREEN over a page whose serialized value PHP refuses:\n"+
			"  canonical %s\n  served    %s\n%s", page, served, buf.String())
	}
	if results[0].BrokenSerialized == 0 {
		t.Errorf("the served stale length was not counted: the canonical baseline "+
			"cancelled it (canon=%d variant=%d)",
			rewrite.BrokenSerialized([]byte(page)), rewrite.BrokenSerialized(served))
	}
}
