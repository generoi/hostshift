package rewrite

import (
	"bytes"
	"io"
	"log/slog"

	"github.com/generoi/hostshift/internal/origin"
)

// SurfaceStraggler is where the sweep's finds are counted. A non-zero count is
// not a success — it is a list of bugs in the structured pass.
const SurfaceStraggler = "straggler"

// Sweep is PLAN §4.4's straggler sweep: a re-scan of already-rewritten output
// for canonical origins the structured pass missed.
//
// It is the backstop, not the primary path. Every hit is a gap somewhere else —
// the known one is foreign content, where the tokenizer does not track SVG or
// MathML namespaces, so a URL inside <svg><title> is treated as raw text and
// missed. The sweep catches and *reports* those; §5.2's every-attribute scan is
// what is supposed to prevent them.
//
// It runs in-stream, not on a buffer. A carry-over window of MaxMatchLen bytes
// is retained across chunk boundaries so no match can straddle one, and a match
// is replaced before those bytes are emitted — so nothing already written needs
// re-aligning and the body is never buffered whole. The replacement changes
// length, which is why the response is chunked (§5.2).
//
// The anchoring is what makes this safe to run at all: it is idempotent, and it
// cannot touch a bare hostname appearing as prose (test 28 requires exactly
// that).
type Sweep struct {
	src    io.Reader
	m      *origin.Matcher
	stats  *Stats
	log    *slog.Logger
	window int
	dryRun bool

	pending []byte
	// read is reused across Read calls. Allocating it per call cost 179 MB of
	// garbage on a single 500 KB page, because the tokenizer upstream hands back
	// one small token at a time and each one drove a fresh 32 KB allocation.
	read   []byte
	out    bytes.Buffer
	offset int // cumulative offset into the stream being swept
	prev   int // the last byte consumed, as left context; origin.NoPrev at the start
	src2in inputOffsetMapper
	eof    bool
	closer io.Closer
}

// inputOffsetMapper is implemented by an upstream stage that changes lengths,
// so a straggler can be reported at its offset in the *original* document.
// PLAN §4.4 requires input-stream offsets precisely so they stay stable across
// a length-changing rewrite; without this the sweep reports its own coordinates.
type inputOffsetMapper interface{ InputOffset(out int) int }

// in maps an offset in the swept stream back to the source document.
func (s *Sweep) in(off int) int {
	if s.src2in == nil {
		return off
	}
	return s.src2in.InputOffset(off)
}

// NewSweep wraps r. src may be nil.
func NewSweep(r io.Reader, m *origin.Matcher, src io.Closer, opt Options) *Sweep {
	st := opt.Stats
	if st == nil {
		st = NewStats(false)
	}
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	sw := &Sweep{
		src: r, m: m, stats: st, log: log, prev: origin.NoPrev,
		window: m.MaxMatchLen(), dryRun: opt.DryRun, closer: src,
	}
	if mp, ok := r.(inputOffsetMapper); ok {
		sw.src2in = mp
	}
	return sw
}

func (s *Sweep) Read(p []byte) (int, error) {
	if s.read == nil {
		s.read = make([]byte, 32*1024)
	}
	for s.out.Len() == 0 && !s.eof {
		n, err := s.src.Read(s.read)
		if n > 0 {
			s.pending = append(s.pending, s.read[:n]...)
		}
		if err == io.EOF {
			s.eof = true
		} else if err != nil {
			return 0, err
		}

		// Matches beginning before limit have all their deciding bytes in
		// hand; anything later waits for the next read. The matcher reports
		// how far it actually consumed, which may run past limit when a match
		// straddles it — truncating at limit instead would split the match and
		// silently miss it.
		limit := len(s.pending)
		if !s.eof {
			limit -= s.window
			if limit <= 0 {
				continue
			}
		}
		consumed := s.flush(s.pending, limit)
		s.pending = append(s.pending[:0], s.pending[consumed:]...)
	}
	if s.out.Len() == 0 && s.eof {
		return 0, io.EOF
	}
	return s.out.Read(p)
}

func (s *Sweep) flush(b []byte, limit int) int {
	if len(b) == 0 {
		return 0
	}
	// s.stats.Explain(), not a bare true. Text is materialised for every
	// ActionRewrote event regardless, which is all the WARN below reads, so
	// forcing explain on bought nothing but a string per *skipped* candidate —
	// and the sweep runs over every byte of every HTML body.
	out, consumed, events := s.m.RewritePrefix(b, limit, s.prev, SurfaceStraggler, s.stats.Explain())
	// Events arrive in increasing offset order, which is what the mapper's
	// cursor needs, and each is mapped individually — the drift is not a
	// constant per flush, it accumulates with every rewrite upstream. The
	// context slice still uses the local offset, since b is the swept stream.
	for i := range events {
		local := events[i].Offset
		events[i].Offset = s.in(s.offset + local)
		if events[i].Action != origin.ActionRewrote {
			continue
		}
		// Every straggler is a bug in the structured pass, so it is reported
		// individually and loudly, with enough context to find it.
		//
		// "an origin", not "a canonical origin": this same sweep runs on the
		// request arm, where the matcher maps variant to canonical and the thing
		// it finds is a *variant* hostname. The message named the wrong
		// direction there, and the `origin` field below says which it was anyway.
		s.log.Warn("straggler swept — an origin survived the structured pass",
			"offset", events[i].Offset,
			"origin", events[i].Text,
			"context", context(b, local))
	}
	s.stats.Record(SurfaceStraggler, 0, events)
	s.offset += consumed
	if consumed > 0 {
		s.prev = int(b[consumed-1])
	}
	if s.dryRun {
		s.out.Write(b[:consumed])
	} else {
		s.out.Write(out)
	}
	return consumed
}

// Close closes the underlying source, if there is one.
func (s *Sweep) Close() error {
	if s.closer != nil {
		return s.closer.Close()
	}
	return nil
}

// SweepBytes is §4.4's straggler backstop for a body that is already buffered.
//
// The streaming Sweep exists because HTML bodies must not be held whole; JSON
// is buffered anyway (§5.8's size cap), so it needs none of that machinery — no
// carry-over window, no left-context byte, no offset map, because the whole
// document is in hand and its offsets are already input offsets.
//
// M4 added an entire response surface without one. Every miss on the JSON path
// was therefore a silent test 28 leak: the same post body that the HTML path
// rewrote and, where it could not, reported, went out over /wp-json/ carrying a
// production origin with no WARN and no non-zero counter — the exact inverse of
// "each straggler is a gap in the structured pass and a bug to fix".
func SweepBytes(b []byte, m *origin.Matcher, st *Stats, log *slog.Logger) []byte {
	if len(b) == 0 || m.Identity() {
		return b
	}
	if log == nil {
		log = slog.Default()
	}
	out, events := m.Rewrite(b, SurfaceStraggler, st.Explain())
	for _, e := range events {
		if e.Action != origin.ActionRewrote {
			continue
		}
		// Direction-neutral for the reason the streaming sweeper's copy gives.
		log.Warn("straggler swept — an origin survived the structured pass",
			"offset", e.Offset,
			"origin", e.Text,
			"context", context(b, e.Offset))
	}
	st.Record(SurfaceStraggler, 0, events)
	return out
}

// context returns the bytes either side of a straggler, for the report.
func context(b []byte, at int) string {
	lo := max(0, at-48)
	hi := min(len(b), at+64)
	return string(bytes.ReplaceAll(b[lo:hi], []byte("\n"), []byte(" ")))
}
