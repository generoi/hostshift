package main

import (
	"strings"
	"testing"
)

// TestSubcommandHelpSaysWhatTheSubcommandIsFor. `hostshift check --help`
// printed nine flags, not one word about what it checks, and exited 2 — so a
// reader who wanted help got a failure and no explanation.
func TestSubcommandHelpSaysWhatTheSubcommandIsFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    func([]string) (int, error)
	}{
		{"rewrite", cmdRewrite},
		{"proxy", cmdProxy},
		{"hosts", cmdHosts},
		{"map", cmdMap},
		{"check", cmdCheck},
		{"diff", cmdDiff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, _ := run(t, "", tc.f, "--help")
			if code != exitOK {
				t.Errorf("exit %d; asking for help is not a failure", code)
			}
			// stdout, so `hostshift map --help > notes.txt` is not empty.
			if !strings.HasPrefix(out, "hostshift "+tc.name+" — ") {
				t.Errorf("stdout does not describe the subcommand: %q", out)
			}
			if !strings.Contains(out, "usage: hostshift "+tc.name) {
				t.Errorf("stdout has no usage line: %q", out)
			}
		})
	}
}
