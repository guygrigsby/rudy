package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestDaemonCapabilitiesMatchContract(t *testing.T) {
	want := daemonCapabilitiesFromContract(t)
	got := DaemonCapabilities()
	if !slices.IsSorted(got) {
		t.Fatalf("daemon capabilities are not sorted: %q", got)
	}
	for i := range len(got) - 1 {
		if got[i+1] == got[i] {
			t.Fatalf("daemon capabilities contain duplicate %q", got[i+1])
		}
	}
	for _, capability := range want {
		if !slices.Contains(got, capability) {
			t.Errorf("contract capability %q is missing from protocol", capability)
		}
	}
	for _, capability := range got {
		if !slices.Contains(want, capability) {
			t.Errorf("protocol capability %q is absent from contract", capability)
		}
	}
}

func TestOldHelloWithoutCapabilitiesDecodesAsEmptySet(t *testing.T) {
	var hello ClientHelloResult
	if err := json.Unmarshal([]byte(`{"server":"rudy","version":"old","instance_id":"01K","home":"/home/guy"}`), &hello); err != nil {
		t.Fatal(err)
	}
	if len(hello.Capabilities) != 0 {
		t.Fatalf("old hello capabilities = %q, want empty", hello.Capabilities)
	}
}

func TestDaemonCapabilitiesReturnsACopy(t *testing.T) {
	first := DaemonCapabilities()
	first[0] = "changed"
	if DaemonCapabilities()[0] == "changed" {
		t.Fatal("caller mutated the closed daemon capability vocabulary")
	}
}

func daemonCapabilitiesFromContract(t *testing.T) []string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("find test source")
	}
	path := filepath.Clean(filepath.Join(filepath.Dir(file), "../../docs/specs/rudy-contracts.md"))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	const heading = "### Internal daemon capabilities"
	start := strings.Index(text, heading)
	if start < 0 {
		t.Fatalf("%s has no %q heading", path, heading)
	}
	var names []string
	inTable := false
	for _, line := range strings.Split(text[start+len(heading):], "\n") {
		if !strings.HasPrefix(line, "|") {
			if inTable {
				break
			}
			continue
		}
		columns := strings.Split(line, "|")
		if len(columns) < 4 {
			continue
		}
		name := strings.Trim(strings.TrimSpace(columns[1]), "`")
		if name == "name" || strings.HasPrefix(name, "---") {
			continue
		}
		inTable = true
		names = append(names, name)
	}
	if len(names) == 0 {
		t.Fatal("internal daemon capability table is empty")
	}
	slices.Sort(names)
	return names
}
