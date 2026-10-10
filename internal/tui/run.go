package tui

import (
	"context"
	"errors"
	"io"

	"airlock/internal/transport"
	tea "charm.land/bubbletea/v2"
)

// Run owns its subscription and terminal program. Exiting the UI never cancels
// requests submitted by hooks on other connections.
func Run(ctx context.Context, c transport.Client, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	f := newFeed(ctx, c)
	go f.run()
	defer func() { cancel(); <-f.done }()
	model := newModel(ctx, f)
	model.socket = c.Socket
	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out), tea.WithoutSignalHandler())
	_, err := program.Run()
	if ctx.Err() != nil && errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	return err
}
