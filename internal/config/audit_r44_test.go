package config

import (
	"strings"
	"testing"
)

// Round 44, on 7cb756c ("Ask the size question in bytes; stop reports asserting
// what they skipped").

// TestAProductionHostnameInAdditionalFqdnsStillNeedsContaining.
//
// 7cb756c widened the ExternalCanonicals exclusion from one test to three:
//
//	if strings.HasSuffix(o.Host, "."+proj.TLD) || mine[o.Host] ||
//		origin.ResolvesLocally(o.Host) {
//		continue
//	}
//
// The `.test` case the commit was written for is answered by `ResolvesLocally`
// on its own. `mine[o.Host]` — every entry of `proj.Hosts`, which
// `internal/ddev.hostnames` builds by appending `additional_fqdns` verbatim —
// is a separate, wider claim, and it is answered in the wrong scope.
//
// ExternalCanonicals is documented as "the subset that leaves the machine,
// which is the set loopback containment exists for", and loopback containment
// is container-scoped: `ddev/docker-compose.hostshift-loopback.yaml` says so in
// its own header, and adds "Stock DDEV emits no `extra_hosts` on `web`, so
// there is nothing to clobber". What `additional_fqdns` buys is an entry in the
// *host's* /etc/hosts — the add-on's own variant check spends a paragraph on
// that distinction ("DDEV registers only the *exact* FQDN in /etc/hosts") — and
// nothing at all inside web. So a name being one of this project's own
// hostnames says nothing about whether web can reach production on it.
//
// The shape is not hypothetical: the add-on names it as the natural one, at
// ddev/commands/host/hostshift ~line 1256 — "a project that points the
// production hostname at the box with additional_fqdns while adopting
// production-canonical — a natural move".
//
// Two guardrails go quiet on it:
//
//   - `ddev hostshift check` gates its whole loopback-containment block on
//     `[ -n "$external" ]`, where `external` is this list. Empty, the block
//     never runs — so the shipped placeholder `www.example.com` containment
//     file passes, wp-cron POSTs the client's live site, and check says
//     nothing.
//   - `cmdCheck`'s canonical-on-production note is gated on
//     `len(res.ExternalCanonicals) > 0`, so the paragraph explaining that
//     `https://acme.ddev.site` serves the shared production database
//     unrewritten disappears too.
//
// And the commit's stated goal — "so the check and the fix agree on one set" —
// is what breaks: `ddev hostshift loopback` derives its host list from
// `--canonical-hosts` minus the project TLD, so it still emits
// `www.acme.fi:127.0.0.1`. The fix writes a line the check no longer asks for.
func TestAProductionHostnameInAdditionalFqdnsStillNeedsContaining(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, ".ddev/config.yaml", "name: acme\nadditional_fqdns:\n  - www.acme.fi\n")
	writeYAML(t, dir, "hostshift.yaml", ""+
		"sites:\n"+
		"  - canonical: https://www.acme.fi\n"+
		"    variant: https://wt-a--acme.ddev.site\n")

	res, err := Load(dir, Flags{Slug: "wt-a"})
	if err != nil {
		t.Fatal(err)
	}

	// Premise: this really is the production-canonical shape, and the hostname
	// really is one `loopback` would write a containment line for.
	if len(res.DirectlyServed) == 0 {
		t.Fatalf("fixture: nothing is directly served, so this is not the shape under test")
	}
	var canonicalHosts []string
	for _, st := range res.Map.Sites {
		for _, o := range st.CanonicalSet() {
			canonicalHosts = append(canonicalHosts, o.Host)
		}
	}
	if strings.Join(canonicalHosts, ",") != "www.acme.fi" {
		t.Fatalf("fixture: canonical hosts = %v", canonicalHosts)
	}

	found := false
	for _, h := range res.ExternalCanonicals {
		if h == "www.acme.fi" {
			found = true
		}
	}
	if !found {
		t.Errorf("www.acme.fi is a real production hostname and is not in\n"+
			"ExternalCanonicals = %v, so `ddev hostshift check` skips loopback\n"+
			"containment entirely and the canonical-on-production note is\n"+
			"suppressed — while `ddev hostshift loopback` still emits\n"+
			"\"www.acme.fi:127.0.0.1\" from --canonical-hosts. An entry in the\n"+
			"host's /etc/hosts is not an entry in web's.", res.ExternalCanonicals)
	}
}

// TestAnAdditionalHostnameUnderTheProjectTLDIsStillLocal is the other half, and
// it passes today: it is here so the fix for the test above cannot be "drop the
// project's own hostnames from `mine` and warn about everything". A canonical
// under the project's TLD resolves by wildcard to the loopback whether or not
// the project also registers it, and warning about it is the second wrong
// answer the commit message enumerates.
func TestAnAdditionalHostnameUnderTheProjectTLDIsStillLocal(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, ".ddev/config.yaml", "name: acme\nadditional_hostnames:\n  - shop\n")
	writeYAML(t, dir, "hostshift.yaml", ""+
		"sites:\n"+
		"  - canonical: https://shop.ddev.site\n"+
		"    variant: https://wt-a--shop.ddev.site\n")

	res, err := Load(dir, Flags{Slug: "wt-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ExternalCanonicals) != 0 {
		t.Errorf("a canonical under the project TLD was called external: %v",
			res.ExternalCanonicals)
	}
}
