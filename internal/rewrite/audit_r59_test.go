package rewrite

import (
	"io"
	"log/slog"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 59, on 05ae5ea. The census hook, which round 58 added so `--explain`
// and `--dry-run` write down what they claim to.

// The hook runs inside the counter lock.
//
// `Record` takes `s.mu`, `defer`s the unlock, and calls `s.onEvent` from inside
// the loop — so an arbitrary callback runs while every other in-flight response
// is blocked on the one `Stats` this proxy shares. The callback `cmdProxy`
// installs writes a line to stderr through slog: a blocking write to the
// container's log driver, per event, holding the lock the whole time. `check`
// tells developers to turn this on in a running preview ("add --explain to
// HOSTSHIFT_ARGS … `ddev restart`"), which is the moment the proxy is under a
// browser's worth of parallel requests.
//
// Measured with `Record` called from eight goroutines, twenty events each, into
// an io.Discard slog handler: 447 ns/op with no hook, 13,100 ns/op with the hook
// under the lock, 4,889 ns/op with the same hook called after the unlock — the
// lock is 63% of the cost it adds. A sink that blocks is worse than
// proportionally: stalling one log write stalls every response's counters.
//
// And it is a re-entrancy trap with no sign on it. `OnEvent` is exported and
// documented as "a callback run for every recorded event"; a callback that asks
// the Stats anything — `Rewrites`, `Total`, `Snapshot`, a second `OnEvent` —
// deadlocks the proxy, because `sync.Mutex` is not reentrant. Nothing in the
// signature or the comment says so.
//
// TryLock rather than a deadlocking call, so a failure here is a red test and
// not a hung suite.
func TestR59TheCensusHookDoesNotRunUnderTheCounterLock(t *testing.T) {
	s := NewStats(false)
	held := 0
	s.OnEvent(func(surface string, e origin.Event) {
		if s.mu.TryLock() {
			s.mu.Unlock()
			return
		}
		held++
	})
	s.Record(SurfaceResponseHeader, 0, []origin.Event{
		{Action: origin.ActionRewrote, Text: "www.example.fi"},
		{Action: origin.ActionRewrote, Text: "www.example.fi"},
	})
	if held != 0 {
		t.Errorf("the census callback ran with Stats.mu held for %d of 2 events, so a\n"+
			"blocking stderr write serialises every in-flight response's counters and\n"+
			"any callback that reads the Stats back deadlocks the proxy", held)
	}
}

// The same statement as a fact about the installed callback: the one cmdProxy
// builds is a slog write, and it must be safe to ask the Stats a question from
// inside one.
func TestR59ACensusCallbackMayReadTheStats(t *testing.T) {
	s := NewStats(false)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var seen int
	s.OnEvent(func(surface string, e origin.Event) {
		if !s.mu.TryLock() {
			return // the deadlock this documents; the test above reports it
		}
		s.mu.Unlock()
		seen = s.Rewrites(surface) // would deadlock under the lock
		log.Info("census", "surface", surface, "action", e.Action, "text", e.Text)
	})
	s.Record(SurfaceResponseHeader, 0, []origin.Event{
		{Action: origin.ActionRewrote, Text: "www.example.fi"},
	})
	if seen == 0 {
		t.Errorf("a callback cannot read the counter it is being told about: OnEvent is\n" +
			"exported, documented as running \"for every recorded event\", and holds\n" +
			"the lock every Stats accessor takes")
	}
}
