package protocol

import "slices"

const (
	CapabilityBoundedProcessEventsV1   = "bounded_process_events_v1"
	CapabilityBoundedSessionEventsV1   = "bounded_session_events_v1"
	CapabilityBoundedSessionListV1     = "bounded_session_list_v1"
	CapabilityTerminalTurnDurabilityV1 = "terminal_turn_durability_v1"
)

var daemonCapabilities = [...]string{
	CapabilityBoundedProcessEventsV1,
	CapabilityBoundedSessionEventsV1,
	CapabilityBoundedSessionListV1,
	CapabilityTerminalTurnDurabilityV1,
}

// DaemonCapabilities returns every name in the closed internal capability vocabulary.
// A Server advertises only the subset whose complete guarantee it implements.
func DaemonCapabilities() []string {
	return slices.Clone(daemonCapabilities[:])
}
