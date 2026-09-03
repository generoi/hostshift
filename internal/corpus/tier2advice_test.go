package corpus

import (
	"bytes"
	"strings"
	"testing"
)

// The Tier 2 line is the one place a deployment is told it is leaking through a
// surface the proxy excludes. It quoted PLAN's condition for lifting the
// exclusion and did not say what lifts it, which for one release meant the
// override existed and the report that should send you to it did not mention it.
func TestTheTierTwoLineNamesTheFlagThatLiftsIt(t *testing.T) {
	var b bytes.Buffer
	WriteReport(&b, []Result{{
		Path: "/style.css", ContentType: "text/css", Equal: true, Tier2: 4,
	}})
	out := b.String()
	if !strings.Contains(out, "Tier 2") {
		t.Fatalf("no Tier 2 line was printed at all:\n%s", out)
	}
	if !strings.Contains(out, "--rewrite-type") {
		t.Errorf("the Tier 2 line reports the leak and does not name the flag "+
			"that rewrites it — the operator is told the trigger fired and left "+
			"to find the override:\n%s", out)
	}
	// And say what it costs, or the advice is a footgun: naming a type buffers
	// every response of it to the size cap instead of streaming it.
	if !strings.Contains(out, "max-body") {
		t.Errorf("the advice does not say the types are then buffered:\n%s", out)
	}
}
