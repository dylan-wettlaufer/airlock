package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"airlock/internal/transport"
	"airlock/internal/tui"
)

func tuiCommand(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer) int {
	flags := flag.NewFlagSet("tui", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	socket := flags.String("socket", transport.DefaultSocket(), "private daemon socket")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(diagnostics, "airlock: usage: airlock tui [--socket PATH]")
		return 1
	}
	if err := tui.Run(ctx, transport.Client{Socket: *socket}, in, out); err != nil {
		fmt.Fprintln(diagnostics, "airlock: could not run terminal UI:", err)
		return 1
	}
	return 0
}
