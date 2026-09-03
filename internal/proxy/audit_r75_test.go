package proxy

import (
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// The multipart part-type gate and `bodyKind` disagree about the same media
// types, which is the asymmetry `bodyKind`'s own comment says was closed.
//
// `bodyKind` sends a *top-level* `application/xml`, `application/xhtml+xml`,
// any `*+xml` and `application/x-www-form-urlencoded` body down the flat arm,
// where it is mapped variant -> canonical. Its comment records why: "the two
// directions must not disagree about the same body … Same shape as the
// multipart finding a round earlier: an arm nobody enumerated."
//
// `rewritablePart` (multipart.go) accepts only `text/*`, `application/json`
// and `*+json`. So the identical bytes, sent as a non-file *part* of a
// multipart body, go upstream with the variant hostname in them — into the
// shared database, which §4.3 calls unrecoverable.
//
// Measured on a running proxy in front of a real WordPress rather than argued.
// One POST, five non-file parts, and what the *application* received was read
// off the web container's disk (`file_put_contents` of `php://input`), never
// echoed back — an echo would have been healed by the response rewriter, which
// is how this harness has been fooled before:
//
//	'plain'      => 'https://www.r75-a.example/a'             (no Content-Type)
//	'json_part'  => '{"u":"https://www.r75-a.example/b"}'     (application/json)
//	'xml_part'   => '<a href="https://wt-a--r75w-ms.ddev.site/c"/>'
//	'form_part'  => 'u=https://wt-a--r75w-ms.ddev.site/d'
//	'octet_part' => 'https://wt-a--r75w-ms.ddev.site/e'
//
// The same three payloads sent as top-level bodies of the same media types all
// arrived canonical.
//
// No browser produces such a part — a browser sends no Content-Type on an
// ordinary text field, and `FormData.append(name, blob)` names the part "blob"
// and so makes it a file part — so this is reachable from a scripted client
// (`curl -F 'x=<f;type=application/xml'`, an SDK, an integration plugin) rather
// than from wp-admin. It is recorded because the codebase has twice declared
// this exact disagreement closed, and it is closed only in the top-level arm.

// The premise, so the skipped test below cannot be vacuous: these media types
// really are mapped back when they arrive as a whole body, and the multipart
// splicer really does run on the body used there.
func TestR75TopLevelArmRewritesWhatTheMultipartArmDoesNot(t *testing.T) {
	for _, ct := range []string{
		"application/xml",
		"application/xhtml+xml",
		"application/rss+xml",
		"application/atom+xml",
		"image/svg+xml",
		"application/x-www-form-urlencoded",
	} {
		if got := bodyKind(ct, nil); got != bodyFlat {
			t.Errorf("bodyKind(%q, nil) = %v, want bodyFlat", ct, got)
		}
	}

	out := rewriteR75Multipart(t)
	if strings.Contains(out, "wt-a--r75w.ddev.site/ok") ||
		!strings.Contains(out, "www.r75a.example/ok") {
		t.Fatalf("harness: the text/plain part was not mapped back, so this "+
			"fixture proves nothing about the gate:\n%s", out)
	}
}

// An `application/xml` part of a multipart body used to reach the application
// with the variant hostname in it, and be written to the shared database:
// `rewritablePart` restated a list that `bodyKind` had since widened, so the
// same payload mapped back as a whole body and not as a part. It derives the set
// from `bodyKind` now, so the two arms cannot drift again — which is the third
// time this file has declared that disagreement closed.
func TestR75MultipartXMLPartIsMappedBack(t *testing.T) {
	out := rewriteR75Multipart(t)
	if strings.Contains(out, "wt-a--r75w.ddev.site/xml") {
		t.Errorf("an application/xml part kept the variant hostname:\n%s", out)
	}
	if !strings.Contains(out, "www.r75a.example/xml") {
		t.Errorf("an application/xml part should carry the canonical origin:\n%s", out)
	}
}

func rewriteR75Multipart(t *testing.T) string {
	t.Helper()
	b := "----r75"
	body := []byte("--" + b + "\r\n" +
		"Content-Disposition: form-data; name=\"ok\"\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"https://wt-a--r75w.ddev.site/ok\r\n" +
		"--" + b + "\r\n" +
		"Content-Disposition: form-data; name=\"xml\"\r\n" +
		"Content-Type: application/xml\r\n\r\n" +
		"<a href=\"https://wt-a--r75w.ddev.site/xml\"/>\r\n" +
		"--" + b + "\r\n" +
		"Content-Disposition: form-data; name=\"bin\"\r\n" +
		"Content-Type: application/octet-stream\r\n\r\n" +
		"https://wt-a--r75w.ddev.site/bin\r\n" +
		"--" + b + "--\r\n")

	c, err := origin.Parse("https://www.r75a.example")
	if err != nil {
		t.Fatal(err)
	}
	v, err := origin.Parse("https://wt-a--r75w.ddev.site")
	if err != nil {
		t.Fatal(err)
	}
	mp, err := origin.NewMap([]origin.Site{{Name: "s", Canonical: c, Variant: v}})
	if err != nil {
		t.Fatal(err)
	}
	return string(rewriteMultipart(body, "multipart/form-data; boundary="+b,
		mp.Reverse(), rewrite.NewStats(false), false, nil))
}

// A part of a type nothing classifies stays byte-identical.
//
// Deriving the gate from bodyKind is only safe because bodyOther is excluded
// with bodyMultipart: an `application/octet-stream` part is a payload whose
// bytes mean something to the application, and splicing a hostname into one is
// the corruption file parts are kept byte-identical to avoid. The set has to be
// "what bodyKind calls flat", not "what bodyKind does not call multipart".
func TestR75AnUnclassifiedPartIsNotRewritten(t *testing.T) {
	out := rewriteR75Multipart(t)
	if !strings.Contains(out, "wt-a--r75w.ddev.site/bin") {
		t.Errorf("an application/octet-stream part was rewritten; its bytes are a "+
			"payload, not prose:\n%s", out)
	}
}
