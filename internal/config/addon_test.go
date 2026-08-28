package config

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestAddonYAMLIsValid parses the DDEV add-on's files. A broken compose file is
// only discovered at `ddev start`, on someone else's machine, so it is worth a
// second of CI.
func TestAddonYAMLIsValid(t *testing.T) {
	for _, f := range []string{
		"../../ddev/docker-compose.hostshift.yaml",
		"../../ddev/docker-compose.hostshift-loopback.yaml",
		"../../ddev/install.yaml",
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var v any
		if err := yaml.Unmarshal(b, &v); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

// TestAddonHasNoHooksOrScripts holds the line PLAN §5.7 draws: the add-on is
// "*only* a compose service — no lib.sh, no generated files, no hooks, no
// guard". §3 measured what happens when per-repo footprint is not held to — 42
// repos carrying 14 different pinned SHAs of the same submodule.
func TestAddonHasNoHooksOrScripts(t *testing.T) {
	entries, err := os.ReadDir("../../ddev")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".yaml") {
			t.Errorf("ddev/%s: the add-on is compose files and install.yaml only", e.Name())
		}
	}

	b, err := os.ReadFile("../../ddev/docker-compose.hostshift.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"hooks:", "post-start", "web_extra_hosts", "additional_fqdns"} {
		if strings.Contains(string(b), banned) {
			// additional_fqdns in particular: DDEV manages host-level hosts
			// entries for those, which would point the developer's own machine
			// at the router for a real production domain — worse than the
			// /etc/hosts approach §4.4 already rejected.
			t.Errorf("the compose file contains %q, which the design rules out (PLAN §4.4, §5.7)", banned)
		}
	}
}
