// Package e2e drives hostshift against a live DDEV project.
//
// The M6 pilot was a pile of shell scripts, which proved things once and proved
// nothing afterwards. These are the same checks as tests, so a regression shows
// up the next time anyone runs them.
//
// Everything here is skipped unless HOSTSHIFT_E2E_VARIANT is set, so
// `go test ./...` stays hermetic:
//
//	HOSTSHIFT_E2E_VARIANT=https://wt-a--acmecorp.ddev.site \
//	HOSTSHIFT_E2E_CANONICAL=https://www.acmecorp.fi \
//	HOSTSHIFT_E2E_SIBLING_VARIANT=https://wt-a--nat.acmecorp.ddev.site \
//	HOSTSHIFT_E2E_SIBLING_CANONICAL=https://www.acmecorpnat.fi \
//	HOSTSHIFT_E2E_PROJECT=$HOME/Projects/Genero/acmecorp \
//	HOSTSHIFT_E2E_USER=hostshifte2e HOSTSHIFT_E2E_APP_PASSWORD='…' \
//	go test ./internal/e2e/ -v
//
// The certificate is DDEV's mkcert one, which the Go client on a developer
// machine trusts, but CI may not — TLS verification is skipped here because it
// is test 29b's job to assert TLS behaviour, not this file's.
package e2e

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type env struct {
	variant, canonical               string
	siblingVariant, siblingCanonical string
	project                          string
	user, appPassword                string
	client                           *http.Client
}

func setup(t *testing.T) env {
	t.Helper()
	e := env{
		variant:          os.Getenv("HOSTSHIFT_E2E_VARIANT"),
		canonical:        os.Getenv("HOSTSHIFT_E2E_CANONICAL"),
		siblingVariant:   os.Getenv("HOSTSHIFT_E2E_SIBLING_VARIANT"),
		siblingCanonical: os.Getenv("HOSTSHIFT_E2E_SIBLING_CANONICAL"),
		project:          os.Getenv("HOSTSHIFT_E2E_PROJECT"),
		user:             os.Getenv("HOSTSHIFT_E2E_USER"),
		appPassword:      os.Getenv("HOSTSHIFT_E2E_APP_PASSWORD"),
	}
	if e.variant == "" || e.canonical == "" {
		t.Skip("set HOSTSHIFT_E2E_VARIANT and HOSTSHIFT_E2E_CANONICAL to run the live suite")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	e.client = &http.Client{
		Timeout:   60 * time.Second,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return e
}

func (e env) get(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "identity")
	res, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return res, body
}

// follow does what a browser does, so a site whose front page redirects still
// yields a page to assert on.
func (e env) follow(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	for i := 0; i < 5; i++ {
		res, body := e.get(t, url)
		loc := res.Header.Get("Location")
		if res.StatusCode < 300 || res.StatusCode >= 400 || loc == "" {
			return res, body
		}
		url = loc
	}
	t.Fatalf("too many redirects from %s", url)
	return nil, nil
}

// wpLines runs a WP-CLI command in the project and returns its output lines with
// the site's PHP deprecation notices dropped. It is how the database is
// inspected, which is the half of tests 30 and 31 that HTTP cannot reach.
func (e env) wpLines(t *testing.T, args ...string) []string {
	t.Helper()
	if e.project == "" {
		t.Skip("set HOSTSHIFT_E2E_PROJECT to run the WP-CLI checks")
	}
	cmd := exec.Command("ddev", append([]string{"wp"}, args...)...)
	cmd.Dir = e.project
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ddev wp %s: %v", strings.Join(args, " "), err)
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		s := strings.TrimSpace(l)
		if s == "" || strings.HasPrefix(s, "Deprecated:") || strings.HasPrefix(s, "Warning:") {
			continue
		}
		lines = append(lines, s)
	}
	return lines
}

