package clientserverrace_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The binary under test is resolved once, in TestMain, before any test
// runs (#429). go test's -timeout alarm starts inside m.Run, so a slow
// build here no longer eats the race test's own budget. CI goes further
// and builds the binary in a workflow step, handing it over through
// CRUSH_RACE_TEST_BIN, because cmd/go still kills the whole test binary
// at -timeout plus one minute however long TestMain takes.

const (
	// binEnv names a prebuilt crush binary to test instead of building
	// one. The CI workflow sets it on non-Windows runners.
	binEnv = "CRUSH_RACE_TEST_BIN"
	// orphanAgeEnv overrides how old an abandoned run directory must be
	// before TestMain reaps it, so the reaping can be exercised by hand
	// without waiting half an hour.
	orphanAgeEnv = "CRUSH_RACE_ORPHAN_AGE"
	// defaultOrphanAge is longer than any live run of this package, so a
	// concurrent run's server is never touched.
	defaultOrphanAge = 30 * time.Minute
	// buildTimeout bounds the local build. cmd/go kills the test binary
	// at -timeout plus one minute (eleven minutes by default), so a
	// genuinely stuck build fails here, with the build output, first.
	buildTimeout = 10 * time.Minute
	// runDirPrefix is the os.MkdirTemp prefix of each race test run.
	runDirPrefix = "crush-race-"
)

var (
	// crushBin is the binary under test, set by TestMain.
	crushBin string
	// crushBinErr is why there is no binary; the race test reports it.
	crushBinErr error
	// errNoGo means there is neither a prebuilt binary nor a go tool to
	// build one; the race test skips rather than fails.
	errNoGo = errors.New("'go' not available on PATH")
)

func TestMain(m *testing.M) {
	flag.Parse()
	// The race test skips in -short mode and on Windows, so neither pays
	// for a build or a reap.
	if testing.Short() || runtime.GOOS == "windows" {
		m.Run()
		return
	}
	reapOrphans("/tmp/"+runDirPrefix+"*", orphanAge(), func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "clientserverrace: "+format+"\n", args...)
	})
	bin, cleanup, err := prepareBinary()
	defer cleanup()
	crushBin, crushBinErr = bin, err
	m.Run()
}

// prepareBinary returns the crush binary to test and a cleanup that
// removes anything it built: the prebuilt binary named by
// CRUSH_RACE_TEST_BIN when set, otherwise a fresh CGO_ENABLED=0 build.
func prepareBinary() (string, func(), error) {
	noop := func() {}
	if p := os.Getenv(binEnv); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", noop, fmt.Errorf("%s=%s: %w", binEnv, p, err)
		}
		if _, err := os.Stat(abs); err != nil {
			return "", noop, fmt.Errorf("%s=%s: %w", binEnv, p, err)
		}
		fmt.Fprintf(os.Stderr, "clientserverrace: using prebuilt crush binary %s\n", abs)
		return abs, noop, nil
	}
	if _, err := exec.LookPath("go"); err != nil {
		return "", noop, errNoGo
	}
	root, err := repoRoot()
	if err != nil {
		return "", noop, err
	}
	binDir, err := os.MkdirTemp("", "crush-race-bin-")
	if err != nil {
		return "", noop, fmt.Errorf("mkdtemp bin: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(binDir) }
	binPath := filepath.Join(binDir, "crush")

	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binPath, ".")
	cmd.Dir = root
	// Match the project's standard build flags. CGO_ENABLED=0 keeps the
	// binary statically linked and avoids surprising the test on hosts
	// without a C toolchain.
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", cleanup, fmt.Errorf("go build crush: %w\n%s", err, out)
	}
	return binPath, cleanup, nil
}

// repoRoot walks up from the working directory to the directory holding
// go.mod. Walking up by a fixed count is fragile across reorganisations.
func repoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	for dir := cwd; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find go.mod walking up from %s", cwd)
		}
		dir = parent
	}
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
