package clientserverrace_test

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/cmd"
)

// The race test needs a real crush binary, and the test binary already is
// one: it links the whole CLI through internal/cmd. A child started with
// runAsCrushEnv set runs crush instead of the tests, and the client starts
// its detached server by re-executing os.Executable() with the same
// environment, so the server is this binary too. That replaces a separate
// `go build` of the CLI, which took minutes on a cold macOS runner inside
// go test's timeout (#429), and needs no CI step and no go toolchain.

const (
	// runAsCrushEnv makes TestMain run the crush CLI instead of the tests.
	runAsCrushEnv = "CRUSH_RACE_RUN_AS_CRUSH"
	// orphanAgeEnv overrides how old an abandoned run directory must be
	// before TestMain reaps it, so the reaping can be exercised by hand
	// without waiting half an hour.
	orphanAgeEnv = "CRUSH_RACE_ORPHAN_AGE"
	// defaultOrphanAge is longer than any live run of this package, so a
	// concurrent run's server is never touched.
	defaultOrphanAge = 30 * time.Minute
	// runDirPrefix is the os.MkdirTemp prefix of each race test run.
	runDirPrefix = "crush-race-"
)

var (
	// crushBin is this test binary, which runs as crush under
	// runAsCrushEnv; set by TestMain.
	crushBin string
	// crushBinErr is why it couldn't be located; the race test reports it.
	crushBinErr error
)

func TestMain(m *testing.M) {
	// Before flag.Parse: a child's arguments are crush's, not go test's.
	if os.Getenv(runAsCrushEnv) == "1" {
		cmd.Execute()
		os.Exit(0)
	}
	flag.Parse()
	// The race test skips in -short mode and on Windows, so neither pays
	// for a reap.
	if testing.Short() || runtime.GOOS == "windows" {
		m.Run()
		return
	}
	reapOrphans("/tmp/"+runDirPrefix+"*", orphanAge(), func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "clientserverrace: "+format+"\n", args...)
	})
	crushBin, crushBinErr = os.Executable()
	m.Run()
}

// orphanAge is the reap threshold: CRUSH_RACE_ORPHAN_AGE when it parses
// as a positive duration, otherwise defaultOrphanAge.
func orphanAge() time.Duration {
	if v := os.Getenv(orphanAgeEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		fmt.Fprintf(os.Stderr, "clientserverrace: ignoring %s=%q, not a positive duration\n", orphanAgeEnv, v)
	}
	return defaultOrphanAge
}

// reapOrphans shuts down the servers that killed runs of this package
// left behind, then removes their run directories. A killed test binary
// runs no cleanups, and the server it spawned is detached (Setsid), so
// nothing else ever stops it. Only directories that still hold a
// crush.sock, and whose newest entry is older than age, are touched: a
// live run, this process's or a concurrent one, is always younger.
func reapOrphans(pattern string, age time.Duration, logf func(string, ...any)) {
	dirs, err := filepath.Glob(pattern)
	if err != nil {
		return
	}
	for _, dir := range dirs {
		sock := filepath.Join(dir, "crush.sock")
		dirInfo, err := os.Stat(dir)
		if err != nil || !dirInfo.IsDir() {
			continue
		}
		sockInfo, err := os.Stat(sock)
		if err != nil {
			continue
		}
		newest := dirInfo.ModTime()
		if sockInfo.ModTime().After(newest) {
			newest = sockInfo.ModTime()
		}
		if time.Since(newest) < age {
			continue
		}
		shutdownSocket(sock, logf)
		if err := os.RemoveAll(dir); err != nil {
			logf("reap %s: %v", dir, err)
			continue
		}
		logf("reaped orphaned run %s", dir)
	}
}

// shutdownSocket best-effort terminates any crush server bound to
// socketPath by POSTing to /v1/control, then waits briefly for the socket
// to disappear. It doesn't import the project's own client package, to
// keep this test free of internal API churn.
func shutdownSocket(socketPath string, logf func(string, ...any)) {
	if _, err := os.Stat(socketPath); err != nil {
		return
	}

	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	hc := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	defer tr.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	body := strings.NewReader(`{"command":"shutdown"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://crush.local/v1/control", body)
	if err != nil {
		logf("shutdown: build request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		// Server may already be gone — not an error.
		logf("shutdown: %v (probably already exited)", err)
		return
	}
	_ = resp.Body.Close()

	// Wait briefly for the socket to disappear so the next run using the
	// same path doesn't race.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
