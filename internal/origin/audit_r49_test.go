package origin

import (
	"strings"
	"testing"
)

// Round 49, on acc0ae8.

// TestR49IDNRoundTrip: content spelling the canonical the way the map declares
// it must survive forward-then-reverse byte for byte. That is §4.3's promise —
// the shared database stays byte-identical to production — and it is what
// acc0ae8 restored for a U-label declaration. It does not hold for an A-label
// one, because Origin.Display is not the declared spelling: foldHost's
// ToUnicode *decodes* an A-label, so Display is always the U-label.
func TestR49IDNRoundTrip(t *testing.T) {
	for _, c := range []struct{ decl, host string }{
		{"https://www.hämeenlinna.fi", "www.hämeenlinna.fi"},
		{"https://www.xn--hmeenlinna-q5a.fi", "www.xn--hmeenlinna-q5a.fi"},
	} {
		m, err := NewMap([]Site{{
			Name: "hml", Canonical: MustParse(c.decl),
			Variant: MustParse("https://wt-a--hml.ddev.site"),
		}})
		if err != nil {
			t.Fatal(err)
		}
		for _, body := range []string{
			"https://" + c.host + "/x",
			`https:\/\/` + c.host + `\/x`,
			"https%3A%2F%2F" + c.host + "%2Fx",
			"//" + c.host + "/x",
		} {
			fwd, _ := m.Forward().RewriteText([]byte(body), "text", false)
			back, _ := m.Reverse().RewriteText(fwd, "text", false)
			if string(back) != body {
				t.Errorf("decl=%s round trip lost bytes:\n  in:   %s\n  fwd:  %s\n  back: %s",
					c.decl, body, fwd, back)
			}
		}
	}
}

// TestR49AUnicodeVariantIsSplicedIntoResponses: the derived variant of an IDN
// canonical is an ACE host, and Origin.Parse gives every ACE host a Display, so
// the forward pass emits a *Unicode variant* into the response body.
func TestR49AUnicodeVariantIsSplicedIntoResponses(t *testing.T) {
	v := MustParse("https://wt-a--www.xn--hmeenlinna-q5a.fi")
	t.Logf("variant Host=%q Display=%q DisplayHostPort=%q", v.Host, v.Display, v.DisplayHostPort())

	m, err := NewMap([]Site{{
		Name: "hml", Canonical: MustParse("https://www.hämeenlinna.fi"),
		Variant: v,
	}})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := m.Forward().RewriteText([]byte("https://www.hämeenlinna.fi/x"), "text", false)
	t.Logf("forward output: %s", out)
	if strings.Contains(string(out), "xn--hmeenlinna") {
		t.Logf("variant spliced as ACE")
	} else {
		t.Logf("variant spliced as U-label")
	}
	// And back again.
	back, _ := m.Reverse().RewriteText(out, "text", false)
	t.Logf("reverse output: %s", back)
	if string(back) != "https://www.hämeenlinna.fi/x" {
		t.Errorf("round trip through a Unicode variant lost bytes: %s", back)
	}
}

// TestR49DisplayInMapKeys: Origin is used as a map key in Validate and in
// NewMap's owner table, and Display is part of the struct — so two declarations
// of the same origin in different spellings are two different keys.
func TestR49DisplayInMapKeys(t *testing.T) {
	_, err := NewMap([]Site{
		{Name: "a", Canonical: MustParse("https://www.hämeenlinna.fi"),
			Variant: MustParse("https://wt-a--a.ddev.site")},
		{Name: "b", Canonical: MustParse("https://www.xn--hmeenlinna-q5a.fi"),
			Variant: MustParse("https://wt-a--b.ddev.site")},
	})
	if err == nil {
		t.Error("two sites declaring the same canonical host in two spellings were accepted")
	} else {
		t.Logf("rejected: %v", err)
	}
}

// TestR50DisplayIsAlwaysTheSameHostAsHost: foldHost's comment says the two have
// to fold identically "or the display form would be a *different* host". Nothing
// checked it, and a declaration mixing a broken `xn--` label with a non-ASCII
// one produced exactly that — a Display that normalises to some third name,
// which the reverse direction would then splice into a save.
func TestR50DisplayIsAlwaysTheSameHostAsHost(t *testing.T) {
	for _, decl := range []string{
		"https://Xn--Xn--ք--",
		"https://xn--hmeenlinna-q5a.hämeenlinna.fi",
		"https://www.hämeenlinna.fi",
		"https://www.xn--hmeenlinna-q5a.fi",
		"https://www.example.fi",
	} {
		o, err := Parse(decl)
		if err != nil {
			continue
		}
		if o.Display == "" {
			continue
		}
		back, err := NormaliseHost(o.Display)
		if err != nil || back != o.Host {
			t.Errorf("%s: Display %q normalises to %q, not to Host %q",
				decl, o.Display, back, o.Host)
		}
	}
}
