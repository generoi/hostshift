package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// The census wiring, which nothing pinned.
//
// Round 59's mutation survey deleted the whole hook, silenced it to Debug and
// hardcoded its surface field, and every test stayed green — on the feature that
// exists so `check`'s instruction at a test-28 refusal ("add --explain, restart,
// `| grep census`") is true. `--dry-run`'s stated behaviour was unpinned the
// same way.
func TestCensusIsWiredForBothFlags(t *testing.T) {
	for _, c := range []struct {
		explain, dryRun, want bool
	}{
		{false, false, false},
		{true, false, true},
		// --dry-run's own help says it logs every rewrite it would have made.
		{false, true, true},
		{true, true, true},
	} {
		if got := wantCensus(c.explain, c.dryRun); got != c.want {
			t.Errorf("wantCensus(explain=%v, dryRun=%v) = %v, want %v",
				c.explain, c.dryRun, got, c.want)
		}
	}
}

// And what it writes, at a level the default handler emits.
func TestCensusHookWritesAGreppableLine(t *testing.T) {
	var buf bytes.Buffer
	// The default handler: Debug is dropped by it, which is what makes the
	// level part of the contract rather than a detail.
	log := slog.New(slog.NewTextHandler(&buf, nil))
	censusHook(log)(("html-attr"), origin.Event{
		Action: origin.ActionRewrote, Offset: 42, Text: "https://www.example.fi",
	})
	got := buf.String()
	if !strings.Contains(got, "census") {
		t.Errorf("the line `check` tells the developer to grep for is not in it:\n  %s", got)
	}
	for _, want := range []string{"html-attr", "rewrote", "42", "www.example.fi"} {
		if !strings.Contains(got, want) {
			t.Errorf("the census line does not carry %q, so it cannot answer "+
				"which surface a leak was on:\n  %s", want, got)
		}
	}
}
