// Package gittest hermetizes the git environment of a test binary.
// Git-backed fixtures inherit the developer's git config and
// environment — commit signing, external diffs, a GIT_DIR or
// GIT_INDEX_FILE exported by a hook, a TMPDIR inside a repository —
// and fail only on the machines that carry any of them.
//
// It is imported only from _test.go files, like
// internal/agent/agenttest, and is never referenced by production
// code, so it is compiled only under tests and never ships in the
// production binary.
package gittest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// testName and testEmail are the fixed identity tests commit
	// under, so commits never depend on global git config.
	testName  = "Crush Test"
	testEmail = "crush-test@example.com"
)

// Setup hermetizes the git environment for the lifetime of the test
// binary and returns a func that restores the process environment to
// the exact state Setup found it in. Call it once from TestMain,
// before m.Run:
//
//	restore, err := gittest.Setup()
//	if err != nil {
//		panic(err)
//	}
//	code := m.Run()
//	restore()
//
// The hermetic environment works through the process environment, so
// production git invocations made by the code under test are covered
// too. Setup unsets every inherited GIT_* variable except
// GIT_EXEC_PATH, and pins:
//
//	GIT_CONFIG_GLOBAL       os.DevNull (requires git 2.32 or newer)
//	GIT_CONFIG_NOSYSTEM     1
//	LC_ALL                  C
//	GIT_AUTHOR_NAME         testName
//	GIT_AUTHOR_EMAIL        testEmail
//	GIT_COMMITTER_NAME      testName
//	GIT_COMMITTER_EMAIL     testEmail
//	GIT_CEILING_DIRECTORIES the resolved os.TempDir()
//
// GIT_CONFIG_GLOBAL and GIT_CONFIG_NOSYSTEM keep the developer's
// global and system config out, including commit.gpgsign,
// diff.external and color.ui. The ceiling keeps git from walking
// past the temp tree to a repository the test never intended,
// including a TMPDIR that sits inside one.
func Setup() (func(), error) {
	original := os.Environ()

	tempDir, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return nil, fmt.Errorf("resolve temp dir: %w", err)
	}

	pinned := map[string]string{
		"LC_ALL":                  "C",
		"GIT_CONFIG_GLOBAL":       os.DevNull,
		"GIT_CONFIG_NOSYSTEM":     "1",
		"GIT_AUTHOR_NAME":         testName,
		"GIT_AUTHOR_EMAIL":        testEmail,
		"GIT_COMMITTER_NAME":      testName,
		"GIT_COMMITTER_EMAIL":     testEmail,
		"GIT_CEILING_DIRECTORIES": tempDir,
	}

	// GIT_EXEC_PATH is the one inherited variable we keep: without
	// it git has to rediscover its own executables by trial and error.
	for _, kv := range original {
		name, value, _ := strings.Cut(kv, "=")
		if name == "GIT_EXEC_PATH" {
			pinned[name] = value
		}
	}

	for name, value := range pinned {
		if err := os.Setenv(name, value); err != nil {
			return nil, fmt.Errorf("set %s: %w", name, err)
		}
	}
	for _, kv := range original {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "GIT_") {
			if _, keep := pinned[name]; !keep {
				if err := os.Unsetenv(name); err != nil {
					return nil, fmt.Errorf("unset %s: %w", name, err)
				}
			}
		}
	}

	return func() {
		os.Clearenv()
		for _, kv := range original {
			name, value, _ := strings.Cut(kv, "=")
			_ = os.Setenv(name, value)
		}
	}, nil
}
