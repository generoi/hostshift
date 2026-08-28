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
	eof    bool
	closer io.Closer
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
	return &Sweep{
		src: r, m: m, stats: st, log: log,
		window: m.MaxMatchLen(), dryRun: opt.DryRun, closer: src,
	}
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
	out, consumed, events := s.m.RewritePrefix(b, limit, SurfaceStraggler, true)
	for _, e := range events {
		if e.Action != origin.ActionRewrote {
			continue
		}
		// Every straggler is a bug in the structured pass, so it is reported
		// individually and loudly, with enough context to find it.
		s.log.Warn("straggler swept — a canonical origin survived the structured pass",
			"offset", s.offset+e.Offset,
			"origin", e.Text,
			"context", context(b, e.Offset))
	}
	s.stats.Record(SurfaceStraggler, s.offset, events)
	s.offset += consumed
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

// context returns the bytes either side of a straggler, for the report.
func context(b []byte, at int) string {
	lo := max(0, at-48)
	hi := min(len(b), at+64)
	return string(bytes.ReplaceAll(b[lo:hi], []byte("\n"), []byte(" ")))
}
