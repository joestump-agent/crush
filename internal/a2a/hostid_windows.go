//go:build windows

package a2a

import (
	"errors"
	"strconv"

	"golang.org/x/sys/windows"
)

// processStartToken returns the process's creation time as a FILETIME
// count (100ns units since 1601) — the token that changes when the
// same pid is reused by a new process.
func processStartToken(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return "", err
	}
	return strconv.FormatUint(uint64(creation.HighDateTime)<<32|uint64(creation.LowDateTime), 10), nil
}

// processAlive compares the pid's current creation time against the
// one the host ID carries. OpenProcess fails for a pid that does not
// exist; any other probe failure is reported so the caller treats the
// process as alive.
func processAlive(pid int, token string) (bool, error) {
	current, err := processStartToken(pid)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return false, nil
		}
		return true, err
	}
	return current == token, nil
}
