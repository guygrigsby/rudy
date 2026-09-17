// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"

	tea "charm.land/bubbletea/v2"
)

// Run draws the client until it quits, and reports whether the session was at rest when it
// did, alongside whatever ended it. The program is the plain one: the alt screen and the mouse
// mode are the view's own (Model.View), read from ui.render on every frame, so a caller cannot
// set them to something the config did not ask for. ctx ends the program the way a cancelled
// parent ends any other command.
//
// Resting is the client's last word on the turn: a session quit mid-turn is one the server is
// still working on, and a remote caller must not bring the box's tree home while it is still
// being written. A final model this package did not produce cannot say, and answers no.
func Run(ctx context.Context, o Options) (resting bool, err error) {
	p := tea.NewProgram(New(o), tea.WithContext(ctx))
	final, err := p.Run()
	m, ok := final.(*Model)
	return ok && m.turn.resting(), err
}
