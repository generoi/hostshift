package rewrite

import (
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/generoi/hostshift/internal/origin"
)

// Surfaces. Test 25 asserts that content outside the rewritable set never enters
// a rewriter, "proven by a per-surface counter of zero" — so these exist from M1,
// before the surfaces that use them do.
const (
	SurfaceHTMLAttr     = "html-attr"
	SurfaceInlineScript = "inline-script"
	SurfaceInlineStyle  = "inline-style"
	// SurfaceRawText is the markup inside every other raw-text element —
	// noscript, textarea, title, iframe, and <title> inside foreign content —
	// which the tokenizer hands back as opaque text rather than parsing.
	SurfaceRawText = "raw-text"
	// SurfaceHTMLEntity is an attribute value whose origin was only visible
	// after character references were decoded — the browser decodes before it
	// resolves, so these are test 28 leaks the raw scan cannot see. A non-zero
	// count is worth looking at: it means content is storing origins in a form
	// §5.3's three encodings do not model.
	SurfaceHTMLEntity = "html-entity"
	SurfaceHeader     = "header"
	SurfaceJSONString = "json-string"
	// SurfaceJSONEscape is a JSON string whose origin was only visible after
	// the string was unquoted — a \uXXXX-escaped IDN host, an HTML character
	// reference inside content.rendered, or double-escaped JSON-in-JSON. Like
	// html-entity, a non-zero count means content is storing origins in a form
	// §5.3 does not model.
	SurfaceJSONEscape  = "json-escape"
	SurfaceRequestLine = "request-line"
	SurfaceRequestBody = "request-body"
)

// Stats accumulates per-surface counters and, under --explain, the trace of
// every candidate that did not result in a rewrite (PLAN §5.8).
//
// Safe for concurrent use: one Stats is shared by every in-flight response.
type Stats struct {
	mu         sync.Mutex
	candidates map[string]int
	rewrites   map[string]int
	skips      map[string]int
	structured map[string]int
	events     []origin.Event
	explain    bool
	maxEvents  int
	noSweep    bool
}

// SweepSkipped records that §4.4's straggler backstop did not run, so the
// report says the census is unavailable instead of printing a zero that reads
// like proof of coverage.
func (s *Stats) SweepSkipped() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.noSweep = true
	s.mu.Unlock()
}

// NewStats returns a Stats. When explain is false only rewrites are recorded,
// which is what the counters need; the skip trace is the expensive half.
func NewStats(explain bool) *Stats {
	return &Stats{
		candidates: map[string]int{},
		rewrites:   map[string]int{},
		skips:      map[string]int{},
		structured: map[string]int{},
		explain:    explain,
		maxEvents:  10000,
	}
}

func (s *Stats) Explain() bool { return s != nil && s.explain }

// Record folds one value's worth of events in, offsetting them to
// cumulative input-stream positions.
func (s *Stats) Record(surface string, base int, events []origin.Event) {
	if s == nil || len(events) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range events {
		s.candidates[surface]++
		if e.Action == origin.ActionRewrote {
			s.rewrites[surface]++
		} else {
			s.skips[e.Reason]++
		}
		if s.explain && len(s.events) < s.maxEvents {
			e.Offset += base
			s.events = append(s.events, e)
		}
	}
}

// Structured counts the attributes §5.2 listed as needing their grammar parsed
// — srcset, imagesrcset, ping, srcdoc, content.
//
// M3 established that none of them does: anchoring finds origins wherever they
// sit, so commas, descriptors, "N;url=" and entity-encoded HTML never have to be
// understood. The counter is kept for visibility into how often those values
// carry origins at all, since a regression there is the first place a leak would
// show.
func (s *Stats) Structured(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.structured[name]++
	s.mu.Unlock()
}

// Rewrites returns the rewrite count for one surface.
func (s *Stats) Rewrites(surface string) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rewrites[surface]
}

// Total returns the total number of rewrites across all surfaces.
func (s *Stats) Total() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, v := range s.rewrites {
		n += v
	}
	return n
}

// Events returns the --explain trace.
func (s *Stats) Events() []origin.Event {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]origin.Event(nil), s.events...)
}

// Snapshot is the --json form of the counters (PLAN §5.8).
type Snapshot struct {
	Rewrites   map[string]int `json:"rewrites"`
	Candidates map[string]int `json:"candidates"`
	Skips      map[string]int `json:"skips"`
	Structured map[string]int `json:"structured,omitempty"`
	Events     []origin.Event `json:"events,omitempty"`
}

// Snapshot returns a copy of the counters.
func (s *Stats) Snapshot() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := func(m map[string]int) map[string]int {
		out := make(map[string]int, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	return Snapshot{
		Rewrites:   cp(s.rewrites),
		Candidates: cp(s.candidates),
		Skips:      cp(s.skips),
		Structured: cp(s.structured),
		Events:     append([]origin.Event(nil), s.events...),
	}
}

// WriteReport writes the counters to w. stdout is data and stderr is
// diagnostics (PLAN §5.8), so callers pass stderr.
func (s *Stats) WriteReport(w io.Writer) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	section := func(title string, m map[string]int) {
		if len(m) == 0 {
			return
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(w, "%s:\n", title)
		for _, k := range keys {
			fmt.Fprintf(w, "  %-24s %d\n", k, m[k])
		}
	}
	section("rewrites by surface", s.rewrites)
	section("candidates by surface", s.candidates)
	section("skipped, by reason", s.skips)
	section("structured attributes seen", s.structured)

	if s.noSweep {
		fmt.Fprintln(w, "straggler sweep: not run — census unavailable")
	}

	if s.explain && len(s.events) > 0 {
		fmt.Fprintf(w, "explain (%d events):\n", len(s.events))
		for _, e := range s.events {
			reason := e.Reason
			if reason == "" {
				reason = "-"
			}
			where := e.Surface
			if e.Path != "" {
				where += " " + e.Path
			}
			fmt.Fprintf(w, "  %8d  %-8s %-22s %-40s %s\n", e.Offset, e.Action, reason, where, e.Text)
		}
	}
}
