package projects

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/config"
)

const projectsFileName = "projects.json"

// Project represents a tracked project directory.
type Project struct {
	Path         string    `json:"path"`
	DataDir      string    `json:"data_dir"`
	LastAccessed time.Time `json:"last_accessed"`
}

// ProjectList holds the list of tracked projects.
type ProjectList struct {
	Projects []Project `json:"projects"`
}

var mu sync.Mutex

// nowFunc returns the current time. Overridden in tests so that ordering
// assertions do not depend on the host clock's resolution.
var nowFunc = func() time.Time { return time.Now().UTC() }

// projectsFilePath returns the path to the projects.json file.
func projectsFilePath() string {
	return filepath.Join(filepath.Dir(config.GlobalConfigData()), projectsFileName)
}

// Load reads the projects list from disk.
func Load() (*ProjectList, error) {
	mu.Lock()
	defer mu.Unlock()

	path := projectsFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ProjectList{Projects: []Project{}}, nil
		}
		return nil, err
	}

	var list ProjectList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}

	return &list, nil
}

// Save writes the projects list to disk.
func Save(list *ProjectList) error {
	mu.Lock()
	defer mu.Unlock()

	path := projectsFilePath()

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0o600)
}

// Register adds or updates a project in the list.
func Register(workingDir, dataDir string) error {
	list, err := Load()
	if err != nil {
		return err
	}

	// Drop any existing entry for this path, then put the freshly registered one
	// at the front. Position, not the timestamp alone, is what makes this project
	// the most recent: coarse system clocks (Windows ticks roughly every 15ms)
	// hand out identical timestamps to back-to-back registrations.
	list.Projects = slices.DeleteFunc(list.Projects, func(p Project) bool {
		return p.Path == workingDir
	})
	list.Projects = append([]Project{{
		Path:         workingDir,
		DataDir:      dataDir,
		LastAccessed: nowFunc(),
	}}, list.Projects...)

	// Sort by last accessed (most recent first). Stable, so projects sharing a
	// timestamp keep the order established above instead of being reordered
	// arbitrarily.
	slices.SortStableFunc(list.Projects, func(a, b Project) int {
		return b.LastAccessed.Compare(a.LastAccessed)
	})

	return Save(list)
}

// List returns all tracked projects sorted by last accessed.
func List() ([]Project, error) {
	list, err := Load()
	if err != nil {
		return nil, err
	}
	return list.Projects, nil
}
