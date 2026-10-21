// SPDX-License-Identifier: MPL-2.0

package windowsbuild

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The fake QEMU ignores the shutdown request and remains alive after TERM until
// this test releases its pipe. The production helper must join that child before
// deleting its overlay, including when the child's terminal status is nonzero.
func TestShutdownKeepsOverlayUntilOwnedQEMUJoins(t *testing.T) {
	for _, childExit := range []int{0, 7} {
		t.Run(fmt.Sprintf("child-exit-%d", childExit), func(t *testing.T) {
			temporaryDirectory := t.TempDir()
			overlay := filepath.Join(temporaryDirectory, "overlay", "overlay.qcow2")
			controlRead, controlWrite, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer controlRead.Close()
			defer controlWrite.Close()
			releaseRead, releaseWrite, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer releaseRead.Close()
			defer releaseWrite.Close()

			// The deadline bounds a broken fixture; pass/fail uses ordered pipe
			// events and actual child joins, never an elapsed-time assertion.
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			script := `
set -euo pipefail
source "$1"
WIN_RUN_DIR="$2/overlay"
WIN_MON_SOCK="$2/monitor.sock"
mkdir "$WIN_RUN_DIR"
printf 'owned overlay\n' >"$WIN_RUN_DIR/overlay.qcow2"

win_ssh_probe() { :; }
seq() { printf '1\n'; }
sleep() { :; }
wait() {
  local result=0
  printf 'wait-enter %s\n' "$1"
  builtin wait "$@" || result="$?"
  printf 'wait-return %s\n' "$result"
  return "$result"
}

bash -c '
  trap '\''printf "child-term\n"'\'' TERM
  printf "child-ready\n"
  while :; do
    token=""
    if IFS= read -r token <&3; then
      [ "$token" = release ] && break
    else
      result=$?
      [ "$result" -gt 128 ] || exit 70
    fi
  done
  printf "child-exiting %s\n" "$1"
  exit "$1"
' fake-qemu "$3" &
WIN_QEMU_PID=$!
printf 'owner %s\n' "$WIN_QEMU_PID"
IFS= read -r command
[ "$command" = shutdown ]
win_shutdown_vm
[ -z "$WIN_QEMU_PID" ] && [ -z "$WIN_RUN_DIR" ] && [ -z "$WIN_MON_SOCK" ]
printf 'shutdown-returned\n'
`
			command := exec.CommandContext(ctx, "bash", "-c", script, "shutdown-test",
				filepath.Join(windowsBuildRoot(t), "lib.sh"), temporaryDirectory, strconv.Itoa(childExit))
			command.Stdin = controlRead
			command.ExtraFiles = []*os.File{releaseRead}
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			// Every process in this fresh group belongs to this fixture. A
			// broken fixture must not leave its blocked fake child behind.
			command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
			var stderr bytes.Buffer
			command.Stderr = &stderr
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			controlRead.Close()
			releaseRead.Close()
			events := make(chan string, 16)
			scanErrors := make(chan error, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					events <- scanner.Text()
				}
				scanErrors <- scanner.Err()
				close(events)
			}()

			ownerPID := 0
			ready, requested, enteredWait, joined, returned, released := false, false, false, false, false, false
			release := func() {
				if !released {
					_, err := fmt.Fprintln(releaseWrite, "release")
					if err != nil {
						t.Errorf("release fake QEMU: %v", err)
					}
					t.Log("test released fake QEMU")
					released = true
					releaseWrite.Close()
				}
			}
			for event := range events {
				t.Log(event)
				switch {
				case strings.HasPrefix(event, "owner "):
					ownerPID, err = strconv.Atoi(strings.TrimPrefix(event, "owner "))
					if err != nil || ownerPID <= 0 {
						t.Errorf("invalid owned child event: %q", event)
					}
				case event == "child-ready":
					ready = true
				case strings.HasPrefix(event, "wait-enter "):
					enteredWait = true
					if event != fmt.Sprintf("wait-enter %d", ownerPID) {
						t.Errorf("wait is not for the directly owned child: %q", event)
					}
					if err := syscall.Kill(ownerPID, 0); err != nil {
						t.Errorf("fake QEMU exited before release: %v", err)
					}
					if _, err := os.Stat(overlay); err != nil {
						t.Errorf("overlay removed before the owned child joined: %v", err)
					}
					release()
				case strings.HasPrefix(event, "wait-return "):
					joined = true
					if event != fmt.Sprintf("wait-return %d", childExit) {
						t.Errorf("actual child wait status = %q, want %d", event, childExit)
					}
				case event == "shutdown-returned":
					returned = true
					if !enteredWait || !joined {
						_, overlayErr := os.Stat(overlay)
						t.Errorf("shutdown returned before owned QEMU join: child-live=%v overlay-exists=%v",
							syscall.Kill(ownerPID, 0) == nil, overlayErr == nil)
						release()
					}
				}
				if ready && ownerPID > 0 && !requested {
					if _, err := fmt.Fprintln(controlWrite, "shutdown"); err != nil {
						t.Errorf("request shutdown: %v", err)
					}
					requested = true
				}
			}
			if err := <-scanErrors; err != nil {
				t.Errorf("read child events: %v", err)
			}
			if err := command.Wait(); err != nil {
				t.Errorf("shutdown shell join: %v; stderr: %s", err, stderr.String())
			} else {
				t.Log("shutdown shell joined with status 0")
			}
			if !enteredWait || !joined || !returned || !released {
				t.Errorf("missing shutdown events: wait=%v joined=%v returned=%v released=%v",
					enteredWait, joined, returned, released)
			}
			if _, err := os.Stat(overlay); !os.IsNotExist(err) {
				t.Errorf("overlay remains after shutdown join: %v", err)
			}
		})
	}
}
