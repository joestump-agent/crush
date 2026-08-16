package projects

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Paths in these tests are opaque identifiers: Register only stores and
// compares them as strings, so the Unix-style spelling is portable and reads
// the same on every platform.

// fakeClock pins the package clock and returns a function that advances it, so
// ordering assertions do not depend on the host clock's resolution. Left at the
// pinned instant, every registration shares a timestamp — the case a coarse
// clock (Windows ticks roughly every 15ms) produces on its own.
func fakeClock(t *testing.T) func(time.Duration) {
	t.Helper()

	current := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	restore := nowFunc
	nowFunc = func() time.Time { return current }
	t.Cleanup(func() { nowFunc = restore })

	return func(d time.Duration) { current = current.Add(d) }
}

func TestRegisterAndList(t *testing.T) {
	// Create a temporary directory for the test
	tmpDir := t.TempDir()

	// Override the projects file path for testing
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	// Both registrations land on the same timestamp, so ordering has to come
	// from recency of registration rather than from the clock.
	fakeClock(t)

	// Test registering a project
	err := Register("/home/user/project1", "/home/user/project1/.crush")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	// List projects
	projects, err := List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(projects) != 1 {
		t.Fatalf("Expected 1 project, got %d", len(projects))
	}

	if projects[0].Path != "/home/user/project1" {
		t.Errorf("Expected path /home/user/project1, got %s", projects[0].Path)
	}

	if projects[0].DataDir != "/home/user/project1/.crush" {
		t.Errorf("Expected data_dir /home/user/project1/.crush, got %s", projects[0].DataDir)
	}

	// Register another project
	err = Register("/home/user/project2", "/home/user/project2/.crush")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	projects, err = List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(projects) != 2 {
		t.Fatalf("Expected 2 projects, got %d", len(projects))
	}

	// Most recent should be first
	if projects[0].Path != "/home/user/project2" {
		t.Errorf("Expected most recent project first, got %s", projects[0].Path)
	}
}

func TestRegisterUpdatesExisting(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	advance := fakeClock(t)

	// Register a project
	err := Register("/home/user/project1", "/home/user/project1/.crush")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	projects, _ := List()
	firstAccess := projects[0].LastAccessed

	// Move the clock forward and re-register
	advance(time.Second)

	err = Register("/home/user/project1", "/home/user/project1/.crush-new")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	projects, _ = List()

	if len(projects) != 1 {
		t.Fatalf("Expected 1 project after update, got %d", len(projects))
	}

	if projects[0].DataDir != "/home/user/project1/.crush-new" {
		t.Errorf("Expected updated data_dir, got %s", projects[0].DataDir)
	}

	if !projects[0].LastAccessed.After(firstAccess) {
		t.Error("Expected LastAccessed to be updated")
	}
}

func TestRegisterOrdersTiedTimestampsByRecency(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	// The clock never advances, so all three projects record the same
	// LastAccessed and only registration order can break the tie.
	fakeClock(t)

	for _, path := range []string{"/home/user/a", "/home/user/b", "/home/user/c"} {
		if err := Register(path, path+"/.crush"); err != nil {
			t.Fatalf("Register(%s) failed: %v", path, err)
		}
	}

	assertOrder(t, "/home/user/c", "/home/user/b", "/home/user/a")

	// Re-registering the oldest project moves it back to the front, still
	// without any help from the clock.
	if err := Register("/home/user/a", "/home/user/a/.crush"); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	assertOrder(t, "/home/user/a", "/home/user/c", "/home/user/b")
}

func TestRegisterOrdersDistinctTimestampsByTime(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	advance := fakeClock(t)

	for _, path := range []string{"/home/user/a", "/home/user/b", "/home/user/c"} {
		if err := Register(path, path+"/.crush"); err != nil {
			t.Fatalf("Register(%s) failed: %v", path, err)
		}
		advance(time.Minute)
	}

	assertOrder(t, "/home/user/c", "/home/user/b", "/home/user/a")
}

// assertOrder checks that List returns exactly the given paths, in order.
func assertOrder(t *testing.T, want ...string) {
	t.Helper()

	projects, err := List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	got := make([]string, len(projects))
	for i, p := range projects {
		got[i] = p.Path
	}

	if !slices.Equal(got, want) {
		t.Errorf("Expected order %v, got %v", want, got)
	}
}

func TestLoadEmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	// List before any projects exist
	projects, err := List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(projects) != 0 {
		t.Errorf("Expected 0 projects, got %d", len(projects))
	}
}

func TestProjectsFilePath(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	expected := filepath.Join(tmpDir, "crush", "projects.json")
	actual := projectsFilePath()

	if actual != expected {
		t.Errorf("Expected %s, got %s", expected, actual)
	}
}

func TestRegisterWithParentDataDir(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	// Register a project where .crush is in a parent directory.
	// e.g., working in /home/user/monorepo/packages/app but .crush is at /home/user/monorepo/.crush
	err := Register("/home/user/monorepo/packages/app", "/home/user/monorepo/.crush")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	projects, err := List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(projects) != 1 {
		t.Fatalf("Expected 1 project, got %d", len(projects))
	}

	if projects[0].Path != "/home/user/monorepo/packages/app" {
		t.Errorf("Expected path /home/user/monorepo/packages/app, got %s", projects[0].Path)
	}

	if projects[0].DataDir != "/home/user/monorepo/.crush" {
		t.Errorf("Expected data_dir /home/user/monorepo/.crush, got %s", projects[0].DataDir)
	}
}

func TestRegisterWithExternalDataDir(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	// Register a project where .crush is in a completely different location.
	// e.g., project at /home/user/project but data stored at /var/data/crush/myproject
	err := Register("/home/user/project", "/var/data/crush/myproject")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	projects, err := List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(projects) != 1 {
		t.Fatalf("Expected 1 project, got %d", len(projects))
	}

	if projects[0].Path != "/home/user/project" {
		t.Errorf("Expected path /home/user/project, got %s", projects[0].Path)
	}

	if projects[0].DataDir != "/var/data/crush/myproject" {
		t.Errorf("Expected data_dir /var/data/crush/myproject, got %s", projects[0].DataDir)
	}
}
