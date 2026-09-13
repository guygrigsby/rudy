package hosts

import "testing"

func TestRevisionReadsGitDescribe(t *testing.T) {
	for in, want := range map[string]string{
		"v0.1.0-3-gabc1234": "abc1234",
		"abc1234":           "abc1234",
		"v0.2.0":            "v0.2.0",
	} {
		got, err := Revision(in)
		if err != nil || got != want {
			t.Fatalf("Revision(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestRevisionRefusesDirtyAndDev(t *testing.T) {
	for _, in := range []string{"v0.1.0-3-gabc1234-dirty", "abc1234-dirty", "dev", ""} {
		if _, err := Revision(in); err == nil {
			t.Fatalf("Revision(%q) accepted a version the box cannot check out", in)
		}
	}
}
