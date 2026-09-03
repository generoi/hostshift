package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// TestR74EarlyHintsBypassModifyResponse measures what reaches the browser in a
// 1xx informational response.
//
// httputil.ReverseProxy forwards 1xx responses to the client through an
// httptrace.Got1xxResponse hook that copies the header verbatim and calls
// WriteHeader. ModifyResponse is never consulted for them, so
// rewriteResponseHeaders — PLAN §5.2's whole guarantee for the header surface —
// does not run. A `103 Early Hints` carrying `Link: <https://canonical/…>;
// rel=preload` is a production URL the browser fetches before the final
// response arrives; the identical header on the final response is rewritten.
func TestR74EarlyHintsBypassModifyResponse(t *testing.T) {
	// OPEN FINDING, skipped rather than deleted. See PLAN §5.2.
	//
	// `httputil.ReverseProxy` forwards an informational response through its own
	// `httptrace.Got1xxResponse` hook, which copies the header verbatim and calls
	// WriteHeader; `ModifyResponse` is never consulted, so `originHeaders` — which
	// PLAN §5.2 calls "the whole guarantee for the header surface" — never runs on
	// a `103 Early Hints`. A browser preloads from a 103, so a `Link: rel=preload`
	// naming production is a fetch issued before the page arrives.
	//
	// Adding a second `Got1xxResponse` via the request context does not fix it:
	// httputil installs its own, and the composed hooks race the WriteHeader, so
	// the mutation lands after the header is already on the wire. Closing this
	// needs the header pass to run on the *upstream* side — a RoundTripper wrapper
	// or a 1xx-aware transport — which is a structural change, not a hook.
	//
	// Ranked SMALL by the audit that found it because nothing in a stock
	// WordPress/nginx stack emits 103; the mechanism is real, the realism is what
	// limits it.
	// CLOSED. The hook is installed by a RoundTripper rather than on the inbound
	// request, which is the whole fix: httptrace.WithClientTrace *composes*, and
	// the trace added last runs first. Setting it in the handler put it behind
	// httputil's — which copies the header and calls WriteHeader — so the
	// mutation landed after the bytes were on the wire. Inside the transport,
	// httputil's trace is already on the context, so ours is the later one and
	// runs before it, mutating the header map httputil then copies.
	const canon = "https://www.r74a.example/wp-content/style.css"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "<"+canon+">; rel=preload; as=style")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		// The same header on the final response, which ModifyResponse does see.
		w.Header().Set("Link", "<"+canon+">; rel=preload; as=style")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(200)
		w.Write([]byte("<p>ok</p>"))
	}))
	defer up.Close()

	p := r74proxy(t, up.URL)
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	var got1xx []textproto.MIMEHeader
	req, _ := http.NewRequest("GET", front.URL+"/", nil)
	req.Host = "wt-a--r74w.ddev.site"
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
			got1xx = append(got1xx, h)
			return nil
		},
	}))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	// The control: the same header, on the final response, is rewritten. If
	// this fails the harness is not measuring the proxy at all.
	if strings.Contains(res.Header.Get("Link"), "r74a.example") {
		t.Fatalf("harness: the final Link was not rewritten either: %q", res.Header.Get("Link"))
	}
	if len(got1xx) == 0 {
		t.Skip("no 1xx response was forwarded to the client at all")
	}
	for _, h := range got1xx {
		if strings.Contains(h.Get("Link"), "r74a.example") {
			t.Errorf("a canonical origin reached the browser in a 103 Early Hints "+
				"Link header: %q (the same header on the final response came out %q)",
				h.Get("Link"), res.Header.Get("Link"))
		}
	}
}

