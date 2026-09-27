// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"encoding/json"

	tea "charm.land/bubbletea/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
)

// NotificationMsg is one server notification on its way into the update loop.
type NotificationMsg protocol.Notification

// DisconnectedMsg is the pump's last message: the server's notification channel closed,
// so nothing more is coming. Err carries a reason when there is one; a channel that
// simply ended carries none.
type DisconnectedMsg struct{ Err error }

type browserOpenedMsg struct {
	SessionID string
	Runtime   string
	LoginID   string
	Err       error
}

// CallResultMsg is one server call's answer, delivered to Update rather than acted on in
// the goroutine that made the call: the model is owned by the update loop and nothing
// else may touch it. Result is the raw result, which the case for the method decodes.
type CallResultMsg struct {
	Method    string
	SessionID string
	// Name is what the call was about when the method alone does not say: the command a
	// command.run ran. The answer does not carry it, and a client that has to know which
	// answer was /help's cannot read it back off the wire.
	Name   string
	Result json.RawMessage
	Err    error
	// Submitted is the typed message session.submit accepted. Its response carries the
	// authoritative turn id, which is also a fresh turn's user_message entry id, so the
	// update loop can draw that row before the independently pumped notification lands.
	Submitted *session.UserMessage
}

// pump reads one notification and hands it over, or reports the connection gone. It reads
// exactly one: the model re-arms it after each, so notifications arrive in order and are
// folded in one at a time, on the update loop's goroutine.
func pump(c *protocol.Client) tea.Cmd {
	notes := c.Notifications()
	return func() tea.Msg {
		n, ok := <-notes
		if !ok {
			return DisconnectedMsg{}
		}
		return NotificationMsg(n)
	}
}

// call makes one server call as a command. The context is the background one on purpose:
// a call outlives the keystroke that started it, and the client's Close is what ends a
// call the program no longer wants.
func (m *Model) call(method string, params any) tea.Cmd {
	return m.callNamed(method, "", params)
}

// callNamed is call carrying what the call is about into its answer (see
// CallResultMsg.Name).
func (m *Model) callNamed(method, name string, params any) tea.Cmd {
	c := m.cl
	return func() tea.Msg {
		var raw json.RawMessage
		err := c.Call(context.Background(), method, params, &raw)
		return CallResultMsg{Method: method, Name: name, Result: raw, Err: err}
	}
}

// callSubmit keeps the submitted value beside the response. Notifications and responses
// are delivered by independent Bubble Tea commands, so entry.appended may already be queued
// while the response wins the race into Update.
func (m *Model) callSubmit(msg session.UserMessage) tea.Cmd {
	c := m.cl
	sid := m.session.SessionID
	return func() tea.Msg {
		var raw json.RawMessage
		err := c.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
			SessionID: sid,
			Content:   msg.Content,
			Source:    msg.Source,
		}, &raw)
		return CallResultMsg{
			Method: protocol.MethodSessionSubmit, SessionID: sid,
			Result: raw, Err: err, Submitted: &msg,
		}
	}
}
