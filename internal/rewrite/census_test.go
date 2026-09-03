package rewrite

import (
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// The proxy is a long-lived process with no end to print a report at, so
// `--explain` set a flag, filled a buffer nothing ever read, and printed
// nothing. `check` told a developer mid test-28 incident to add the flag,
// restart every container in the project and grep the log for "rewrote" — a
// word the proxy never writes. This hook is what makes that instruction true,
// so it needs a test of its own; the remedy is only as good as the callback.
func TestStatsOnEventFiresForEveryEvent(t *testing.T) {
	st := NewStats(false) // deliberately not explain: --dry-run uses it too
	var got []origin.Event
	var surfaces []string
	st.OnEvent(func(surface string, e origin.Event) {
		surfaces = append(surfaces, surface)
		got = append(got, e)
	})
	st.Record(SurfaceHeader, 100, []origin.Event{
		{Offset: 5, Action: origin.ActionRewrote, Text: "www.example.fi"},
		{Offset: 9, Action: origin.ActionSkipped, Reason: origin.ReasonNotAURL},
	})
	if len(got) != 2 {
		t.Fatalf("the hook saw %d event(s), want 2 — a census nothing reports is "+
			"the defect this exists to fix", len(got))
	}
	// The base is added, because an offset the caller cannot locate in the
	// response is not a diagnosis.
	if got[0].Offset != 105 || got[1].Offset != 109 {
		t.Errorf("offsets %d/%d, want 105/109 — the base was not applied",
			got[0].Offset, got[1].Offset)
	}
	if surfaces[0] != SurfaceHeader {
		t.Errorf("surface %q, want %q", surfaces[0], SurfaceHeader)
	}
	if got[0].Action != origin.ActionRewrote || got[1].Action != origin.ActionSkipped {
		t.Errorf("actions %q/%q — skips have to reach it too, or `--explain`'s "+
			"stated job of tracing candidates that did *not* rewrite is undone",
			got[0].Action, got[1].Action)
	}
	// And a nil hook is the ordinary path.
	NewStats(false).Record(SurfaceHeader, 0, []origin.Event{{Action: origin.ActionRewrote}})
}
