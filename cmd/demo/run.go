package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// command is an external command the console runs on the operator's behalf.
type command struct {
	name string
	args []string
}

// String renders the command the way it would be typed.
func (c command) String() string {
	return strings.Join(append([]string{c.name}, c.args...), " ")
}

// run executes the command with its output going straight to the terminal.
func (c command) run(ctx context.Context) error {
	process := exec.CommandContext(ctx, c.name, c.args...)
	process.Stdout, process.Stderr = os.Stdout, os.Stderr
	if err := process.Run(); err != nil {
		return fmt.Errorf("%s: %w", c, err)
	}
	return nil
}

// output executes the command and returns what it wrote, for the cases where
// the console reads the result rather than showing it.
func (c command) output(ctx context.Context) (string, error) {
	process := exec.CommandContext(ctx, c.name, c.args...)
	process.Stderr = os.Stderr

	out, err := process.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", c, err)
	}
	return string(out), nil
}
