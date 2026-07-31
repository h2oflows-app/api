package handlers

import "testing"

func strPtr(s string) *string { return &s }

func TestDisplayLabel(t *testing.T) {
	cases := []struct {
		name        string
		displayName *string
		handle      string
		want        string
	}{
		{"nil display name", nil, "iankco", "@iankco"},
		{"empty display name", strPtr(""), "iankco", "@iankco"},
		{"whitespace-only display name", strPtr("  "), "iankco", "@iankco"},
		{"set display name", strPtr("Ian K"), "iankco", "Ian K (@iankco)"},
		{"padded display name", strPtr(" Ian K "), "iankco", "Ian K (@iankco)"},
	}
	for _, c := range cases {
		if got := displayLabel(c.displayName, c.handle); got != c.want {
			t.Errorf("%s: displayLabel = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDisplayHost(t *testing.T) {
	if got := displayHost(strPtr("Ian K"), ""); got != "A paddler" {
		t.Errorf("empty handle: displayHost = %q, want %q", got, "A paddler")
	}
	if got := displayHost(strPtr("Ian K"), "iankco"); got != "Ian K (@iankco)" {
		t.Errorf("displayHost = %q, want %q", got, "Ian K (@iankco)")
	}
}

func TestStripControlRunes(t *testing.T) {
	cases := map[string]string{
		"Ian K":                "Ian K",
		"Foo\r\nBar":           "FooBar",
		"Tab\there":            "Tabhere",
		"nul\x00byte":          "nulbyte",
		"José García-H":        "José García-H",
		"Foo\r\nX-EVIL;CN=x:m": "FooX-EVIL;CN=x:m",
	}
	for in, want := range cases {
		if got := stripControlRunes(in); got != want {
			t.Errorf("stripControlRunes(%q) = %q, want %q", in, got, want)
		}
	}
}
