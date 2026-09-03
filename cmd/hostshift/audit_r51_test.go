package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// Round 51, on 2c6f6a8.

// TestR51TheLiveCrawlGuardFoldsTheBaseAndTheDialerDoesNot.
//
// Round 41 found that `cmdDiff`'s live-crawl guardrail and `corpus.Run`'s
// dialer normalised in opposite directions, and fixed it by routing both
// through `corpus.ResolveKey`:
//
//	_, covered := resolveMap[corpus.ResolveKey(net.JoinHostPort(cb.Hostname(), port))]
//
// with the map itself keyed by the same function. That folds both *keys*. It
// does not fold the thing the dialer is handed:
//
//	if to, ok := o.Resolve[addr]; ok {
//
// where `addr` comes from `net/http`, which punycodes the URL host but leaves
// its case alone (`idnaASCII` returns an ASCII host unchanged, and
// `canonicalAddr` lowercases nothing). diff.go's comment on that line asserts
// the opposite — "net/http hands this the punycode host lowercased" — and that
// is the bug: it is true of the IDN half and false of the case half.
//
// So round 41 closed the direction where `--resolve` is misspelled and left
// open the direction where the *base* is. `--canonical-base https://WWW.ACME.FI`
// with a lowercase `--resolve www.acme.fi:443:127.0.0.1:8443`:
//
//   - the guard folds `WWW.ACME.FI` to `www.acme.fi`, finds the key, says
//     covered, and prints nothing;
//   - the dialer looks up `WWW.ACME.FI:443`, misses, and goes to real DNS.
//
// Under production-canonical the canonical base *is* the client's production
// hostname, and `-n 20` is twenty real requests to the live site — made while
// the one message written to stop that stays silent, which as this command's
// own comment says "reads as confirmation". A guardrail a typo can disable is
// the failure mode both round 41 and the `--resolve` help text exist for; only
// half of it was closed.
//
// The hosts here are under `.invalid` (RFC 2606), so the test never leaves the
// machine, and 127.0.0.1:9 is discard — a redirected fetch fails with "connect:
// connection refused" naming it, one that was not fails with "no such host".
// The destination is read out of the report rather than assumed, so this
// asserts the dialer's behaviour and not the flag's.
//
// The fix is one line: fold the address in the dialer too — or, since the
// comment there argues against folding twice, fold `cb`'s host once where the
// URL is built, so every consumer of it (the dialer, the report line, the
// `Host` header) sees the same spelling. Either way `corpus.ResolveKey` stays
// the single definition of the fold.
func TestR51TheLiveCrawlGuardFoldsTheBaseAndTheDialerDoesNot(t *testing.T) {
	const local = "127.0.0.1:9"
	const resolve = "www.example.invalid:443:" + local

	cases := map[string]string{
		// The control: round 41's spelling, which works.
		"a lowercase base":      "https://www.example.invalid",
		"an upper-cased base":   "https://WWW.EXAMPLE.INVALID",
		"a mixed-case base":     "https://WWW.Example.Invalid",
		"a base with one shift": "https://Www.example.invalid",
	}

	var redirected, notRedirected int
	for name, base := range cases {
		_, out, errOut := run(t, "", cmdDiff,
			"--canonical-base", base,
			"--from", "https://www.example.fi", "--to", "https://wt-a--client.ddev.site",
			"-n", "1", "--timeout", "3s", "--resolve", resolve)

		warned := strings.Contains(errOut, "not pointed anywhere local")
		wentLocal := strings.Contains(out, local)
		wentToDNS := strings.Contains(out, "no such host")
		if wentLocal == wentToDNS {
			t.Fatalf("%s: the crawl's destination is not legible, so this test would "+
				"assert nothing:\nstdout:\n%s\nstderr:\n%s", name, out, errOut)
		}
		if wentLocal {
			redirected++
		} else {
			notRedirected++
		}

		// Every case here names the host being crawled, differing from it only
		// in case — which is not a difference in a hostname.
		if !wentLocal {
			t.Errorf("%s: --resolve %q names the host --canonical-base %s crawls, so "+
				"the fetch should have gone to %s; it fell through to real DNS, which "+
				"under production-canonical is the client's live site\nstdout:\n%s",
				name, resolve, base, local, out)
		}
		// And the guardrail must not report containment it did not get.
		if !warned && !wentLocal {
			t.Errorf("%s: the crawl was NOT pointed anywhere local and hostshift said "+
				"nothing, so the silence reads as confirmation that the live site was "+
				"not fetched\nstderr:\n%s\nstdout:\n%s", name, errOut, out)
		}
	}

	if redirected == 0 {
		t.Fatalf("no case in this table was redirected (%d/%d), so the fixture no "+
			"longer exercises --resolve at all", redirected, notRedirected)
	}
}