// TestR74BareOriginBeforeAControlInAnAttributeIsNotRewritten
//
// Measured first on stock WordPress 7.1 through a running proxy, not
// constructed: `wp-admin/site-health.php?tab=debug` renders the copy-to-clipboard
// report into a *single attribute*, `<button … data-clipboard-text="…">`, whose
// value is many lines separated by raw LF. Two of those lines are
//
//	WP_HOME: https://www.hostshift-a.example
//	WP_SITEURL: https://www.hostshift-a.example
//
// and both survived to the browser, while the identical values in the `<table>`
// six kilobytes earlier on the same page came out as the variant. Neither the
// §4.4 straggler sweep nor `hostshift diff` said anything: the sweep logged no
// WARN, and `diff` printed "GREEN: no canonical origin reached the browser"
// with LEAKS 0, because countLeaks scores a body by re-running the engine on
// it and the engine declines the same origin twice.
//
// The rule the matrix below finds: inside an attribute value, a *bare* origin —
// scheme and host, no path — followed by TAB, LF or CR and then more content is
// left alone. Those are exactly the three bytes the WHATWG URL parser removes
// from inside a URL, so the parser view reads `https://host` + LF + `B` as the
// host `hostB`, which is not canonical. In an `href` that reading is right: a
// browser resolves that attribute to `hostB` too. In `ping` and `srcset`,
// whose grammars make whitespace a *separator*, it is wrong and the browser
// dereferences production; in `data-clipboard-text`, `content`, `title` and
// every other prose attribute, it is wrong and production's hostname is shown
// to the developer.
//
// Element text on the same surfaces already answers correctly, which is why
// this is a question about the attribute view rather than the prose one.
func TestR74BareOriginBeforeAControlInAnAttributeIsNotRewritten(t *testing.T) {
	// OPEN FINDING, skipped rather than deleted. See PLAN §5.2.
	//
	// `html-attr` joins because `href`, `src` and `action` are handed to a URL
	// parser, which strips the three controls — right for those, wrong for a
	// whitespace-separated list (`ping`, `srcset`, where an LF really does end one
	// URL and start the next) and wrong for a prose attribute, which is how
	// WordPress core's site-health page put two production origins in one
	// `data-clipboard-text` and shipped them.
	//
	// The obvious remedy — extending `joinsControlsIn`'s space-or-second-scheme
	// discriminant to `html-attr` — was measured and is wrong: it takes
	// TestR52CrossProduct from 0 to **6800** failing cells, because a control can
	// sit *inside* the separator (`https:/<CR><LF>/host`, which ada resolves to
	// the host) in an attribute that also contains prose. Inside versus after,
	// again. The remedy is the two-view design round 74 built for the tab,
	// extended to attributes — one pass joining, a second keeping the controls —
	// and that is a change to make deliberately, not at the end of a round.
	const canon = "https://www.r74a.example"

	type cell struct{ name, doc string }
	var cells []cell
	for _, sep := range []struct{ name, b string }{
		{"SP", " "}, {"TAB", "\t"}, {"LF", "\n"}, {"CR", "\r"}, {"CRLF", "\r\n"},
	} {
		for _, tail := range []struct{ name, b string }{{"bare", ""}, {"path", "/p"}} {
			cells = append(cells,
				cell{"attr/" + tail.name + "/" + sep.name,
					`<button data-clipboard-text="A: ` + canon + tail.b + sep.b + `B: end"></button>`},
				cell{"text/" + tail.name + "/" + sep.name,
					`<p>A: ` + canon + tail.b + sep.b + `B: end</p>`},
				// `ping` is space-separated per HTML, and LF is a space
				// character: the browser POSTs to each token.
				cell{"ping/" + tail.name + "/" + sep.name,
					`<a href="/x" ping="` + canon + tail.b + sep.b + canon + `/second">x</a>`},
			)
		}
	}

	for _, c := range cells {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, "<!doctype html><html><body>"+c.doc+"</body></html>")
		}))
		p := r74proxy(t, up.URL)
		front := httptest.NewServer(p.Handler())

		req, _ := http.NewRequest("GET", front.URL+"/", nil)
		req.Host = "wt-a--r74w.ddev.site"
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(res.Body)
		res.Body.Close()
		front.Close()
		up.Close()

		// The body has to have arrived at all. Without this, any wiring mistake
		// that yields an empty body — an unmatched Host, a refusal — scores
		// every cell clean and the whole matrix passes.
		if !bytes.Contains(out, []byte("</body>")) {
			t.Fatalf("harness: %s got no document back (%d): %q", c.name, res.StatusCode, out)
		}
		leaked := bytes.Contains(out, []byte("r74a.example"))
		switch {
		case strings.HasPrefix(c.name, "text/") && leaked:
			t.Errorf("%s: element text left a canonical origin behind: %q", c.name, out)
		case strings.HasSuffix(c.name, "/SP"), strings.Contains(c.name, "/path/"):
			// The controls. If these leak, the harness is wrong, not the proxy.
			if leaked {
				t.Fatalf("harness: control %s leaked, so nothing here is measuring "+
					"the proxy: %q", c.name, out)
			}
		case leaked:
			t.Errorf("%s: a canonical origin reached the browser: %q", c.name, out)
		}
	}
}

func r74proxy(t *testing.T, upstream string) *Proxy {
	t.Helper()
	c, err := origin.Parse("https://www.r74a.example")
	if err != nil {
		t.Fatal(err)
	}
	v, err := origin.Parse("https://wt-a--r74w.ddev.site")
	if err != nil {
		t.Fatal(err)
	}
	m, err := origin.NewMap([]origin.Site{{Name: "s", Canonical: c, Variant: v}})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(upstream)
	return &Proxy{
		Upstream: u, Map: m, Stats: rewrite.NewStats(false),
		Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	}
}
