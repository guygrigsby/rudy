// SPDX-License-Identifier: AGPL-3.0-or-later

// Package agentruntime defines the vendor-neutral port for execution backends that own
// their agent loop, canonical thread history and tools.
package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

var ErrAmbiguous = errors.New("agent runtime: operation outcome is ambiguous")

type LoginMode string

const (
	LoginBrowser LoginMode = "browser"
	LoginDevice  LoginMode = "device"
)

type ChallengeType string

const (
	ChallengeBrowser ChallengeType = "browser"
	ChallengeDevice  ChallengeType = "device"
)

type AccountState struct {
	Runtime       string `json:"runtime"`
	Authenticated bool   `json:"authenticated"`
	AuthMode      string `json:"auth_mode"`
	PlanType      string `json:"plan_type"`
}

type AuthChallenge struct {
	Type            ChallengeType `json:"type"`
	Runtime         string        `json:"runtime"`
	LoginID         string        `json:"login_id"`
	URL             string        `json:"url,omitempty"`
	VerificationURL string        `json:"verification_url,omitempty"`
	UserCode        string        `json:"user_code,omitempty"`
}

func NewBrowserChallenge(runtime, loginID, url string) (AuthChallenge, error) {
	if runtime == "" || loginID == "" || url == "" {
		return AuthChallenge{}, fmt.Errorf("agent runtime: browser challenge requires runtime, login id and url")
	}
	return AuthChallenge{Type: ChallengeBrowser, Runtime: runtime, LoginID: loginID, URL: url}, nil
}

func NewDeviceChallenge(runtime, loginID, verificationURL, userCode string) (AuthChallenge, error) {
	if runtime == "" || loginID == "" || verificationURL == "" || userCode == "" {
		return AuthChallenge{}, fmt.Errorf("agent runtime: device challenge requires runtime, login id, verification url and user code")
	}
	return AuthChallenge{
		Type: ChallengeDevice, Runtime: runtime, LoginID: loginID,
		VerificationURL: verificationURL, UserCode: userCode,
	}, nil
}

type ThreadRef struct {
	Runtime   string    `json:"runtime"`
	SessionID ulid.ULID `json:"session_id"`
	ThreadID  string    `json:"thread_id"`
}

type TurnRef struct {
	ThreadRef
	TurnID string `json:"turn_id"`
}

type StartThreadRequest struct {
	SessionID ulid.ULID
	Workspace session.Workspace
	Model     session.ModelRef
	Thinking  session.ThinkingLevel
	Mode      session.Mode
}

type StartTurnRequest struct {
	Thread   ThreadRef
	Content  []session.Block
	Model    session.ModelRef
	Thinking session.ThinkingLevel
	Mode     session.Mode
}

type SteerTurnRequest struct {
	Turn    TurnRef
	Content []session.Block
}

type TurnStatus string

const (
	TurnRunning     TurnStatus = "running"
	TurnCompleted   TurnStatus = "completed"
	TurnInterrupted TurnStatus = "interrupted"
	TurnFailed      TurnStatus = "failed"
)

type ItemType string

const (
	ItemUserMessage  ItemType = "user_message"
	ItemAgentMessage ItemType = "agent_message"
	ItemReasoning    ItemType = "reasoning"
	ItemCommand      ItemType = "command"
	ItemFileChange   ItemType = "file_change"
	ItemTool         ItemType = "tool"
	ItemPlan         ItemType = "plan"
	ItemDiff         ItemType = "diff"
	ItemWarning      ItemType = "warning"
	ItemError        ItemType = "error"
)

type FileChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type Item struct {
	ItemID  string          `json:"item_id"`
	Type    ItemType        `json:"type"`
	Status  string          `json:"status"`
	Content []session.Block `json:"content"`
	Command string          `json:"command"`
	CWD     string          `json:"cwd"`
	Output  string          `json:"output"`
	Changes []FileChange    `json:"changes"`
	Error   string          `json:"error"`
}

type Turn struct {
	TurnID string        `json:"turn_id"`
	Status TurnStatus    `json:"status"`
	Items  []Item        `json:"items"`
	Usage  session.Usage `json:"usage"`
}

type Thread struct {
	Runtime  string `json:"runtime"`
	ThreadID string `json:"thread_id"`
	Turns    []Turn `json:"turns"`
}

