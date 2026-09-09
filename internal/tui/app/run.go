package app

import (
	"context"

	tea "charm.land/bubbletea/v2"
)

// Run draws the client until it quits, and returns whatever ended it. The program is the
// plain one: the alt screen and the mouse mode are the view's own (Model.View), read from
// ui.render on every frame, so a caller cannot set them to something the config did not
// ask for. ctx ends the program the way a cancelled parent ends any other command.
func Run(ctx context.Context, o Options) error {
	p := tea.NewProgram(New(o), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}
