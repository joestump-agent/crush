package clientserverrace_test

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestReapOrphans pins TestMain's cleanup of runs a killed test binary
// abandoned (#429): a run directory older than the threshold that still
// holds a crush.sock gets a shutdown POST and is removed, while a fresh
// one (a live run) and one with no socket are left alone.
func TestReapOrphans(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the reaped servers listen on unix sockets")
	}
	// /tmp, not t.TempDir(): darwin caps a unix socket path at 104
	// bytes, and t.TempDir() lives under a long /var/folders path.
	root, err := os.MkdirTemp("/tmp", "crush-reap-test-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	old := time.Now().Add(-2 * time.Hour)
	orphan, orphanShutdowns := fakeServerDir(t, filepath.Join(root, "crush-race-orphan"))
	live, liveShutdowns := fakeServerDir(t, filepath.Join(root, "crush-race-live"))
	noSock := filepath.Join(root, "crush-race-nosock")
	if err := os.Mkdir(noSock, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, p := range []string{orphan, filepath.Join(orphan, "crush.sock"), noSock} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}

	reapOrphans(filepath.Join(root, "crush-race-*"), time.Hour, t.Logf)

	if got := orphanShutdowns.Load(); got != 1 {
		t.Errorf("orphaned server got %d shutdown requests, want 1", got)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphaned run dir %s still exists (stat err %v)", orphan, err)
	}
	if got := liveShutdowns.Load(); got != 0 {
		t.Errorf("live server got %d shutdown requests, want 0", got)
	}
	if _, err := os.Stat(filepath.Join(live, "crush.sock")); err != nil {
		t.Errorf("live run's socket was touched: %v", err)
	}
	if _, err := os.Stat(noSock); err != nil {
		t.Errorf("a run dir without a socket was removed: %v", err)
	}
}

// TestOrphanAge pins the reap threshold's override: a positive duration
// in CRUSH_RACE_ORPHAN_AGE wins, anything else falls back to the default.
func TestOrphanAge(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{
		{"", defaultOrphanAge},
		{"1s", time.Second},
		{"-5m", defaultOrphanAge},
		{"soon", defaultOrphanAge},
	} {
		t.Setenv(orphanAgeEnv, tc.env)
		if got := orphanAge(); got != tc.want {
			t.Errorf("%s=%q: orphanAge() = %v, want %v", orphanAgeEnv, tc.env, got, tc.want)
		}
	}
}

// fakeServerDir creates dir with a crush.sock served by a stand-in for
// the crush server's control endpoint, and returns the directory and a
// count of the shutdown requests it received.
func fakeServerDir(t *testing.T, dir string) (string, *atomic.Int32) {
	t.Helper()
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", filepath.Join(dir, "crush.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var shutdowns atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/control", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		if strings.Contains(string(body), `"shutdown"`) {
			shutdowns.Add(1)
			// Like the real server, go away: closing a unix listener
			// unlinks its socket, which shutdownSocket waits for.
			go func() { _ = ln.Close() }()
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return dir, &shutdowns
}
