//go:build darwin

package a2a

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// processStartToken returns the process's start time as Unix seconds,
// read from ps's lstart — the token that changes when the same pid is
// reused by a new process.
func processStartToken(pid int) (string, error) {
	// No deadline on purpose: a probe killed by its context fails with
	// an *exec.ExitError, which processAlive reads as a dead process.
	out, err := exec.CommandContext(context.Background(), "ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(out))
	t, err := time.Parse("Mon Jan _2 15:04:05 2006", line)
	if err != nil {
		return "", fmt.Errorf("parse ps lstart %q for pid %d: %w", line, pid, err)
	}
	return strconv.FormatInt(t.Unix(), 10), nil
}

// processAlive compares the pid's current start token against the one
// the host ID carries. ps exits nonzero for a pid that does not exist
// — a definite no; any other failure is a probe error the caller
// treats as alive.
func processAlive(pid int, token string) (bool, error) {
	current, err := processStartToken(pid)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return true, err
	}
	return current == token, nil
}
