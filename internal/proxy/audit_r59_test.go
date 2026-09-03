package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// Round 59, on 05ae5ea, auditing the response-header emission pipeline: every
// decision made between an upstream header value and the bytes the browser
// receives, and every mirror of that pipeline elsewhere in the tree.

// The self-redirect guard decides with a pipeline the emission pass no longer
// runs.
//
// Round 58 split the header surface by direction — `SurfaceResponseHeader` does
// not decode CSS escapes, because a browser following a `Location` runs the URL
// parser and nothing else — and moved `modifyResponse`'s emission loop onto it.
// The guard eleven lines above that loop was left on `SurfaceHeader`:
//
//	rewritten, _ := fwd.Rewrite([]byte(loc), rewrite.SurfaceHeader, false)
//	rewritten = rewrite.HostLeaksCounted(fwd, rewritten, true, p.Stats, rewrite.SurfaceHeader, 0)
//
// so it still reads `\65` as a CSS hex escape for `e`, finds a canonical host
// nothing else in the response path can see, and answers "yes, this Location is
// the URL the browser just asked for". It is the same defect round 58 recorded
// fixing, in the one caller it did not move: a decision whose whole job is to
// predict what the emission pass will do, computed by a different pipeline.
//
// ada, with the variant origin as base:
//
//	new URL("https://www.exampl\\65.fi/x", base).host === "www.exampl"
//	new URL("https://www.example.f\\69/x", base).host === "www.example.f"
//	new URL("https://\\77ww.example.fi/x", base).host === "77ww.example.fi"
//
// None of those is this map's canonical and none is its variant: a browser
// handed one of them leaves for a third-party host. Under `--strict-origins` —
// the flag that exists so a preview never sends anyone to production —
// hostshift answers 404 and says the reason is that it "forbids redirecting the
// browser to the canonical origin", which ada says is not where this redirect
// goes. A page that serves is turned into a 404 on a false diagnosis. Without
// the flag the same misreading records an `ActionSkipped/self-redirect` census
// event, which is the count PLAN §4.4 weighs the leak budget against.
//
// 52 of the 56 one-character CSS escapes of this canonical host do it.
func TestR59TheSelfRedirectGuardAsksTheSurfaceItAnswersOn(t *testing.T) {
	// candidate → the host ada resolves it to, from the variant origin.
	elsewhere := map[string]string{
		`https://www.exampl\65.fi/x`:  "www.exampl",
		`https://www.example.f\69/x`:  "www.example.f",
		`https://\77ww.example.fi/x`:  "77ww.example.fi",
		`https://www.exam\70 le.fi/x`: "www.exam",
	}
	for loc, adaHost := range elsewhere {
		t.Run(loc, func(t *testing.T) {
			p, st := r59Proxy(t)
			p.StrictOrigins = true
			code, out := r59Redirect(t, p, "/x", loc)
			if code != http.StatusFound {
				t.Errorf("a browser resolves this Location to %q, which is neither the\n"+
					"canonical nor the variant, so it is not a self-redirect and\n"+
					"--strict-origins has nothing to forbid — hostshift answered %d:\n"+
					" in  %q\n out %q", adaHost, code, loc, out)
			}
			if n := st.Snapshot().Skips[origin.ReasonSelfRedirect]; n != 0 {
				t.Errorf("the census recorded %d self-redirect skip(s) for a Location a\n"+
					"browser resolves to %q — the carve-out counted against a redirect\n"+
					"that never takes anyone to production: %q", n, adaHost, loc)
			}
		})
	}

	// The control cell: a real self-redirect still is one, so the assertion
	// above is not passing by turning the guard off.
	//
	// ada: new URL("https://www.example.fi/x", base).host === "www.example.fi",
	// which is this map's canonical, and rewriting it yields the URL the browser
	// asked for — PLAN §4.4 and test 32's carve-out exactly.
	p, st := r59Proxy(t)
	p.StrictOrigins = true
	if code, out := r59Redirect(t, p, "/x", `https://`+r58Canonical+`/x`); code != http.StatusNotFound {
		t.Errorf("the guard stopped seeing a real self-redirect: status %d, out %q", code, out)
	}
	p2, _ := r59Proxy(t)
	if _, out := r59Redirect(t, p2, "/x", `https://`+r58Canonical+`/x`); out != `https://`+r58Canonical+`/x` {
		t.Errorf("a self-redirect must go out untouched (test 32), got %q", out)
	}
	_ = st
}

