package shell

import (
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Wait returns once the process has been waited for, not as soon as Close
// cancels it: the result was nil then and the process still running. The
// mixer restarts FFmpeg on what Wait reports (#2484).
func TestWaitAfterCloseReturnsTheResult(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep command")
	}

	cmd := NewCommand("sleep 10")
	require.NoError(t, cmd.Start())
	require.NoError(t, cmd.Close())

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		require.Error(t, err) // killed
		require.NotNil(t, cmd.ProcessState)
		require.False(t, cmd.ProcessState.Success())
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return")
	}
}

// A program that ends by itself reports its own exit status to every caller
// of Wait (on Linux after the kill of what is left of its group).
func TestWaitFromSeveralPlaces(t *testing.T) {
	if _, err := exec.LookPath("false"); err != nil {
		t.Skip("no false command")
	}

	cmd := NewCommand("false")
	require.NoError(t, cmd.Start())

	errs := make(chan error, 2)
	go func() { errs <- cmd.Wait() }()
	go func() { errs <- cmd.Wait() }()

	for range 2 {
		select {
		case err := <-errs:
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr)
		case <-time.After(5 * time.Second):
			t.Fatal("Wait did not return")
		}
	}
}
