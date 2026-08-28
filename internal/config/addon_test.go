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
	// The compose service, install.yaml, and the one command that scaffolds a
	// project. §5.7 said "only a compose service — no lib.sh, no hooks, no
	// guard", and that still holds for the request path: nothing here runs
	// during a request, generates a guard, or wraps a task that should just
	// work. What the command does is the setup the *binary* deliberately does
	// not do, because scaffolding DDEV is opinionated and hostshift is not a
	// DDEV tool.
	for _, e := range entries {
		if e.Name() == "commands" {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".yaml") {
			t.Errorf("ddev/%s: the add-on is compose files, install.yaml and commands/", e.Name())
		}
	}
	cmds, err := os.ReadDir("../../ddev/commands/host")
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 || cmds[0].Name() != "hostshift" {
		t.Errorf("ddev/commands/host holds %v; one command is the budget", cmds)
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
