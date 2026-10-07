//go:build linux

package a2a

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// processStartToken returns the process's start time in clock ticks
// (/proc/<pid>/stat field 22), the token that changes when the same
// pid is reused by a new process.
func processStartToken(pid int) (string, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	// stat's second field (comm) is parenthesized and may itself contain
	// spaces and parentheses, so fields are counted after the last ')':
	// state is field 3, which makes starttime (field 22) index 19 there.
	comm := strings.LastIndexByte(string(data), ')')
	if comm < 0 {
		return "", fmt.Errorf("stat for pid %d has no comm field", pid)
	}
	fields := strings.Fields(string(data)[comm+1:])
	if len(fields) < 20 {
		return "", fmt.Errorf("stat for pid %d is truncated", pid)
	}
	return fields[19], nil
}

// processAlive compares the pid's current start token against the one
// the host ID carries. A missing /proc entry is a definite no; any
// other read failure is a probe error the caller treats as alive.
func processAlive(pid int, token string) (bool, error) {
	current, err := processStartToken(pid)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return true, err
	}
	return current == token, nil
}