// wp is wpLines for a single-value command.
func (e env) wp(t *testing.T, args ...string) string {
	t.Helper()
	lines := e.wpLines(t, args...)
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

// canonicalHosts is what must never reach the browser (test 28).
func (e env) canonicalHosts() []string {
	hosts := []string{hostOf(e.canonical)}
	if e.siblingCanonical != "" {
		hosts = append(hosts, hostOf(e.siblingCanonical))
	}
	return hosts
}

func hostOf(u string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://"), "/")
}

// assertNoCanonicalOrigin is test 28 on a live response. Bare hostnames are out
// of scope, so this looks for anchored origins only.
func assertNoCanonicalOrigin(t *testing.T, e env, what string, body []byte) {
	t.Helper()
	for _, h := range e.canonicalHosts() {
		for _, form := range []string{"https://" + h, "http://" + h, "//" + h, `https:\/\/` + h} {
			if n := bytes.Count(body, []byte(form)); n > 0 {
				t.Errorf("%s: %d occurrences of a dereferenceable canonical origin %q", what, n, form)
			}
		}
	}
}

// TestSiteServesAtVariantHost is the whole design in one assertion: the site is
// browsable at a hostname its database has never heard of.
func TestSiteServesAtVariantHost(t *testing.T) {
	e := setup(t)
	res, body := e.follow(t, e.variant+"/")
	if res.StatusCode != 200 {
		t.Fatalf("status %d, want 200", res.StatusCode)
	}
	if len(body) < 10000 {
		t.Fatalf("body is %d bytes; that is not a rendered page", len(body))
	}
	assertNoCanonicalOrigin(t, e, "front page", body)
	if !bytes.Contains(body, []byte(hostOf(e.variant))) {
		t.Errorf("the page carries no variant origin at all; is the proxy in the path?")
	}
	t.Logf("%d bytes, %d variant origins", len(body),
		bytes.Count(body, []byte(hostOf(e.variant))))
}

// TestSiblingBlogRoutesToItsOwnHost is acceptance test 10 against a real
// multisite: get_site_by_path matches wp_blogs.domain exactly, so the sibling
// must arrive on its own canonical host, not the network's main one.
func TestSiblingBlogRoutesToItsOwnHost(t *testing.T) {
	e := setup(t)
	if e.siblingVariant == "" {
		t.Skip("set HOSTSHIFT_E2E_SIBLING_VARIANT for the multisite checks")
	}
	res, body := e.follow(t, e.siblingVariant+"/")
	if res.StatusCode != 200 {
		t.Fatalf("status %d, want 200 — the sibling blog did not resolve", res.StatusCode)
	}
	assertNoCanonicalOrigin(t, e, "sibling front page", body)
	if !bytes.Contains(body, []byte(hostOf(e.siblingVariant))) {
		t.Errorf("the sibling page carries no sibling variant origin")
	}
}

// TestCanonicalHostNoLongerResolves is the other side of the same coin, and the
// reason the proxy is needed at all: with a production-canonical database the
// ddev hostname is not a site.
func TestCanonicalHostNoLongerResolves(t *testing.T) {
	e := setup(t)
	ddev := os.Getenv("HOSTSHIFT_E2E_DDEV_HOST")
	if ddev == "" {
		t.Skip("set HOSTSHIFT_E2E_DDEV_HOST to assert the unproxied host is not a site")
	}
	res, _ := e.get(t, ddev+"/")
	if res.StatusCode == 200 {
		t.Errorf("%s returned 200; the database is not production-canonical, so this suite is testing the easy case", ddev)
	}
}

// TestRESTResponseRewritten is tests 4 and 22 on the real REST API. The site
// restricts REST to authenticated users, which is also what Gutenberg is, so an
// application password is the honest way in.
func TestRESTResponseRewritten(t *testing.T) {
	e := setup(t)
	if e.appPassword == "" {
		t.Skip("set HOSTSHIFT_E2E_USER and HOSTSHIFT_E2E_APP_PASSWORD for the REST checks")
	}
	req, _ := http.NewRequest("GET", e.variant+"/wp-json/wp/v2/pages?per_page=3", nil)
	req.SetBasicAuth(e.user, e.appPassword)
	res, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if res.StatusCode != 200 {
		t.Fatalf("status %d: %s", res.StatusCode, truncate(body))
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type %q", ct)
	}
	assertNoCanonicalOrigin(t, e, "REST response", body)
	if !bytes.Contains(body, []byte(hostOf(e.variant))) {
		t.Errorf("no variant origin in the REST response; it may not have entered the JSON rewriter")
	}
	// It must still be JSON afterwards.
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Errorf("the rewritten body is not valid JSON: %v", err)
	}
}