type EventType string

const (
	EventThreadStarted   EventType = "thread_started"
	EventThreadStatus    EventType = "thread_status"
	EventTurnStarted     EventType = "turn_started"
	EventTurnCompleted   EventType = "turn_completed"
	EventItemStarted     EventType = "item_started"
	EventItemDelta       EventType = "item_delta"
	EventItemCompleted   EventType = "item_completed"
	EventDiffUpdated     EventType = "diff_updated"
	EventPlanUpdated     EventType = "plan_updated"
	EventUsageUpdated    EventType = "usage_updated"
	EventWarning         EventType = "warning"
	EventError           EventType = "error"
	EventRequestResolved EventType = "request_resolved"
)

type Event struct {
	Type      EventType     `json:"type"`
	ThreadID  string        `json:"thread_id"`
	TurnID    string        `json:"turn_id"`
	ItemID    string        `json:"item_id"`
	RequestID string        `json:"request_id"`
	Sequence  uint64        `json:"sequence"`
	Item      Item          `json:"item"`
	Text      string        `json:"text"`
	Status    string        `json:"status"`
	Usage     session.Usage `json:"usage"`
}

type ApprovalKind string

const (
	ApprovalCommand     ApprovalKind = "command"
	ApprovalFileChange  ApprovalKind = "file_change"
	ApprovalPermissions ApprovalKind = "permissions"
)

type ApprovalScope string

const (
	ScopeOnce    ApprovalScope = "once"
	ScopeSession ApprovalScope = "session"
)

type ApprovalDecision string

const (
	DecisionAllow ApprovalDecision = "allow"
	DecisionDeny  ApprovalDecision = "deny"
)

type NetworkPermission struct {
	Host     string `json:"host"`
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
}

type ApprovalQuestion struct {
	Runtime       string              `json:"runtime"`
	RequestID     string              `json:"request_id"`
	ThreadID      string              `json:"thread_id"`
	TurnID        string              `json:"turn_id"`
	ItemID        string              `json:"item_id"`
	Kind          ApprovalKind        `json:"kind"`
	Summary       string              `json:"summary"`
	Command       string              `json:"command"`
	CWD           string              `json:"cwd"`
	Reason        string              `json:"reason"`
	Changes       []FileChange        `json:"changes"`
	Network       []NetworkPermission `json:"network"`
	Permissions   []string            `json:"permissions"`
	AllowedScopes []ApprovalScope     `json:"allowed_scopes"`
}

type ApprovalAnswer struct {
	Decision ApprovalDecision `json:"decision"`
	Scope    ApprovalScope    `json:"scope"`
	Reason   string           `json:"reason"`
	Granted  []string         `json:"granted"`
}

type LoginCompletion struct {
	Runtime string `json:"runtime"`
	LoginID string `json:"login_id"`
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

type Sink interface {
	RuntimeEvent(Event)
	AccountUpdated(AccountState)
	LoginCompleted(LoginCompletion)
	RequestApproval(context.Context, ApprovalQuestion) (ApprovalAnswer, error)
}

type ProjectedEntry struct {
	ID       ulid.ULID       `json:"id"`
	At       time.Time       `json:"at"`
	Kind     ItemType        `json:"kind"`
	Runtime  string          `json:"runtime"`
	ThreadID string          `json:"thread_id"`
	TurnID   string          `json:"turn_id"`
	ItemID   string          `json:"item_id"`
	Content  []session.Block `json:"content"`
	Status   string          `json:"status"`
	Usage    session.Usage   `json:"usage"`
}

type Runtime interface {
	provider.ModelSource
	Account(context.Context) (AccountState, error)
	StartLogin(context.Context, LoginMode) (AuthChallenge, error)
	CancelLogin(context.Context, string) error
	StartThread(context.Context, StartThreadRequest) (ThreadRef, error)
	ResumeThread(context.Context, ThreadRef) error
	ForkThread(context.Context, ThreadRef) (ThreadRef, error)
	ReadThread(context.Context, ThreadRef) (Thread, error)
	StartTurn(context.Context, StartTurnRequest) (TurnRef, error)
	SteerTurn(context.Context, SteerTurnRequest) (TurnRef, error)
	InterruptTurn(context.Context, TurnRef) error
	SetSink(Sink)
}
