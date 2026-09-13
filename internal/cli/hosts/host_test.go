package hosts

import "testing"

func TestParseHostAcceptsAliasAndUserAtHost(t *testing.T) {
	for _, s := range []string{"box", "ubuntu@box", "box.local", "user@10.0.0.7"} {
		h, err := ParseHost(s)
		if err != nil {
			t.Fatalf("ParseHost(%q): %v", s, err)
		}
		if h.String() != s {
			t.Fatalf("ParseHost(%q).String() = %q", s, h.String())
		}
	}
}

func TestParseHostRefusesEmptyAndOptionLooking(t *testing.T) {
	for _, s := range []string{"", "-oProxyCommand=x", "-", " box"} {
		if _, err := ParseHost(s); err == nil {
			t.Fatalf("ParseHost(%q) accepted a value ssh would misread", s)
		}
	}
}
