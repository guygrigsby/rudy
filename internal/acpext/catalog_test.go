package acpext

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestGeneratedCatalogueMatchesContract(t *testing.T) {
	root := repositoryRoot(t)
	want := parseExtensionTable(t, filepath.Join(root, "docs/specs/rudy-contracts.md"))
	got := All()

	if len(got) != len(want) {
		t.Fatalf("catalogue has %d entries, contract has %d", len(got), len(want))
	}
	for capability, contract := range want {
		entry, ok := ByCapability(capability)
		if !ok {
			t.Errorf("contract capability %q is missing from catalogue", capability)
			continue
		}
		if entry.Method != contract.method || entry.Notification != contract.notification || entry.Side != contract.side {
			t.Errorf("capability %q = {%q, notification:%t, side:%q}, want {%q, notification:%t, side:%q}", capability, entry.Method, entry.Notification, entry.Side, contract.method, contract.notification, contract.side)
		}
	}
	for _, entry := range got {
		contract, ok := want[entry.Capability]
		if !ok {
			t.Errorf("catalogue capability %q is absent from contract", entry.Capability)
			continue
		}
		if entry.Method != contract.method || entry.Notification != contract.notification || entry.Side != contract.side {
			t.Errorf("catalogue entry for %q = {%q, notification:%t, side:%q}, contract has {%q, notification:%t, side:%q}", entry.Capability, entry.Method, entry.Notification, entry.Side, contract.method, contract.notification, contract.side)
		}
	}
}

func TestGeneratedCatalogueIsSortedAndDuplicateFree(t *testing.T) {
	assertSortedUnique(t, "capabilities", CapabilityNames())
	assertSortedUnique(t, "methods", MethodNames())
}

func TestGeneratedCatalogueIsCurrent(t *testing.T) {
	root := repositoryRoot(t)
	temporary := filepath.Join(t.TempDir(), "catalog_gen.go")
	cmd := exec.Command("go", "run", "./internal/acpext/cmd/cataloggen",
		"-contract", "docs/specs/rudy-contracts.md",
		"-output", temporary,
	)
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("regenerate catalogue: %v\n%s", err, output)
	}

	got, err := os.ReadFile(temporary)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(root, "internal/acpext/catalog_gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("internal/acpext/catalog_gen.go is stale; run go generate ./internal/acpext")
	}
}

func TestCanonicalCapabilitiesSortsAndRemovesDuplicates(t *testing.T) {
	got := CanonicalCapabilities([]string{"session.update", "command.run", "session.update", "command.list"})
	want := []string{"command.list", "command.run", "session.update"}
	if !slices.Equal(got, want) {
		t.Fatalf("canonical capabilities = %q, want %q", got, want)
	}
}

type contractEntry struct {
	method       string
	notification bool
	side         string
}

func parseExtensionTable(t *testing.T, path string) map[string]contractEntry {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const heading = "#### Rudy extension methods"
	text := string(b)
	start := strings.Index(text, heading)
	if start < 0 {
		t.Fatalf("%s has no %q heading", path, heading)
	}

	rows := make(map[string]contractEntry)
	inTable := false
	for _, line := range strings.Split(text[start+len(heading):], "\n") {
		if !strings.HasPrefix(line, "|") {
			if inTable {
				break
			}
			continue
		}
		columns := strings.Split(line, "|")
		if len(columns) < 6 {
			continue
		}
		capability := strings.Trim(strings.TrimSpace(columns[1]), "`")
		if capability == "Capability" || strings.HasPrefix(capability, "---") {
			continue
		}
		methodColumn := strings.TrimSpace(columns[2])
		methodEnd := strings.Index(methodColumn[1:], "`")
		if !strings.HasPrefix(methodColumn, "`") || methodEnd < 0 {
			t.Fatalf("malformed extension method column %q", methodColumn)
		}
		method := methodColumn[1 : methodEnd+1]
		notification := strings.Contains(methodColumn[methodEnd+2:], "notification")
		side := strings.TrimSpace(columns[3])
		if side != "agent" && side != "client" {
			t.Fatalf("extension row %q has side %q, want agent or client", capability, side)
		}
		inTable = true
		if capability == "" || method == "" {
			t.Fatalf("malformed extension row %q", line)
		}
		if old, exists := rows[capability]; exists {
			t.Fatalf("duplicate capability %q maps to %q and %q", capability, old.method, method)
		}
		rows[capability] = contractEntry{method: method, notification: notification, side: side}
	}
	if len(rows) == 0 {
		t.Fatal("Rudy extension table is empty")
	}
	return rows
}

func assertSortedUnique(t *testing.T, name string, values []string) {
	t.Helper()
	if !slices.IsSorted(values) {
		t.Errorf("%s are not sorted: %q", name, values)
	}
	for i := range len(values) - 1 {
		if values[i+1] == values[i] {
			t.Errorf("%s contain duplicate %q", name, values[i+1])
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("find test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}
