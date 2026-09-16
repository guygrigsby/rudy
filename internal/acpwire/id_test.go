package acpwire

import (
	"strings"
	"testing"
)

func TestExactIDs(t *testing.T) {
	for _, tt := range []struct{ raw, canonical string }{
		{`"a"`, "a"}, {`"\u0061"`, "a"}, {"1", "1"}, {"1.0", "1"}, {"1e0", "1"}, {"-0", "0"}, {"null", ""},
		{"9223372036854775807", "9223372036854775807"}, {"-9223372036854775808", "-9223372036854775808"},
		{"92233720368547758070e-1", "9223372036854775807"}, {"-9223372036854775808.000", "-9223372036854775808"},
		{"0e4096", "0"},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			id, err := ParseID([]byte(tt.raw))
			if err != nil || id.Value != tt.canonical {
				t.Fatalf("unexpected canonical id or error: %v", err)
			}
		})
	}
	for _, raw := range []string{"1.1", "9223372036854775808", "-9223372036854775809", "1e4097", "1e-4097", "1e-1", "true", "[]", "01", `"` + strings.Repeat("x", 4097) + `"`, strings.Repeat("1", 4097), "0e" + strings.Repeat("0", 4096)} {
		if _, err := ParseID([]byte(raw)); err == nil {
			t.Fatal("accepted invalid id")
		}
	}
	a, _ := ParseID([]byte(`"a"`))
	b, _ := ParseID([]byte(`"\u0061"`))
	if a != b {
		t.Fatal("escaped strings differ")
	}
	n, _ := ParseID([]byte("null"))
	s, _ := ParseID([]byte(`""`))
	if n == s {
		t.Fatal("null collides with empty string")
	}
}
