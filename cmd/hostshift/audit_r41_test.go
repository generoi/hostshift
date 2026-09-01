package main

import (
	"strings"
	"testing"
)

// The live-crawl guardrail and the dialer must agree about what `--resolve`
// covers, and they normalise in opposite directions.
//
// cmdDiff decides whether to warn with:
//
//	if ok && strings.EqualFold(h, cb.Hostname()) && p == port {
//
// while corpus.Run's DialContext decides whether the fetch is redirected with
// an exact map lookup:
//
//	if to, ok := o.Resolve[addr]; ok {
//
// where `addr` is what net/http hands the dialer — which is the *punycode*
// host (Transport calls idnaASCII on the URL host before it builds the connect
// address) with its case preserved.
//
// So the guard folds case and the dialer does not; the dialer folds IDNA and
// the guard does not. Both directions are wrong, and one of them is the
// direction that matters: the guard reports "covered" and stays silent while
// the crawl falls through to real DNS and fetches -n pages from the client's
// live production site. That is what HEAD's own commit message calls the
// unacceptable failure — "a guardrail a typo can disable is worse than none,
// because it reads as confirmation" — for the two spellings it did check, and
// it is still true for these two.
//
// Measured against `www.hämeenlinna.fi` on the machine this was written: with
// `--resolve www.hämeenlinna.fi:443:127.0.0.1:9` hostshift printed no warning
// and the canonical fetch returned `status 301` from the live site. The hosts
// here are under `.invalid` (RFC 2606, guaranteed NXDOMAIN) so the test itself
// never leaves the machine.
//
// The assertion is the invariant rather than a table of expected verdicts: the
// warning must be silent exactly when the fetch was in fact redirected. A
// spelling that silences the warning without redirecting the fetch is the leak;
// a spelling that redirects the fetch and warns anyway is the false alarm that
// teaches a developer to scroll past it.
func TestTheLiveCrawlWarningDescribesWhereTheCrawlActuallyGoes(t *testing.T) {
	const variant = "https://wt-a--client.ddev.site"
	// 127.0.0.1:9 is discard: a redirected fetch fails with "connect:
	// connection refused" naming that address, and one that was not redirected
	// fails with "no such host". The two are distinguishable in the report,
	// which is what makes this test able to check the dialer rather than
	// assume it.
	const local = "127.0.0.1:9"

	cases := map[string]struct{ base, resolve string }{
		// The control. Lowercase ASCII, right host, right port: this is the
		// spelling the existing test covers, and it works.
		"a plain host": {
			"https://www.example.invalid",
			"www.example.invalid:443:" + local,
		},
		// An IDN canonical, written the way a person writes it — and the way
		// PLAN's own prose writes .fi client domains. net/http punycodes it
		// before dialling, so this key never matches.
		"an IDN spelled in unicode": {
			"https://www.hämeenlinna.invalid",
			"www.hämeenlinna.invalid:443:" + local,
		},
		// The same IDN spelled the way the dialer will ask for it. This one
		// does redirect the fetch, and the guard warns anyway.
		"an IDN spelled in punycode": {
			"https://www.hämeenlinna.invalid",
			"www.xn--hmeenlinna-q5a.invalid:443:" + local,
		},
		// A hostname copied out of somewhere that upper-cased it. EqualFold in
		// the guard says covered; the exact map lookup in the dialer does not.
		"a host in a different case": {
			"https://www.example.invalid",
			"WWW.EXAMPLE.INVALID:443:" + local,
		},
		// The other branch of the invariant, so the table is not degenerate:
		// a --resolve that genuinely does not cover this crawl. Every spelling
		// above is a *misspelling* of the right host, and once those are folded
		// they all redirect — leaving nothing to exercise the "warned, and
		// rightly" side. This is the curl mistake the guard exists for.
		"a --resolve for another host entirely": {
			"https://www.example.invalid",
			"other.invalid:443:" + local,
		},
	}

	// Which cases *should* land locally. The invariant below — silent iff the
	// crawl went local — is true of a build where the guard and the dialer are
	// both wrong in the same way, so it cannot be the only assertion: with the
	// resolve map keyed on the raw spelling, the IDN case goes to DNS *and*
	// warns, and the invariant holds while the flag does nothing.
	shouldLandLocal := map[string]bool{
		"an IDN spelled in unicode":  true,
		"an IDN spelled in punycode": true,
		"a host in a different case": true,
	}

	var redirected, notRedirected int
	for name, c := range cases {
		code, out, errOut := run(t, "", cmdDiff,
			"--canonical-base", c.base,
			"--from", "https://www.example.fi", "--to", variant,
			"-n", "1", "--timeout", "3s", "--resolve", c.resolve)
		_ = code

		warned := strings.Contains(errOut, "not pointed anywhere local")
		// Where the fetch actually went. Not inferred from the flag — read out
		// of the failure the crawl reported.
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
		if want, ok := shouldLandLocal[name]; ok && wentLocal != want {
			t.Errorf("%s: --resolve %q names the host being crawled, so the fetch "+
				"should have gone to %s; it went to real DNS instead, which means "+
				"the flag silently did nothing\nstdout:\n%s", name, c.resolve, local, out)
		}

		// The invariant. Silent means "this crawl is pointed somewhere local",
		// and that has to be true.
		if warned == wentLocal {
			if wentLocal {
				t.Errorf("%s: --resolve %q sent the crawl to %s and hostshift warned "+
					"that it was \"not pointed anywhere local\" anyway — a false alarm "+
					"on the one message that must stay worth reading\nstderr:\n%s",
					name, c.resolve, local, errOut)
			} else {
				t.Errorf("%s: --resolve %q did NOT cover the crawl — it fell through to "+
					"real DNS — and hostshift printed no warning, so under "+
					"production-canonical it would have fetched the client's live site "+
					"while reading as confirmation that it had not\nstdout:\n%s",
					name, c.resolve, out)
			}
		}
	}

	// Not a degenerate table: both outcomes are present, so neither branch of
	// the invariant above is unreachable.
	if redirected == 0 || notRedirected == 0 {
		t.Fatalf("the table exercised only one outcome (%d redirected, %d not), "+
			"so the invariant was only half-tested", redirected, notRedirected)
	}
}
