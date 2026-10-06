// Package clientserverrace_test is a regression test for the
// CRUSH_CLIENT_SERVER=1 socket-init race documented in
// docs/notes/2026-05-11-client-server-socket-init-race.md (item F5).
//
// It lives in its own directory so it can build even if other test
// files in internal/cmd are temporarily broken: it needs the cmd
// package's code, which its test binary runs as crush, not its tests.
package clientserverrace_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// readinessErrSubstr is the user-visible error string emitted by
// ensureServer when it gives up waiting for the server socket /
// readiness probe (internal/cmd/root.go). Seeing this in any client's
// output means the race fired.
const readinessErrSubstr = "failed to initialize crush server"

// numClients is intentionally larger than the typical CPU count to
// ensure the spawn lock + readiness probe are exercised under
// contention.
const numClients = 8

// clientTimeout bounds each child invocation. It only needs to be long
// enough for the spawn-and-readiness phase to complete on a cold cache;
// after that, the client may legitimately keep running (e.g.
// subscribing to server events) and we'll cancel it. The race we care
// about is observable strictly within ensureServer.
const clientTimeout = 15 * time.Second

func TestClientServerSpawnRace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping client/server spawn race test in -short mode")
	}
	// The race and its fix are unix-socket specific. Windows uses
	// named pipes via a different code path; not covered here.
	if runtime.GOOS == "windows" {
		t.Skip("skipping unix-socket specific race test on windows")
	}
	// The test binary runs as crush in the children (#429).
	if crushBinErr != nil {
		t.Fatalf("no crush binary to test: %v", crushBinErr)
	}
	bin := crushBin

	// Use /tmp directly so the unix socket path stays under the
	// 104-char sockaddr_un limit on darwin. t.TempDir() can return a
	// path inside /var/folders/... that is too long. TestMain reaps
	// these directories when a killed run leaves one behind.
	runDir, err := os.MkdirTemp("/tmp", runDirPrefix)
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })

	socketPath := filepath.Join(runDir, "crush.sock")
	host := "unix://" + socketPath

	// Fresh, isolated XDG/HOME so we don't touch the user's real
	// state or any other test's cache. These all live under runDir
	// so cleanup is one RemoveAll.
	cacheHome := filepath.Join(runDir, "cache")
	dataHome := filepath.Join(runDir, "data")
	configHome := filepath.Join(runDir, "config")
	homeDir := filepath.Join(runDir, "home")
	for _, d := range []string{cacheHome, dataHome, configHome, homeDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	env := append(
		os.Environ(),
		runAsCrushEnv+"=1",
		"CRUSH_CLIENT_SERVER=1",
		"XDG_CACHE_HOME="+cacheHome,
		"XDG_DATA_HOME="+dataHome,
		"XDG_CONFIG_HOME="+configHome,
		"HOME="+homeDir,
		// Belt-and-suspenders: if anything tries to talk to a real
		// provider, fail loudly rather than make a network call.
		"CRUSH_DISABLE_PROVIDER_AUTO_UPDATE=1",
	)

	// Make sure no server is up before we start.
	if _, err := os.Stat(socketPath); err == nil {
		t.Fatalf("socket %s exists before test started", socketPath)
	}

	// Always try to shut down any server we spawned, regardless of
	// outcome.
	t.Cleanup(func() { shutdownServer(t, socketPath) })

	type result struct {
		idx    int
		stdout string
		stderr string
	}
	results := make(chan result, numClients)

	// Probe /v1/health concurrently while the clients are still
	// running. The server self-shuts-down when the last workspace is
	// released (internal/backend/backend.go:DeleteWorkspace), so once
	// all clients exit cleanly the socket may legitimately be gone —
	// asserting the socket post-hoc would race with that documented
	// self-shutdown. Instead we require that during the parallel run
	// at least one /v1/health probe got a 2xx, which proves the
	// spawn-and-readiness path actually produced a live server.
	var sawHealthy atomic.Bool
	probeDone := make(chan struct{})
	stopProbe := make(chan struct{})

	var wg sync.WaitGroup
	start := make(chan struct{})

	go func() {
		defer close(probeDone)
		<-start
		deadline := time.Now().Add(clientTimeout)
		for time.Now().Before(deadline) {
			select {
			case <-stopProbe:
				return
			default:
			}
			if err := pingHealth(socketPath); err == nil {
				sawHealthy.Store(true)
				return
			}
			select {
			case <-stopProbe:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()

	for i := range numClients {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			// Each client gets its own working directory so the
			// per-client workspace registration paths don't collide
			// in confusing ways.
			cwd := filepath.Join(runDir, fmt.Sprintf("ws-%d", i))
			if err := os.MkdirAll(cwd, 0o700); err != nil {
				results <- result{idx: i, stderr: fmt.Sprintf("mkdir cwd: %v", err)}
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), clientTimeout)
			defer cancel()

			// `crush run` exercises connectToServer (which is where
			// the readiness race lives). On a fresh sandbox the
			// command may legitimately keep running past the race
			// (e.g. waiting on event subscriptions); the context
			// timeout above bounds that. We assert race outcomes
			// purely from output, not exit code.
			c := exec.CommandContext(
				ctx, bin,
				"--host", host,
				"--cwd", cwd,
				"run", "hi",
			)
			c.Env = env
			var outBuf, errBuf strings.Builder
			c.Stdout = &outBuf
			c.Stderr = &errBuf

			<-start
			_ = c.Run()
			results <- result{
				idx:    i,
				stdout: outBuf.String(),
				stderr: errBuf.String(),
			}
		}(i)
	}

	close(start) // release all clients as simultaneously as possible
	wg.Wait()
	close(results)
	close(stopProbe)
	<-probeDone

	var raceFailures []string
	for r := range results {
		if strings.Contains(r.stderr, readinessErrSubstr) ||
			strings.Contains(r.stdout, readinessErrSubstr) {
			raceFailures = append(raceFailures, fmt.Sprintf(
				"client %d: readiness error in output\nstderr:\n%s\nstdout:\n%s",
				r.idx, r.stderr, r.stdout,
			))
		}
	}

	if len(raceFailures) > 0 {
		t.Fatalf(
			"client/server spawn race regressed: %d/%d clients failed\n\n%s",
			len(raceFailures), numClients,
			strings.Join(raceFailures, "\n---\n"),
		)
	}

	// Positive sanity check: at some point during the parallel run a
	// /v1/health probe must have succeeded. We deliberately do *not*
	// stat the socket post-hoc: when every client returns cleanly
	// (e.g. exits early because no providers are configured), the
	// last DeleteWorkspace triggers the server's self-shutdown and
	// the socket disappears. That is correct behaviour, not a race
	// regression.
	if !sawHealthy.Load() {
		t.Fatalf("no /v1/health probe succeeded on %s while %d clients were running",
			socketPath, numClients)
	}
}

// pingHealth issues a single GET /v1/health over the unix socket and
// requires a 2xx response.
func pingHealth(socketPath string) error {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, Timeout: 2 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://crush.local/v1/health", nil)
	if err != nil {
		return err
	}
	rsp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer rsp.Body.Close()
	if rsp.StatusCode < 200 || rsp.StatusCode >= 300 {
		return fmt.Errorf("health check returned %s", rsp.Status)
	}
	return nil
}

// shutdownServer best-effort terminates any crush server bound to
// socketPath, logging through t.
func shutdownServer(t *testing.T, socketPath string) {
	t.Helper()
	shutdownSocket(socketPath, t.Logf)
}
