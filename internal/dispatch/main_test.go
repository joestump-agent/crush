package dispatch

import (
	"fmt"
	"os"
	"testing"

	"github.com/charmbracelet/crush/internal/gittest"
)

// TestMain hermetizes the git environment for every test in this
// package. The fixtures here run git with the inherited process
// environment, so a developer's commit signing, external diff, or a
// GIT_DIR exported by a hook breaks them; see internal/gittest.
func TestMain(m *testing.M) {
	restore, err := gittest.Setup()
	if err != nil {
		panic(fmt.Sprintf("hermetic git env: %v", err))
	}
	code := m.Run()
	restore()
	os.Exit(code)
}
