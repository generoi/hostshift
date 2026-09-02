package corpus

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"testing"
)

// The leak column has to see base64 for the same reason the write-back column
// does, and it still cannot.
//
// Round 67 taught `WriteBacks` to look inside base64
// (internal/corpus/diff.go:377, TestAVariantOriginInsideBase64IsCountedAsAWriteBack)
// because `countLeaks` runs the rewrite pipeline and the pipeline has no base64
// view. That reasoning is direction-free — it is a property of the pipeline, not
// of which map is pointed at it — but the fix was applied to one direction only.
// `HiddenInBase64` is called in exactly two places in the whole tree
// (internal/proxy/proxy.go:859 and internal/corpus/diff.go:377) and both look
// for a *variant* origin. Nothing anywhere looks for a *canonical* one.
//
// So the mirror image of round 67's fixture — the same widget instance, with
// production's hostname inside it, served through the proxy to the developer's
// browser — is scored:
//
//	Leaks 0, WriteBacks 0, byte-identical, GREEN
//
// on the run PLAN §7 calls "the only test that validates against reality". And
// the page is not inert. `WP_REST_Widgets_Controller` returns a legacy widget's
// settings as `instance.encoded` = base64(serialize(...)); the widgets screen
// and the Customizer decode it in JavaScript and render the widget, so an
// `<img src>` or an `<a href>` inside it becomes a live production URL in the
// developer's authenticated browser. That is test 28.
//
// As with the write-back column, the ask is not that the proxy rewrite it —
// `wp_hash()` covers exactly those bytes, so rewriting makes WordPress discard
// the save, and PLAN §4.3 accepts that. The ask is that something say so. The
// request direction already does, with a WARN naming the decoded blob
// (internal/proxy/proxy.go:859-874). The response direction and this report say
// nothing at all, which is the state PLAN §4.3 describes as the one that "went
// unnoticed for twenty-two rounds".
func TestACanonicalOriginInsideBase64IsCountedAsALeak(t *testing.T) {
	inner := `<a href="https://www.canon.test/promo/">promo</a>`
	blob := base64.StdEncoding.EncodeToString([]byte(
		`a:1:{s:7:"content";s:` + strconv.Itoa(len(inner)) + `:"` + inner + `";}`))
	body := `<div data-cfg="` + blob + `">x</div>`

	// The proxy cannot rewrite it, so both sides carry the same bytes — which is
	// exactly why byte comparison cannot see this and a scan has to.
	r := compareBodies(t, body, body)
	if r.WriteBacks != 0 {
		t.Fatalf("this fixture carries no variant origin, got %d write-backs", r.WriteBacks)
	}
	if r.Leaks == 0 {
		t.Error("a production origin inside base64 was served to the browser and the " +
			"report counted nothing — the mirror of the hole round 67 closed in the " +
			"other direction")
	}
	var buf bytes.Buffer
	if WriteReport(&buf, []Result{r}) {
		t.Errorf("the run went GREEN on a page whose base64 decodes to a live "+
			"production origin:\n%s", buf.String())
	}

	// A blob with nothing mapped in it stays at zero, so this is not just
	// "any base64 is suspicious".
	clean := base64.StdEncoding.EncodeToString([]byte(`a:1:{s:7:"content";s:9:"/promo/ x";}`))
	if q := compareBodies(t, `<div data-cfg="`+clean+`">x</div>`,
		`<div data-cfg="`+clean+`">x</div>`); q.Leaks != 0 {
		t.Errorf("an ordinary base64 blob reported %d leaks", q.Leaks)
	}
}
