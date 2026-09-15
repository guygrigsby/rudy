package acpschema_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/acpschema"
	"github.com/guygrigsby/rudy/internal/acptest"
)

func TestPinnedStableSchemaIdentity(t *testing.T) {
	if acpschema.Version != "0.13.5" {
		t.Fatalf("schema version = %q, want 0.13.5", acpschema.Version)
	}
	digest := sha256.Sum256(acpschema.Bytes())
	got := hex.EncodeToString(digest[:])
	if got != acpschema.SHA256 {
		t.Fatalf("schema digest = %s, recorded %s", got, acpschema.SHA256)
	}
	if got != "0da9fe718746ccc2e8ef789efa6687e64a252dea3a8789faae9a1acbe60e0d3b" {
		t.Fatalf("schema digest = %s, want ACP SDK v0.13.5 digest", got)
	}
}

func TestACPModulePinMatchesSchemaVersion(t *testing.T) {
	root := repositoryRoot(t)
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	const module = "github.com/coder/acp-go-sdk"
	var versions []string
	for _, line := range strings.Split(string(goMod), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == module {
			versions = append(versions, fields[1])
		}
	}
	want := "v" + acpschema.Version
	if len(versions) != 1 || versions[0] != want {
		t.Fatalf("go.mod versions for %s = %q, want [%q]", module, versions, want)
	}
}

func TestValidateAcceptsStableACPEnvelopes(t *testing.T) {
	for name, fixture := range map[string][]byte{
		"initialize request":  acptest.InitializeRequest,
		"initialize response": acptest.InitializeResponse,
		"cancel notification": acptest.CancelNotification,
	} {
		t.Run(name, func(t *testing.T) {
			if err := acpschema.Validate(fixture); err != nil {
				t.Fatalf("validate stable fixture: %v", err)
			}
		})
	}
}

func TestValidateRejectsWrongProtocolVersion(t *testing.T) {
	bad := []byte(`{"protocolVersion":65536,"clientCapabilities":{}}`)
	if err := acpschema.ValidateDefinition("InitializeRequest", bad); err == nil {
		t.Fatal("protocolVersion above uint16 maximum passed schema validation")
	}
}

func TestSchemaValidationDoesNotEnforceInt64Format(t *testing.T) {
	wide := []byte(`{"jsonrpc":"2.0","id":9223372036854775808,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}`)
	if err := acpschema.Validate(wide); err != nil {
		t.Fatalf("format is intentionally not enforced by jsonschema-go; framing must check numeric width separately: %v", err)
	}
}

func TestCheckedInSchemaVersionFile(t *testing.T) {
	root := repositoryRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "internal/acpschema/version"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != acpschema.Version {
		t.Fatalf("version file = %q, want %q", strings.TrimSpace(string(b)), acpschema.Version)
	}
}

func TestCheckedInSchemaDigestFile(t *testing.T) {
	root := repositoryRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "internal/acpschema/schema.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 || fields[0] != acpschema.SHA256 || fields[1] != "schema.json" {
		t.Fatalf("schema.sha256 = %q, want recorded digest and schema.json", strings.TrimSpace(string(b)))
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
