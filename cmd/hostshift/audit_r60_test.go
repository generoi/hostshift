package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// The test written to kill the constant-surface mutation cannot see it.
//
// Round 59 extracted `censusHook` because "the wiring was three mutations deep
// in unpinned code, and nothing failed when the hook was deleted, silenced to
// Debug, or given a constant surface", and wrote
// TestCensusHookWritesAGreppableLine against all three. Two of the three are
// dead now. The third is not: that test calls the hook exactly once, with
// `"html-attr"`, and then asserts the line contains `"html-attr"` — which is
// also true of a hook that ignores its argument. Replacing
//
//	log.Info("census", "surface", surface, ...)
//
// with
//
//	log.Info("census", "surface", "html-attr", ...)
//
// leaves `go test ./...` green, on the field the census exists to carry.
//
// It is the field, not a detail of it. `ddev hostshift check` tells a developer
// at a test-28 refusal to turn the census on and grep it, and the answer it is
// there to give is *which surface* — a Location on the way out, or a Referer on
// its way into the shared database, which round 59's own first finding is about
// telling apart. A hook that names one surface for every event answers that
// question wrongly for every event but one.
//
// Two surfaces, because one can never distinguish a variable from a constant.
func TestR60CensusHookNamesTheSurfaceItWasGiven(t *testing.T) {
	line := func(surface string) string {
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, nil))
		censusHook(log)(surface, origin.Event{
			Action: origin.ActionRewrote, Offset: 42,
			Text: "https://www.example.fi",
		})
		return buf.String()
	}
	// The two the census is asked to tell apart: round 59's own finding is that
	// filing one under the other's name sends the developer at a §4.3 write the
	// diagnosis for a test-28 leak, and the reverse.
	for _, surface := range []string{"response-header", "header", "request-body"} {
		got := line(surface)
		if !strings.Contains(got, "surface="+surface) {
			t.Errorf("censusHook was given surface %q and did not write it:\n  %s",
				surface, got)
		}
	}
	// And the pair differs, which is the assertion a single call cannot make:
	// a hook that ignores its argument passes every containment check written
	// against the one value it happens to emit.
	if a, b := line("response-header"), line("header"); a == b {
		t.Errorf("two different surfaces produced the same census line, so the field\n"+
			"`check` sends a developer to cannot distinguish a Location on the way\n"+
			"out from a Referer on its way into the shared database:\n  %s", a)
	}
}
