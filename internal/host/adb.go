package host

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// ADB permits replacing the process boundary in tests, without simulated devices
// in the production server.
type ADB interface {
	Run(context.Context, ...string) ([]byte, error)
	Start(context.Context, ...string) (Process, error)
}
type Process interface {
	Stop() error
	Done() <-chan struct{}
}
type ExecADB struct{ Path string }

type adbTarget struct{ serial, transportID string }

func (t adbTarget) args(args ...string) []string {
	if t.transportID != "" {
		return append([]string{"-t", t.transportID}, args...)
	}
	return append([]string{"-s", t.serial}, args...)
}

func (a ExecADB) command(ctx context.Context, args ...string) *exec.Cmd {
	path := a.Path
	if path == "" {
		path = "adb"
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = slices.DeleteFunc(cmd.Environ(), func(entry string) bool {
		return strings.HasPrefix(entry, "MARIONETTE_AGENT_TOKEN=")
	})
	return cmd
}
func (a ExecADB) Run(ctx context.Context, args ...string) ([]byte, error) {
	command := a.command(ctx, args...)
	var stderr limitedBuffer
	command.Stderr = &stderr
	b, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("adb: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return b, nil
}
func (a ExecADB) Start(ctx context.Context, args ...string) (Process, error) {
	c := a.command(ctx, args...)
	c.Stdout = io.Discard
	c.Stderr = os.Stderr
	if err := c.Start(); err != nil {
		return nil, err
	}
	p := &execProcess{cmd: c, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		defer logPanic("wait for adb")
		_ = c.Wait()
	}()
	return p, nil
}

type execProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (p *execProcess) Stop() error           { return p.cmd.Process.Kill() }
func (p *execProcess) Done() <-chan struct{} { return p.done }

// adb shell joins its arguments into a remote shell command. Quote every value
// at that second parsing boundary as well as using exec argument arrays.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
