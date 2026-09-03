package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// Round 49, on acc0ae8.

// TestR49TheEventStreamSkipIsRecorded pins the skip acc0ae8 added.
//
// Nothing in the repository covers it: replacing the media type it compares
// against with a string no response can carry leaves `go test ./...` green, so
// the arm is silently revertible and --explain would go back to printing
// nothing at all for the one body class §5.8 accepts as unrewritten.
//
// The charset case is the half worth a test in its own right: WordPress's own
// SSE endpoints send `text/event-stream; charset=utf-8`, and a comparison on
// the raw header rather than on mediaType() would miss every one of them.
func TestR49TheEventStreamSkipIsRecorded(t *testing.T) {
	m, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt-a--example.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, ct := range []string{"text/event-stream", "text/event-stream; charset=utf-8"} {
		st := rewrite.NewStats(true)
		p := &Proxy{Map: m, Stats: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		resp := &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": {ct}},
			Body:       io.NopCloser(strings.NewReader("data: https://www.example.fi/x\n\n")),
		}
		site, _ := m.SiteForHost("wt-a--example.ddev.site")
		if err := p.finishBody(resp, &state{site: site}, false); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range st.Events() {
			if e.Reason == origin.ReasonEventStream && e.Action == origin.ActionSkipped {
				found = true
			}
		}
		if !found {
			t.Errorf("Content-Type %q: no event-stream skip recorded; --explain reports nothing "+
				"for a body whose origins go out as written", ct)
		}
	}
}