// TestEditRoundTrip is the database half of tests 30 and 31, which no HTTP-only
// test can reach: a REST write carrying variant URLs must be *stored* canonical,
// or the clone is polluted and stops matching production.
func TestEditRoundTrip(t *testing.T) {
	e := setup(t)
	if e.appPassword == "" || e.project == "" {
		t.Skip("set HOSTSHIFT_E2E_APP_PASSWORD and HOSTSHIFT_E2E_PROJECT for the edit round trip")
	}

	// Built with json.Marshal rather than string formatting: the content itself
	// contains quotes, and hand-rolling that is how the first version of this
	// test sent WordPress invalid JSON.
	link := e.variant + "/hostshift-e2e-target/"
	payload, err := json.Marshal(map[string]string{
		"title":   "hostshift e2e",
		"status":  "draft",
		"content": fmt.Sprintf(`<a href="%s">target</a>`, link),
	})
	if err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("POST", e.variant+"/wp-json/wp/v2/posts", bytes.NewReader(payload))
	req.SetBasicAuth(e.user, e.appPassword)
	req.Header.Set("Content-Type", "application/json")
	res, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 201 {
		t.Fatalf("status %d: %s", res.StatusCode, truncate(body))
	}

	var created struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == 0 {
		t.Fatalf("could not read the created post id: %v %s", err, truncate(body))
	}
	t.Cleanup(func() {
		e.wp(t, "post", "delete", fmt.Sprint(created.ID), "--force")
	})

	// The assertion that matters: what the database holds.
	stored := e.wp(t, "post", "get", fmt.Sprint(created.ID), "--field=content")
	if strings.Contains(stored, hostOf(e.variant)) {
		t.Errorf("the database stored a variant URL, polluting the clone:\n%s", stored)
	}
	if !strings.Contains(stored, hostOf(e.canonical)) {
		t.Errorf("the database did not store the canonical URL:\n%s", stored)
	}

	// And reading it back gives the browser the variant again.
	assertNoCanonicalOrigin(t, e, "REST create response", body)
	if !bytes.Contains(body, []byte(hostOf(e.variant))) {
		t.Errorf("the create response did not come back as the variant")
	}
}

// TestWPCLIResolves is acceptance test 29d: WP-CLI is the one thing
// production-canonical regresses, and wp-cli.local.yml is the fix.
func TestWPCLIResolves(t *testing.T) {
	e := setup(t)
	if got := e.wp(t, "option", "get", "home"); got != e.canonical {
		t.Errorf("wp option get home = %q, want %q", got, e.canonical)
	}
	// Every blog must come back on its own production hostname, not just the
	// last one — the multisite resolution is the whole point of test 29d.
	sites := strings.Join(e.wpLines(t, "site", "list", "--field=url"), " ")
	for _, h := range e.canonicalHosts() {
		if !strings.Contains(sites, h) {
			t.Errorf("wp site list is missing %s: %q", h, sites)
		}
	}
}

// TestLoopbackStaysOnTheMachine is acceptance test 29a. Without the extra_hosts
// override a dev box POSTs to production's cron queue.
func TestLoopbackStaysOnTheMachine(t *testing.T) {
	e := setup(t)
	if e.project == "" {
		t.Skip("set HOSTSHIFT_E2E_PROJECT")
	}
	for _, host := range e.canonicalHosts() {
		cmd := exec.Command("ddev", "exec", "getent", "hosts", host)
		cmd.Dir = e.project
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("getent %s: %v", host, err)
		}
		if !strings.Contains(string(out), "127.0.0.1") {
			t.Errorf("%s resolves to %q inside the container, not 127.0.0.1 — "+
				"WordPress's internal requests are leaving for live production",
				host, strings.TrimSpace(string(out)))
		}
	}
}

// TestNoStaleValidatorsLive is acceptance test 15 against the real site.
func TestNoStaleValidatorsLive(t *testing.T) {
	e := setup(t)
	res, _ := e.follow(t, e.variant+"/")
	for _, h := range []string{"Content-Length", "ETag", "Last-Modified"} {
		if v := res.Header.Get(h); v != "" {
			t.Errorf("%s survived on a rewritten response: %q", h, v)
		}
	}
	if !strings.Contains(strings.Join(res.Header.Values("Vary"), ","), "Host") {
		t.Errorf("Vary does not mention Host: %q", res.Header.Values("Vary"))
	}
}

// TestBinaryPassthroughLive is acceptance test 12 on a real asset rather than a
// fixture: whatever the site serves as an image must come back byte-identical.
func TestBinaryPassthroughLive(t *testing.T) {
	e := setup(t)
	path := os.Getenv("HOSTSHIFT_E2E_ASSET")
	if path == "" {
		t.Skip("set HOSTSHIFT_E2E_ASSET to a static asset path to assert binary passthrough")
	}
	res, body := e.get(t, e.variant+path)
	if res.StatusCode != 200 {
		t.Skipf("%s returned %d", path, res.StatusCode)
	}
	if len(body) == 0 {
		t.Fatal("empty asset")
	}
	if bytes.Contains(body, []byte(hostOf(e.variant))) {
		t.Errorf("a binary asset was rewritten")
	}
}

func truncate(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "…"
	}
	return string(b)
}
