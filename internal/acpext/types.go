package acpext

import "encoding/json"

// Version is the negotiated Rudy extension version.
const Version = 1

// Metadata is the reserved ACP metadata envelope for a Rudy-owned value.
type Metadata[T any] struct {
	Rudy T `json:"_rudy"`
}

// InitializeRequest is the Rudy-owned initialize offer inside _meta._rudy.
type InitializeRequest struct {
	Version      int      `json:"version"`
	Control      string   `json:"control,omitempty"`
	Asker        bool     `json:"asker"`
	Capabilities []string `json:"capabilities"`
}

// InitializeResponse is the negotiated Rudy initialize metadata returned by an agent.
type InitializeResponse struct {
	Version      int      `json:"version"`
	Control      string   `json:"control,omitempty"`
	Capabilities []string `json:"capabilities"`
	Home         string   `json:"home"`
	InstanceID   string   `json:"instanceId"`
	RudyVersion  string   `json:"rudyVersion"`
}

// OpenMetadata carries Rudy Session defaults on a standard ACP session/new request.
type OpenMetadata struct {
	Open OpenOptions `json:"open"`
}

// OpenOptions narrows the Session opened through ACP. A nil Tools value means no
// caller narrowing, while an empty non-nil slice narrows the Session to no tools.
type OpenOptions struct {
	Model    string   `json:"model,omitempty"`
	Mode     string   `json:"mode,omitempty"`
	Thinking string   `json:"thinking,omitempty"`
	Agent    string   `json:"agent,omitempty"`
	Tools    []string `json:"tools"`
}

// SequenceMetadata orders one negotiated Rudy event carrier on an ACP connection.
type SequenceMetadata struct {
	Version  int    `json:"version"`
	Sequence uint64 `json:"sequence"`
}

// PermissionOutcomeMetadata accompanies a negotiated permission selection.
type PermissionOutcomeMetadata struct {
	Version int    `json:"version"`
	Reason  string `json:"reason,omitempty"`
}

// ErrorMetadata carries only Rudy's safe closed error discriminator.
type ErrorMetadata struct {
	Version  int    `json:"version"`
	Kind     string `json:"kind,omitempty"`
	PromptID string `json:"promptId,omitempty"`
	TurnID   string `json:"turnId,omitempty"`
}

type ForkParams struct {
	SessionID string `json:"sessionId"`
	AtEntryID string `json:"atEntryId"`
}

// InitialSessionState keeps the standard ACP mode and config-option values raw.
// Their stable wire types belong to the SDK inside the adapters.
type InitialSessionState struct {
	SessionID     string            `json:"sessionId"`
	Modes         json.RawMessage   `json:"modes"`
	ConfigOptions []json.RawMessage `json:"configOptions"`
}

type SteerParams struct {
	SessionID string            `json:"sessionId"`
	Prompt    []json.RawMessage `json:"prompt"`
}

type TurnResult struct {
	TurnID string `json:"turnId"`
}

type CompactParams struct {
	SessionID    string `json:"sessionId"`
	Instructions string `json:"instructions,omitempty"`
}

type EntryResult struct {
	EntryID string `json:"entryId"`
}

type ShellParams struct {
	SessionID string `json:"sessionId"`
	Command   string `json:"command"`
}

type ShellResult struct {
	EntryID string `json:"entryId"`
	IsError bool   `json:"isError"`
}

type SetTitleParams struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
}

type CommandListParams struct{}

type Command struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type CommandListResult struct {
	Commands []Command `json:"commands"`
}

type CommandRunParams struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Args      string `json:"args"`
}

type CommandRunResult struct {
	TurnID     string `json:"turnId"`
	Notice     string `json:"notice"`
	SessionID  string `json:"sessionId"`
	StopReason string `json:"stopReason,omitempty"`
}

type RegistryParams struct {
	Provider string `json:"provider,omitempty"`
}

type RegistryResult struct {
	FetchedAt string          `json:"fetchedAt"`
	Models    []RegistryModel `json:"models"`
}

// RegistryRefreshResult always carries failures. A successful refresh encodes
// an empty array, while RegistryResult for registry.list omits the member.
type RegistryRefreshResult struct {
	RegistryResult
	Failures RefreshFailures `json:"failures"`
}

// RefreshFailures preserves the required empty-array representation when no
// providers failed, including for a zero-value RegistryRefreshResult.
type RefreshFailures []RefreshFailure

func (f RefreshFailures) MarshalJSON() ([]byte, error) {
	if f == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]RefreshFailure(f))
}

type RegistryModel struct {
	Ref           ModelRef          `json:"ref"`
	DisplayName   string            `json:"displayName"`
	ContextWindow int64             `json:"contextWindow"`
	MaxOutput     int64             `json:"maxOutput"`
	Pricing       Pricing           `json:"pricing"`
	Capabilities  ModelCapabilities `json:"capabilities"`
	Upstream      string            `json:"upstream,omitempty"`
}

type ModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type Pricing struct {
	Input      string `json:"input"`
	Output     string `json:"output"`
	CacheRead  string `json:"cacheRead"`
	CacheWrite string `json:"cacheWrite"`
}

type ModelCapabilities struct {
	Tools     bool `json:"tools"`
	Vision    bool `json:"vision"`
	Reasoning bool `json:"reasoning"`
}

type RefreshFailure struct {
	Provider string `json:"provider"`
	Error    string `json:"error"`
}

// RefreshFailureText is the only provider failure detail exposed by ACP.
const RefreshFailureText = "Refresh failed; see box log"

type ShutdownParams struct{}

type ShutdownResult struct {
	InstanceID string `json:"instanceId"`
	State      string `json:"state"`
}

// SessionUpdateParams is the common envelope for the closed process-event union.
// Payload remains raw until the adapter selects the negotiated kind.
type SessionUpdateParams struct {
	Meta    Metadata[SequenceMetadata] `json:"_meta"`
	Kind    string                     `json:"kind"`
	Payload json.RawMessage            `json:"payload"`
}
