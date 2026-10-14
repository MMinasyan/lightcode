//go:build linux

package cliclient

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func pty(t *testing.T) (*os.File, *os.File, *unix.Termios) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	master := os.NewFile(uintptr(fd), "ptmx")
	t.Cleanup(func() { master.Close() })
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	old, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return master, slave, old
}
func rawRestored(t *testing.T, slave *os.File, old *unix.Termios) {
	t.Helper()
	now, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(now, old) {
		t.Fatalf("raw mode not restored: before %#v after %#v", old, now)
	}
}
func TestPTYRawRestorationNormalEOFAndActualSignal(t *testing.T) {
	for _, exit := range []string{"normal", "EOF", "signal"} {
		t.Run(exit, func(t *testing.T) {
			live := liveFixture(t, false, nil)
			master, slave, old := pty(t)
			c := New(Options{Startup: live.opts, Workspace: "/workspace"})
			screen := newScreen(80)
			done := runClient(c, slave, screen)
			frameContaining(t, screen, "canonical clipboard")
			now, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil || now.Lflag&unix.ICANON != 0 {
				t.Fatal("Run did not establish raw mode")
			}
			switch exit {
			case "normal":
				_, _ = master.Write([]byte("/exit\r"))
			case "EOF":
				// A hung-up PTY rejects TCGETS/TCSETS with EIO, so it cannot
				// verify restoration. Enable only canonical EOF admission in
				// the native fixture and send VEOF: Read returns zero while
				// the descriptor remains usable. The remaining raw flags must
				// still be restored by Run, not by this input stimulus.
				now.Lflag |= unix.ICANON
				if err := unix.IoctlSetTermios(int(slave.Fd()), unix.TCSETS, now); err != nil {
					t.Fatal(err)
				}
				// Complete the already parked raw read with one byte, then
				// supply an empty canonical record to the next native read.
				_, _ = master.Write([]byte{'x', old.Cc[unix.VEOF], old.Cc[unix.VEOF]})
			case "signal":
				if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			err = receive(t, done)
			if exit == "signal" {
				var e ExitError
				if !errors.As(err, &e) || e.Code != 130 {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			rawRestored(t, slave, old)
			assertNoImplicitControls(t, live)
		})
	}
}

// TestPTYInputPressureSignalReleasesAndRestores proves one real terminal under
// valid-input pressure: the pasted keys fill the bounded queue while the writer
// is held, the signal releases the capacity-blocked reader, and Run restores raw
// mode without the held output ever being released.
func TestPTYInputPressureSignalReleasesAndRestores(t *testing.T) {
	live := liveFixture(t, false, nil)
	master, slave, old := pty(t)
	blocked := &blockedWriter{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(blocked.release)
	c := New(Options{Startup: live.opts, Workspace: "/workspace"})
	done := runClient(c, slave, blocked)
	barrier(t, blocked.entered)
	go func() { _, _ = master.Write([]byte(strings.Repeat("x", 8192))) }()
	barrierUntilFull(t, c)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var exit ExitError
	if err := receive(t, done); !errors.As(err, &exit) || exit.Code != 130 {
		t.Fatal(err)
	}
	rawRestored(t, slave, old)
	assertNoImplicitControls(t, live)
}

func TestPTYBlockedCopyIdleActiveSIGINTRestoresWithoutWriterJoin(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprint(active), func(t *testing.T) {
			interrupt := make(chan struct{}, 1)
			live := liveFixture(t, active, func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasSuffix(r.URL.Path, "/interrupt") {
					interrupt <- struct{}{}
					w.WriteHeader(204)
					return true
				}
				return false
			})
			master, slave, old := pty(t)
			blocked := &blockedWriter{entered: make(chan struct{}), release: make(chan struct{}), copyOnly: true, ready: make(chan struct{})}
			defer close(blocked.release)
			c := New(Options{Startup: live.opts, Workspace: "/workspace"})
			done := runClient(c, slave, blocked)
			barrier(t, blocked.ready)
			_, _ = master.Write([]byte("/copy\r"))
			barrier(t, blocked.entered)
			if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
				t.Fatal(err)
			}
			if active {
				barrier(t, interrupt)
				if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			var exit ExitError
			if err := receive(t, done); !errors.As(err, &exit) || exit.Code != 130 {
				t.Fatal(err)
			}
			rawRestored(t, slave, old)
		})
	}
}
