package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// Colour codes are written directly rather than pulled from a dependency: the
// console needs six of them and nothing else.
const (
	sgrReset = "\x1b[0m"
	sgrBold  = "\x1b[1m"
	sgrDim   = "\x1b[2m"
	sgrRed   = "\x1b[31m"
	sgrGreen = "\x1b[32m"
	sgrAmber = "\x1b[33m"
	sgrCyan  = "\x1b[36m"
)

// console writes the demo output and reads the operator's choices.
type console struct {
	out      io.Writer
	in       *bufio.Scanner
	coloured bool
}

// newConsole returns a console on the terminal, with colour unless the output
// is redirected or NO_COLOR asks for none.
func newConsole() *console {
	return &console{
		out:      os.Stdout,
		in:       bufio.NewScanner(os.Stdin),
		coloured: isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == "",
	}
}

// isTerminal reports whether f is attached to a terminal rather than a file.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// paint wraps text in the given codes, or returns it untouched without colour.
func (c *console) paint(text string, codes ...string) string {
	if !c.coloured || len(codes) == 0 {
		return text
	}
	return strings.Join(codes, "") + text + sgrReset
}

// printf writes a line. The console is the program's output, so a failed write
// leaves nothing to report it with and is ignored deliberately.
func (c *console) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(c.out, format, args...)
}

func (c *console) blank() {
	c.printf("\n")
}

// heading starts a section.
func (c *console) heading(text string) {
	c.printf("\n%s\n", c.paint(text, sgrBold))
}

// step reports something the console is about to do or has just done.
func (c *console) step(format string, args ...any) {
	c.printf("  %s\n", fmt.Sprintf(format, args...))
}

// mark writes a line led by a coloured symbol.
func (c *console) mark(symbol, colour, format string, args ...any) {
	c.printf("  %s %s\n", c.paint(symbol, colour), fmt.Sprintf(format, args...))
}

func (c *console) ok(format string, args ...any)   { c.mark("✓", sgrGreen, format, args...) }
func (c *console) warn(format string, args ...any) { c.mark("!", sgrAmber, format, args...) }
func (c *console) fail(format string, args ...any) { c.mark("✗", sgrRed, format, args...) }

// note writes a dimmed aside.
func (c *console) note(format string, args ...any) {
	c.printf("  %s\n", c.paint(fmt.Sprintf(format, args...), sgrDim))
}

// echo shows a command before it runs, so that whoever is watching sees what
// the console actually does and can repeat it by hand.
func (c *console) echo(command string) {
	c.printf("\n  %s %s\n", c.paint("$", sgrCyan), c.paint(command, sgrCyan))
}

// ask prints a prompt and returns the trimmed answer.
func (c *console) ask(prompt string) string {
	c.printf("\n%s ", c.paint(prompt, sgrBold))
	if !c.in.Scan() {
		return "q"
	}
	return strings.TrimSpace(c.in.Text())
}

// confirm asks a yes or no question that defaults to no.
func (c *console) confirm(prompt string) bool {
	answer := strings.ToLower(c.ask(prompt + " [y/N]"))
	return answer == "y" || answer == "yes"
}
