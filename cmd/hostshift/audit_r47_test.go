package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// Round 47.
//
// 1e4f62f paired the diff's bases by site because "--canonical-base <site 2>
// was crawled against site 1's variant, every page differing for the obvious
// reason, and the run printed GREEN". 4e8c68e widened the unmatched-base warning
// to fire for --variant-base too. Both are about a base the map does not know.
//
// The pairing asks whether it knows one with strings.EqualFold(h, u.Host), where
// h is origin.Origin.Host — "lowercase, punycode, no trailing dot" — and u.Host
// is whatever url.Parse kept of what the developer typed. Those are the same
// string for an ASCII hostname and never the same string for an IDN one, so a
// canonical base written the way its owner writes it pairs with nothing.
//
// The command's own --resolve guardrail had exactly this bug and was fixed by
// keying both sides through corpus.ResolveKey — "asking this question a second
// way is what made the guardrail disagree with the dialer". This is the same
// question asked a third way, one flag over. §5.5 calls IDN "real for .fi client
// domains".
func TestR47AnIDNBaseDoesNotPairWithItsOwnSite(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme\n")
	writeFile(t, dir, "hostshift.yaml", ""+
		"sites:\n"+
		"  - canonical: https://www.acme.fi\n    variant: https://wt-a--acme.ddev.site\n"+
		"  - canonical: https://www.hämeenlinna.fi\n    variant: https://wt-a--hml.ddev.site\n")

	args := append([]string{"-C", dir, "--slug", "wt-a", "-n", "1",
		"--canonical-base", "https://www.hämeenlinna.fi"},
		noNetwork("www.acme.fi", "www.hämeenlinna.fi",
			"wt-a--acme.ddev.site", "wt-a--hml.ddev.site")...)
	_, _, errOut := run(t, "", cmdDiff, args...)

	// The ASCII site pairs, so the mechanism works and only the spelling does not.
	ascii := append([]string{"-C", dir, "--slug", "wt-a", "-n", "1",
		"--canonical-base", "https://www.acme.fi"},
		noNetwork("www.acme.fi", "wt-a--acme.ddev.site")...)
	if _, _, ok := run(t, "", cmdDiff, ascii...); !strings.Contains(ok,
		"corpus diff: https://www.acme.fi vs https://wt-a--acme.ddev.site") {
		t.Fatalf("the control case no longer holds, so this test measures nothing:\n%s", ok)
	}

	if strings.Contains(errOut, "is not a canonical of this 2-site map") {
		t.Errorf("the warning says a base that IS a canonical of the map is not one:\n%s", errOut)
	}
	if !strings.Contains(errOut, "vs https://wt-a--hml.ddev.site") {
		t.Errorf("the IDN base was paired with a different site's variant — the "+
			"comparison of unrelated pages the pairing exists to prevent:\n%s", errOut)
	}
}

// TestR47TheLiveSiteWarningCountsWhatWillBeFetched: the sentence exists to stop
// a developer before a crawl of the client's live site, and it printed `-n`
// whatever the run was actually going to fetch — so a `--paths` file of two
// lines warned about twenty pages.
func TestR47TheLiveSiteWarningCountsWhatWillBeFetched(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme\n")
	writeFile(t, dir, "hostshift.yaml",
		"sites:\n  - canonical: https://www.acme.invalid\n    variant: https://wt-a--acme.ddev.site\n")
	writeFile(t, dir, "paths.txt", "/one\n/two\n")

	args := append([]string{"-C", dir, "--slug", "wt-a", "-n", "20",
		"--paths", filepath.Join(dir, "paths.txt")},
		noNetwork("wt-a--acme.ddev.site")...)
	_, _, errOut := run(t, "", cmdDiff, args...)
	if !strings.Contains(errOut, "crawling 2 page(s)") {
		t.Errorf("the warning does not count the supplied paths:\n%s", errOut)
	}
	if strings.Contains(errOut, "crawling 20 page(s)") {
		t.Errorf("the warning still prints -n rather than what will be fetched:\n%s", errOut)
	}
}
