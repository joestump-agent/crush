package a2a

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// HostID identifies this crush process for the durable A2A tables
// (#355): "hostname|pid|start-token", where the start token changes
// every time the process restarts on the same pid. Rows written by a
// process that is no longer running — the same pid that restarted, or
// one that never came back — are what startup reconciliation (#355)
// fails and re-delivers, so the ID must distinguish a live process
// from a dead one even when the pid is reused.
//
// #365's owner marker (InstanceID/PID/CreatedAt) covers workspaces, not
// served tasks; the a2a rows need a self-contained identifier that
// travels with every row they write.
func HostID() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown"
	}
	token, err := processStartToken(os.Getpid())
	if err != nil || token == "" {
		// A process whose start token cannot be read still gets a row
		// identity; reconcile treats an unprovable host as dead only on
		// a malformed ID, never on a probe error.
		token = "unknown"
	}
	return fmt.Sprintf("%s|%d|%s", hostname, os.Getpid(), token)
}

// parseHostID splits a host ID back into its parts. Anything that does
// not match the HostID shape — including an empty column — parses as
// not-ok: reconcile fails those rows, because no live crush process
// ever writes one.
func parseHostID(hostID string) (hostname string, pid int, token string, ok bool) {
	parts := strings.Split(hostID, "|")
	if len(parts) != 3 {
		return "", 0, "", false
	}
	pid, err := strconv.Atoi(parts[1])
	if err != nil || pid <= 0 || parts[2] == "" || parts[0] == "" {
		return "", 0, "", false
	}
	return parts[0], pid, parts[2], true
}

// ProcessAlive reports whether the process a host ID names is still
// running with the start token the ID carries.
//
// Failure modes are deliberately asymmetric: a malformed ID is dead
// (nothing live writes one), but a probe that could not run — a
// missing ps, an unreadable /proc — reports alive, because the cost of
// leaving a live process's task untouched is far lower than failing
// one that is still working. Pid reuse is handled by the start-token
// comparison, not the pid alone.
func ProcessAlive(hostID string) bool {
	_, pid, token, ok := parseHostID(hostID)
	if !ok {
		return false
	}
	alive, err := processAlive(pid, token)
	if err != nil {
		return true
	}
	return alive
}
