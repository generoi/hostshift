package main

import (
	"strings"
	"testing"
)

// TestHostsNeverPrintsWhatMapWouldReject. `hosts` prints one hostname per line
// and the add-on splits that on whitespace, so a hostname containing a space
// becomes two VIRTUAL_HOST entries. It is reachable: DDEV names an unnamed
// project after its directory, and a directory may be called "My Site". `map`
// rejected the same project with exit 2 while `hosts` exited 0, so the two
// disagreed about one config.
func TestHostsNeverPrintsWhatMapWouldReject(t *testing.T) {
	dir := t.TempDir() + "/My Site"
	writeFile(t, dir, ".ddev/config.yaml", "additional_hostnames:\n  - blog\n")

	code, out, errOut := run(t, "", cmdHosts, "-C", dir)
	if code == 0 {
		t.Fatalf("exit 0 with output %q; map rejects the same project with exit 2", out)
	}
	if !strings.Contains(errOut, "not a hostname") {
		t.Errorf("stderr does not say why: %q", errOut)
	}
}

// A project with ordinary hostnames still prints them, one per line.
func TestHostsPrintsOnePerLine(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: site\nadditional_hostnames:\n  - blog\n")

	code, out, _ := run(t, "", cmdHosts, "-C", dir)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out != "site.ddev.site\nblog.ddev.site\n" {
		t.Errorf("out = %q", out)
	}
}

// TestCheckFailsAnIdentityMap. check documents itself as exiting 2 when the map
// is not usable, and "no rewrite can occur" is the definition of not usable.
// Exiting 0 made it a status line rather than a check.
func TestCheckFailsAnIdentityMap(t *testing.T) {
	code, _, errOut := run(t, "", cmdCheck, "--map", "https://a.example=https://a.example")
	if code != exitConfig {
		t.Errorf("exit %d; want %d", code, exitConfig)
	}
	if !strings.Contains(errOut, "identity map") {
		t.Errorf("stderr = %q", errOut)
	}
}

// TestCheckRejectsAnUnusableUpstream. `check` is the preflight; an upstream the
// proxy cannot dial has to fail here rather than at the next restart.
func TestCheckRejectsAnUnusableUpstream(t *testing.T) {
	code, _, errOut := run(t, "", cmdCheck,
		"--map", "https://a.example=https://b.example", "--upstream", "web:80")
	if code != exitConfig {
		t.Errorf("exit %d; want %d", code, exitConfig)
	}
	if !strings.Contains(errOut, "upstream") {
		t.Errorf("stderr = %q", errOut)
	}
}
