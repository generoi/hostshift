package proxy

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// TestR69MultipartBase64PartIsSilent
//
// Round 68 made a base64 run whitespace-tolerant so that a
// `Content-Transfer-Encoding: base64` part, wrapped at RFC 2045's 76 columns,
// is one run rather than one run per line. The run is greedy in both directions
// and stripB64Space concatenates whatever it swallowed, so the literal word
// `base64` on the header line — six characters, all in the alphabet, separated
// from the blob by nothing but CRLF — is prepended to the blob. Six is not a
// multiple of four, the decode fails, and the detector says nothing.
//
// The refusal is the whole remedy for a variant hostname inside base64 (§4.3),
// so a silent detector is the harm. Measured against a running proxy: the same
// blob with no CTE header, or with `7bit`/`8bit` (four characters), is reported;
// with `base64`, `binary` or `quoted-printable` it is not.
func TestR69MultipartBase64PartIsSilent(t *testing.T) {
	blob := base64.StdEncoding.EncodeToString(
		[]byte(`a:1:{s:5:"title";s:31:"https://wt-a--r69w.ddev.site/x/";}`))
	const bd = "----r69b"
	const cd = `Content-Disposition: form-data; name="instance"`

	body := func(cte string) []byte {
		h := cd
		if cte != "" {
			h += "\r\nContent-Transfer-Encoding: " + cte
		}
		return []byte("--" + bd + "\r\n" + h + "\r\n\r\n" + blob + "\r\n--" + bd + "--\r\n")
	}

	for _, tc := range []struct{ cte, why string }{
		{"", "no Content-Transfer-Encoding"},
		{"7bit", "a four-character token keeps the alignment"},
		{"8bit", "a four-character token keeps the alignment"},
		{"base64", "the token round 68's whitespace tolerance exists for"},
		{"binary", "six characters"},
		{"quoted-printable", "the run resumes at `printable`, nine characters"},
	} {
		var buf bytes.Buffer
		p := r69proxy(t, &buf)
		front := httptest.NewServer(p.Handler())

		req, _ := http.NewRequest("POST", front.URL+"/probe", bytes.NewReader(body(tc.cte)))
		req.Host = "wt-a--r69w.ddev.site"
		req.Header.Set("Content-Type", "multipart/form-data; boundary="+bd)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		front.Close()

		if !strings.Contains(buf.String(), "inside base64") {
			t.Errorf("Content-Transfer-Encoding: %q (%s): a variant hostname went "+
				"upstream inside base64 with nothing logged", tc.cte, tc.why)
		}
	}
}

func r69proxy(t *testing.T, out *bytes.Buffer) *Proxy {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	t.Cleanup(up.Close)

	c, err := origin.Parse("https://www.r69a.example")
	if err != nil {
		t.Fatal(err)
	}
	v, err := origin.Parse("https://wt-a--r69w.ddev.site")
	if err != nil {
		t.Fatal(err)
	}
	m, err := origin.NewMap([]origin.Site{{Name: "s", Canonical: c, Variant: v}})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(up.URL)
	return &Proxy{
		Upstream: u, Map: m, Stats: rewrite.NewStats(false),
		Log: slog.New(slog.NewTextHandler(out, nil)),
	}
}