// The census names the direction the header came from — and it names the other
// one.
//
// Round 58 gave the response direction its own surface and then wrote the two
// `Stats.Record` labels the wrong way round: `rewriteRequest`'s Referer/Origin
// pass records `SurfaceResponseHeader`, and `modifyResponse`'s Tier 1 loop
// records `SurfaceHeader`. Nothing else in the tree crosses the two, so the
// suite could not see it.
//
// It is the developer-facing signal for exactly the two failures this tool
// separates. `check` prints, at the moment of a test-28 refusal, "To see which
// surface they are on, turn the census on … | grep census", so a response header
// that carried a live production origin to the browser is reported on the
// surface named for requests, and a variant hostname on its way into the shared
// database — §4.3 — is reported on the surface named for responses. That is the
// same inversion round 58 fixed for the size-cap count, in the same commit.
func TestR59TheHeaderCensusNamesTheDirectionItCameFrom(t *testing.T) {
	// A response header and no request header: only the forward header pass can
	// rewrite anything.
	h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Location", canonical+"/x")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
	})
	req, _ := http.NewRequest("GET", h.front.URL+"/p", nil)
	req.Host = variantHost
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if got := res.Header.Get("Content-Location"); got != variant+"/x" {
		t.Fatalf("fixture: the header was not rewritten at all, got %q", got)
	}
	snap := h.stats.Snapshot()
	if snap.Rewrites[rewrite.SurfaceResponseHeader] == 0 {
		t.Errorf("a Tier 1 response header was rewritten and the census filed it under\n"+
			"%q, the name round 58 gave the *request* direction. `check` sends a\n"+
			"developer to this field at the moment of a test-28 refusal:\n  %v",
			surfaceOf(snap.Rewrites, rewrite.SurfaceHeader, rewrite.SurfaceResponseHeader),
			snap.Rewrites)
	}
	if snap.Rewrites[rewrite.SurfaceHeader] != 0 {
		t.Errorf("nothing on the request direction was rewritten, yet %q counts %d:\n  %v",
			rewrite.SurfaceHeader, snap.Rewrites[rewrite.SurfaceHeader], snap.Rewrites)
	}

	// And the mirror: a request header and no response header.
	h2 := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
	})
	req2, _ := http.NewRequest("GET", h2.front.URL+"/p", nil)
	req2.Host = variantHost
	req2.Header.Set("Referer", variant+"/previous")
	res2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if got := h2.seen.Header.Get("Referer"); got != canonical+"/previous" {
		t.Fatalf("fixture: the Referer was not mapped back, got %q", got)
	}
	snap2 := h2.stats.Snapshot()
	if snap2.Rewrites[rewrite.SurfaceHeader] == 0 {
		t.Errorf("a Referer was mapped back to canonical — the §4.3 direction — and the\n"+
			"census filed it under %q, the name round 58 gave the *response*\n"+
			"direction:\n  %v",
			surfaceOf(snap2.Rewrites, rewrite.SurfaceResponseHeader, rewrite.SurfaceHeader),
			snap2.Rewrites)
	}
	if snap2.Rewrites[rewrite.SurfaceResponseHeader] != 0 {
		t.Errorf("no response header was rewritten, yet %q counts %d:\n  %v",
			rewrite.SurfaceResponseHeader,
			snap2.Rewrites[rewrite.SurfaceResponseHeader], snap2.Rewrites)
	}
}

// surfaceOf names whichever of two surfaces actually holds the count, for the
// message.
func surfaceOf(m map[string]int, wrong, right string) string {
	if m[wrong] > 0 {
		return wrong
	}
	return right
}

func r59Proxy(t *testing.T) (*Proxy, *rewrite.Stats) {
	t.Helper()
	p := r58Proxy(t, "https")
	p.Stats = rewrite.NewStats(false)
	p.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return p, p.Stats
}

// r59Redirect runs the real response pass over a 302, with the request state the
// self-redirect guard reads.
func r59Redirect(t *testing.T, p *Proxy, path, loc string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "https://"+r58Variant+path, nil)
	site, ok := p.Map.SiteForHost(r58Variant)
	if !ok {
		t.Fatalf("fixture: %s is not in the map", r58Variant)
	}
	st := &state{site: site, url: "https://" + r58Variant + req.URL.RequestURI()}
	req = req.WithContext(context.WithValue(req.Context(), stateKey, st))
	resp := &http.Response{
		StatusCode: http.StatusFound,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}
	resp.Header.Set("Location", loc)
	if err := p.modifyResponse(resp); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Location")
}