// TestR51TheReplacedMapIsWhatTheLeakScanActuallyUses.
//
// Round 50's LARGE was that `diff`'s bases moved the crawl and not the map, so
// the leak scan hunted an origin that could not occur and printed `0 leaks`
// over pages full of them. 2c6f6a8 fixed it in two halves: a notice on stderr,
// and the assignment behind it —
//
//	res.Map = m
//
// Only the notice has a test. Deleting that one line leaves `go test ./...` and
// `test/addon-command.sh` both green, and the run then prints
//
//	neither base is in the map … and that is what the leak scan looks for
//
// while the leak scan looks at the map from `--slug`, exactly as it did before
// the fix. That is worse than the state round 50 found, because the message now
// asserts the thing that is false. A fix whose behaviour no test observes is a
// fix a refactor removes silently.
//
// This asserts the behaviour: a variant page carrying the canonical base's own
// origin must be counted as a leak. Both bases are pinned to one local
// httptest server with `--resolve`, so nothing leaves the machine and the two
// fetches return the same bytes; what is being measured is which hostname the
// scan was looking for.
func TestR51TheReplacedMapIsWhatTheLeakScanActuallyUses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<a href="http://acme.ddev.site/x">canonical</a>`)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme-wt-a\n")
	writeFile(t, dir, "paths.txt", "/\n")

	_, out, errOut := run(t, "", cmdDiff,
		"-C", dir, "--slug", "wt-a",
		"--paths", filepath.Join(dir, "paths.txt"),
		"--canonical-base", "http://acme.ddev.site",
		"--variant-base", "http://wt-a--acme.ddev.site",
		"--resolve", "acme.ddev.site:80:"+addr,
		"--resolve", "wt-a--acme.ddev.site:80:"+addr,
		"--timeout", "5s")

	if strings.Contains(out, "errors") && !strings.Contains(out, "0 errors") {
		t.Fatalf("the fetches did not reach the local server, so this asserts nothing:\n%s\n%s", out, errOut)
	}
	if !strings.Contains(errOut, "neither base is in the map") {
		t.Fatalf("the bases-are-the-map branch did not fire, so this fixture no longer "+
			"tests it:\n%s", errOut)
	}
	if strings.Contains(out, " 0 leaks,") {
		t.Errorf("the variant page carries http://acme.ddev.site — the canonical base "+
			"this run announced it was comparing against — and the scan reported no "+
			"leak, so it was still looking for the hostnames --slug derived:\n%s\n%s",
			errOut, out)
	}
}

// TestR51TheUnrewritableTypeNoticeFiresOnTheTypesItRewrites.
//
// 2c6f6a8 added a line to `cmdRewrite` for the case where `--type` names
// something outside the rewritable set — "a typo looks exactly like a clean
// result otherwise". It was added *after* the `switch`, and the switch's
// rewriting arms only assign `src`; none of them returns. So the notice runs on
// every path:
//
//	$ echo '<a href="https://www.acme.fi/x">y</a>' | hostshift rewrite --type text/html …
//	hostshift: --type text/html is outside the rewritable set, so the input is passed through unchanged.
//	  Rewritten types: text/html, application/xhtml+xml, the JSON family, …
//	<a href="https://wt-a--acme.ddev.site/x">y</a>
//	rewrites by surface:
//	  html-attr                1
//
// text/html is the *default* `--type`, so the plain invocation in the README
// now denies doing the thing it just did, and names itself in the list of types
// it claims not to be in, two lines later.
//
// It also breaks `--json`, documented as "emit counters as JSON on stderr":
// three lines of prose now precede the object on that stream, so anything
// piping stderr to a parser gets a syntax error. The add-on's own leak check
// pipes `--dry-run --json 2>&1 >/dev/null` into awk and survives only because
// its pattern is `"rewrites"` and the prose does not contain it.
//
// The notice belongs inside the switch's default — or the rewriting arms need
// to return — so it fires exactly when nothing rewrote.
func TestR51TheUnrewritableTypeNoticeFiresOnTheTypesItRewrites(t *testing.T) {
	const in = `<a href="https://www.acme.fi/x">y</a>`
	mapFlag := "https://www.acme.fi=https://wt-a--acme.ddev.site"

	for _, mt := range []string{
		"text/html", "application/xhtml+xml", "application/json",
		"text/plain", "application/rss+xml", "image/svg+xml",
	} {
		body := in
		if mt == "application/json" {
			body = `{"u":"https://www.acme.fi/x"}`
		}
		code, out, errOut := run(t, body, cmdRewrite, "--map", mapFlag, "--type", mt)
		if code != exitOK {
			t.Fatalf("--type %s: exit %d\n%s", mt, code, errOut)
		}
		if out == body {
			t.Fatalf("--type %s did not rewrite, so this fixture no longer tests the "+
				"claim:\n  %s", mt, out)
		}
		if strings.Contains(errOut, "outside the rewritable set") {
			t.Errorf("--type %s rewrote the input and said it was \"outside the "+
				"rewritable set, so the input is passed through unchanged\":\n"+
				"  stdout: %s\n  stderr: %s", mt, out, errOut)
		}
	}

	// And --json must still be JSON on stderr, which is what it is for.
	_, _, errOut := run(t, in, cmdRewrite, "--map", mapFlag, "--type", "text/html", "--json")
	trimmed := strings.TrimLeft(errOut, " \t\r\n")
	if !strings.HasPrefix(trimmed, "{") {
		t.Errorf("--json is documented as counters as JSON on stderr, and stderr does "+
			"not start with an object:\n%s", errOut)
	}

	// The type the notice was written for still gets it — a fix must not be
	// "delete the message".
	_, _, errOut = run(t, "body{}", cmdRewrite, "--map", mapFlag, "--type", "text/css")
	if !strings.Contains(errOut, "outside the rewritable set") {
		t.Errorf("text/css is outside the rewritable set and nothing said so:\n%s", errOut)
	}
}
