package acpext

import (
	"encoding/json"
	"testing"
)

func TestInitializeMetadataUsesTheContractShape(t *testing.T) {
	value := Metadata[InitializeRequest]{Rudy: InitializeRequest{
		Version:      Version,
		Asker:        true,
		Capabilities: []string{"session.update"},
	}}
	got, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"_rudy":{"version":1,"asker":true,"capabilities":["session.update"]}}`
	if string(got) != want {
		t.Fatalf("metadata = %s, want %s", got, want)
	}
}

func TestInitializeResponseOmitsControlForANormalConnection(t *testing.T) {
	value := Metadata[InitializeResponse]{Rudy: InitializeResponse{
		Version:      Version,
		Capabilities: []string{"session.update"},
		Home:         "/home/guy",
		InstanceID:   "01K...",
		RudyVersion:  "v0.1.0",
	}}
	got, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"_rudy":{"version":1,"capabilities":["session.update"],"home":"/home/guy","instanceId":"01K...","rudyVersion":"v0.1.0"}}`
	if string(got) != want {
		t.Fatalf("normal initialize response = %s, want %s", got, want)
	}
}

func TestInitializeResponsePreservesShutdownControl(t *testing.T) {
	want := `{"_rudy":{"version":1,"control":"shutdown","capabilities":["server.shutdown"],"home":"/home/guy","instanceId":"01K...","rudyVersion":"v0.1.0"}}`
	value := Metadata[InitializeResponse]{Rudy: InitializeResponse{
		Version:      Version,
		Control:      "shutdown",
		Capabilities: []string{"server.shutdown"},
		Home:         "/home/guy",
		InstanceID:   "01K...",
		RudyVersion:  "v0.1.0",
	}}
	got, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("shutdown initialize response = %s, want %s", got, want)
	}

	var decoded Metadata[InitializeResponse]
	if err := json.Unmarshal([]byte(want), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Rudy.Control != "shutdown" {
		t.Fatalf("decoded control = %q, want shutdown", decoded.Rudy.Control)
	}
}

func TestOpenMetadataDistinguishesAbsentAndEmptyTools(t *testing.T) {
	absent, err := json.Marshal(Metadata[OpenMetadata]{Rudy: OpenMetadata{Open: OpenOptions{}}})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := json.Marshal(Metadata[OpenMetadata]{Rudy: OpenMetadata{Open: OpenOptions{Tools: []string{}}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(absent) != `{"_rudy":{"open":{"tools":null}}}` {
		t.Fatalf("absent tools = %s", absent)
	}
	if string(empty) != `{"_rudy":{"open":{"tools":[]}}}` {
		t.Fatalf("empty tools = %s", empty)
	}
}

func TestCommandRunOmitsStopReasonUntilItsTurnEnds(t *testing.T) {
	got, err := json.Marshal(CommandRunResult{TurnID: "01K", Notice: "started", SessionID: "01S"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"turnId":"01K","notice":"started","sessionId":"01S"}`
	if string(got) != want {
		t.Fatalf("command result = %s, want %s", got, want)
	}
}

func TestRegistryListOmitsFailures(t *testing.T) {
	got, err := json.Marshal(RegistryResult{FetchedAt: "2026-09-15T00:00:00Z", Models: []RegistryModel{}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"fetchedAt":"2026-09-15T00:00:00Z","models":[]}`
	if string(got) != want {
		t.Fatalf("registry list result = %s, want %s", got, want)
	}
}

func TestSuccessfulRegistryRefreshRequiresEmptyFailures(t *testing.T) {
	got, err := json.Marshal(RegistryRefreshResult{
		RegistryResult: RegistryResult{FetchedAt: "2026-09-15T00:00:00Z", Models: []RegistryModel{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"fetchedAt":"2026-09-15T00:00:00Z","models":[],"failures":[]}`
	if string(got) != want {
		t.Fatalf("successful registry refresh result = %s, want %s", got, want)
	}
}

func TestFailedRegistryRefreshUsesFixedPublicText(t *testing.T) {
	got, err := json.Marshal(RegistryRefreshResult{
		RegistryResult: RegistryResult{FetchedAt: "2026-09-15T00:00:00Z", Models: []RegistryModel{}},
		Failures: RefreshFailures{{
			Provider: "aperture",
			Error:    RefreshFailureText,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"fetchedAt":"2026-09-15T00:00:00Z","models":[],"failures":[{"provider":"aperture","error":"Refresh failed; see box log"}]}`
	if string(got) != want {
		t.Fatalf("failed registry refresh result = %s, want %s", got, want)
	}
}