// A cookie named `domain` is a cookie, not a Domain= attribute.
//
// RFC 6265 §4.1.1: the first `;`-part of a Set-Cookie is the cookie's own
// name=value and every later one is an attribute. Reading the first as an
// attribute dropped the whole pair, so the browser was handed a cookie named
// after whatever attribute happened to follow — the session it was carrying
// simply gone, on a preview whose entire job is to look like production.
func TestR59TheCookieNameIsNotAnAttribute(t *testing.T) {
	p, _ := r59Proxy(t)
	for _, c := range []struct{ in, want string }{
		// The name is `domain`, and the value is a canonical host: both of the
		// things that made the old reading fire.
		{"domain=www.example.fi; Path=/; HttpOnly", "domain=www.example.fi; Path=/; HttpOnly"},
		{"Domain=www.example.fi; Path=/", "Domain=www.example.fi; Path=/"},
		// ...while a real Domain= attribute naming a canonical still goes.
		{"sess=abc; Domain=www.example.fi; Path=/", "sess=abc; Path=/"},
		{"sess=abc; Domain=.www.example.fi; Path=/", "sess=abc; Path=/"},
		// ...and a third party's is left alone.
		{"sess=abc; Domain=cdn.other.example; Path=/", "sess=abc; Domain=cdn.other.example; Path=/"},
	} {
		if got := p.dropCookieDomain(c.in); got != c.want {
			t.Errorf("Set-Cookie %q\n  got  %q\n  want %q", c.in, got, c.want)
		}
	}
}

// The size-cap WARN names the request it belongs to.
//
// `check`'s refusal promises "Which paths:" and points the developer at these
// log lines, and they carried `cap` and `content-type` only — so the answer to
// "which large responses put production origins in the browser" was a
// content-type histogram. Same gap on the request side, where the question is
// which save wrote a variant hostname into the shared database.
func TestR60TheSizeCapWarnNamesThePath(t *testing.T) {
	big := `{"u":"https://www.example.fi/x","pad":"` + strings.Repeat("z", 4096) + `"}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(big))
	}))
	defer up.Close()
	target, _ := url.Parse(up.URL)

	m, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt-a--ex.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	p := &Proxy{
		Upstream: target, Map: m, Stats: rewrite.NewStats(false),
		MaxBody: 128, // below the body above, so the cap fires
		Log:     slog.New(slog.NewTextHandler(&log, nil)),
	}
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	req, _ := http.NewRequest("GET", front.URL+"/wp-json/wp/v2/posts/5", nil)
	req.Host = "wt-a--ex.ddev.site"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	got := log.String()
	if !strings.Contains(got, "size cap") {
		t.Fatalf("fixture: the cap did not fire, so this asserts nothing:\n%s", got)
	}
	for _, want := range []string{"/wp-json/wp/v2/posts/5", "GET"} {
		if !strings.Contains(got, want) {
			t.Errorf("the log promises \"which paths\" and does not carry %q:\n%s",
				want, got)
		}
	}
}

// The self-redirect carve-out is a *response* event.
//
// It is the one enumerated test-28 exemption — an asset the worktree does not
// have, redirected to the canonical on purpose, which redirect-uploads.conf does
// in 87% of the fleet — and it was filed under the surface that means §4.3, a
// variant hostname going into the database. Round 59 moved the guard that
// decides it and left the Record that reports it, and the test written to stop
// exactly that inversion asserts only on the rewrite counters, so it could not
// see a third Record that only ever skips.
func TestR60TheSelfRedirectIsAResponseEvent(t *testing.T) {
	m, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt-a--ex.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	// The shape redirect-uploads.conf produces: a missing asset 302'd to the
	// canonical origin at the same path the browser asked for.
	const path = "/wp-content/uploads/2026/01/missing.jpg"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://www.example.fi"+path)
		w.WriteHeader(http.StatusFound)
	}))
	defer up.Close()
	target, _ := url.Parse(up.URL)

	st := rewrite.NewStats(true)
	var seen []string
	st.OnEvent(func(surface string, e origin.Event) {
		if e.Reason == origin.ReasonSelfRedirect {
			seen = append(seen, surface)
		}
	})
	p := &Proxy{Upstream: target, Map: m, Stats: st,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	req, _ := http.NewRequest("GET", front.URL+path, nil)
	req.Host = "wt-a--ex.ddev.site"
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(seen) == 0 {
		t.Fatalf("fixture: no self-redirect event was recorded, so this asserts nothing")
	}
	for _, surface := range seen {
		if surface != rewrite.SurfaceResponseHeader {
			t.Errorf("a self-redirect — a canonical origin going to the browser — "+
				"was filed under %q, the surface that means a variant hostname "+
				"going into the database", surface)
		}
	}
}
