package proxy

import (
	"bytes"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/rewrite"
)

// The response direction is silent about the very thing the request direction
// warns on.
//
// `rewrite.HiddenInBase64` is called in exactly two places in the tree —
// internal/proxy/proxy.go:859 and internal/corpus/diff.go:377 — and both ask
// the *reverse* map, i.e. both look for a variant hostname. Nothing asks the
// forward map, so a *canonical* hostname carried inside base64 goes to the
// browser with no WARN, no counter and no `--explain` event.
//
// The blob is not inert. `WP_REST_Widgets_Controller::prepare_item_for_response`
// returns a legacy widget's settings as `instance.encoded`, base64 of the
// serialized instance; the widgets screen and the Customizer decode it in
// JavaScript and render the widget, so an `<a href>` or an `<img src>` inside it
// becomes a live production URL in the developer's authenticated browser. PLAN
// §4.3 accepts that hostshift must not rewrite those bytes — `wp_hash()` covers
// exactly them — and says the property that matters is that something reports
// it: "It does not fix the corruption; it ends the silence".
func TestR68ACanonicalOriginInsideBase64InAResponseIsReported(t *testing.T) {
	link := `<a href="` + canonical + `/promo/">the promo</a>`
	blob := base64.StdEncoding.EncodeToString([]byte(
		`a:1:{s:7:"content";s:` + strconv.Itoa(len(link)) + `:"` + link + `";}`))
	page := `<!doctype html><html><body><input type="hidden" ` +
		`name="widget-text[2][instance][encoded]" value="` + blob + `"></body></html>`

	var lb bytes.Buffer
	st := rewrite.NewStats(true)
	h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, page)
	}, func(p *Proxy) {
		p.Stats = st
		p.Log = slog.New(slog.NewTextHandler(&lb, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	_, got := h.get(t, variantHost, "/wp-admin/widgets.php")

	// The bytes go out unchanged, which is correct and is exactly why something
	// has to say so.
	if !strings.Contains(string(got), blob) {
		t.Fatalf("the blob was rewritten, which would break wp_hash(): %q", got)
	}
	if n, _ := rewrite.HiddenInBase64([]byte(got), func(b []byte) []byte {
		out, _ := h.proxy.Map.Forward().Rewrite(b, rewrite.SurfaceText, false)
		return out
	}); n == 0 {
		t.Fatal("fixture is wrong: the served page carries no canonical origin in base64")
	}
	if !strings.Contains(lb.String(), "base64") {
		t.Errorf("a canonical origin was served to the browser inside base64 and the "+
			"proxy logged nothing — the request direction WARNs on the mirror image "+
			"(proxy.go:859). Logged:\n%s", lb.String())
	}
}

// The mirror, which does warn. Kept beside it so the asymmetry is the assertion
// rather than a claim about it.
func TestR68TheRequestDirectionStillWarns(t *testing.T) {
	link := `<a href="https://` + variantHost + `/promo/">the promo</a>`
	blob := base64.StdEncoding.EncodeToString([]byte(
		`a:1:{s:7:"content";s:` + strconv.Itoa(len(link)) + `:"` + link + `";}`))

	var lb bytes.Buffer
	h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}, func(p *Proxy) {
		p.Log = slog.New(slog.NewTextHandler(&lb, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	h.do(t, "POST", variantHost, "/wp-admin/admin-ajax.php",
		"application/x-www-form-urlencoded", []byte("action=customize_save&customized="+blob))
	if !strings.Contains(lb.String(), "base64") {
		t.Fatalf("control case regressed; the request direction logged:\n%s", lb.String())
	}
}
