package main

import (
	"strings"
	"testing"
)

// Round 46, on 598de7c.
//
// 598de7c widened `diff`'s unmatched-base warning from multisite-only to
// "multisite, or a map with no external canonicals":
//
//	if !matched && (len(res.Map.Sites) > 1 || len(res.ExternalCanonicals) == 0)
//
// ExternalCanonicals is non-empty exactly under production-canonical, which is
// the worktree deployment the README documents — so on the commonest single-site
// map the warning is suppressed for *both* flags. The commit's own comment says
// why that is wrong for one of them:
//
//	"The asymmetry had no reason behind it: the argument for tolerating an
//	 unmatched base is about the canonical side, where the production-canonical
//	 baseline is the project's own <project>.ddev.site and deliberately not in
//	 the map. Every variant is in the map by construction."
//
// A `--variant-base` naming a host no site declares is therefore always worth
// saying out loud, and here it is silent.
func TestR46VariantBaseUnmatchedIsSilentOnAProductionCanonicalSite(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme\n")
	writeFile(t, dir, "hostshift.yaml", ""+
		"sites:\n"+
		"  - canonical: https://www.acme.fi\n    variant: https://wt-a--acme.ddev.site\n")

	args := append([]string{"-C", dir, "--slug", "wt-a", "-n", "1",
		"--variant-base", "https://wt-b--acme.ddev.site"},
		noNetwork("www.acme.fi", "wt-b--acme.ddev.site")...)
	_, _, errOut := run(t, "", cmdDiff, args...)
	if !strings.Contains(errOut, "--variant-base") {
		t.Errorf("a --variant-base that names no site of the map went unwarned, and\n"+
			"the run compared https://www.acme.fi against a host the map does not know:\n%s",
			errOut)
	}
}
