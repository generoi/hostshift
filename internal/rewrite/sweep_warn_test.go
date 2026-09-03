package rewrite

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// The straggler WARN names the origin it swept, and that has to hold with
// --explain off — which is every proxy in normal operation.
//
// The sweep asks the matcher for events with explain=false so a skipped
// candidate costs no string. Event.Text is populated for ActionRewrote
// regardless, and the WARN only fires for ActionRewrote, so the two fit. If
// that ever stops being true the WARN degrades to `origin=""`: still a warning,
// still counted, but no longer naming the URL that leaked — and a straggler
// nobody can locate is a bug report nobody can act on. Nothing else asserts it.
func TestStragglerWarnNamesTheOriginWithoutExplain(t *testing.T) {
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt-a--example.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`leaked https://www.example.fi/a here`)

	for _, c := range []struct {
		name string
		run  func(*slog.Logger, *Stats) []byte
	}{
		{"SweepBytes", func(log *slog.Logger, st *Stats) []byte {
			return SweepBytes(body, m, st, log)
		}},
		{"Sweep", func(log *slog.Logger, st *Stats) []byte {
			s := NewSweep(bytes.NewReader(body), m, nil, Options{Stats: st, Log: log})
			out, err := io.ReadAll(s)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logBuf, nil))
			st := NewStats(false) // --explain off, as in production

			out := c.run(log, st)
			if bytes.Contains(out, []byte("www.example.fi")) {
				t.Fatalf("the sweep did not rewrite: %s", out)
			}
			logged := logBuf.String()
			if !strings.Contains(logged, "straggler swept") {
				t.Fatalf("no WARN at all:\n%s", logged)
			}
			if !strings.Contains(logged, "www.example.fi") {
				t.Errorf("the WARN does not name the origin it swept, so nobody can find it:\n%s", logged)
			}
		})
	}
}
