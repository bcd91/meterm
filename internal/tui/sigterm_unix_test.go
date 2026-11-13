//go:build !windows

package tui

import (
	"errors"
	"io"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type idle struct{}

func (idle) Init() tea.Cmd                       { return nil }
func (idle) Update(tea.Msg) (tea.Model, tea.Cmd) { return idle{}, nil }
func (idle) View() string                        { return "" }

// A TERM ends the picker with ErrTerminated (main exits 143), not as the
// person quitting; a HUP ends it too, and isn't taken for a TERM. The
// signals go to this test process only.
func TestPickerEndsNonZeroOnSIGTERM(t *testing.T) {
	for _, s := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		p := tea.NewProgram(idle{}, tea.WithInput(nil), tea.WithOutput(io.Discard))
		go func() {
			time.Sleep(200 * time.Millisecond)
			_ = syscall.Kill(syscall.Getpid(), s)
		}()
		res := make(chan error, 1)
		go func() { _, err := runProgram(p); res <- err }()
		select {
		case err := <-res:
			if s == syscall.SIGTERM && !errors.Is(err, ErrTerminated) {
				t.Errorf("SIGTERM: %v, want ErrTerminated", err)
			}
			if s == syscall.SIGHUP && (err == nil || errors.Is(err, ErrTerminated)) {
				t.Errorf("SIGHUP: %v, want the program killed", err)
			}
		case <-time.After(5 * time.Second):
			p.Kill()
			t.Fatalf("%v didn't end the picker", s)
		}
	}
}
