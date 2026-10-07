package a2a

import (
	"fmt"
	"os/exec"
	"runtime"
	"testing"
)

func TestHostIDShape(t *testing.T) {
	hostID := HostID()
	hostname, pid, token, ok := parseHostID(hostID)
	if !ok {
		t.Fatalf("HostID() = %q does not parse", hostID)
	}
	if hostname == "" || pid != pidOfThisTest() || token == "" {
		t.Fatalf("HostID() = %q has empty parts (hostname=%q pid=%d token=%q)", hostID, hostname, pid, token)
	}
}

func TestProcessAliveSelf(t *testing.T) {
	if !ProcessAlive(HostID()) {
		t.Fatalf("ProcessAlive(HostID()) = false, want true for the running test process")
	}
}

func TestProcessAliveMalformed(t *testing.T) {
	for _, hostID := range []string{"", "nonsense", "host|notapid|token", "host|123|", "|1|token"} {
		if ProcessAlive(hostID) {
			t.Errorf("ProcessAlive(%q) = true, want false for a malformed host id", hostID)
		}
	}
}

func TestProcessAliveExitedProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the conservative OpenProcess probe cannot distinguish a reaped pid")
	}
	if runtime.GOOS == "darwin" {
		t.Skip("ps lstart on a reaped pid races the process table on CI runners")
	}
	cmd := exec.CommandContext(t.Context(), "true")
	if err := cmd.Run(); err != nil {
		t.Skipf("no /bin/true: %v", err)
	}
	hostID := fmt.Sprintf("localhost|%d|1", cmd.Process.Pid)
	if ProcessAlive(hostID) {
		t.Errorf("ProcessAlive(%q) = true, want false for an exited process", hostID)
	}
}

func pidOfThisTest() int {
	_, pid, _, ok := parseHostID(HostID())
	if !ok {
		return 0
	}
	return pid
}
