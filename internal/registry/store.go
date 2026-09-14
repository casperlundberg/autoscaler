package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/platform"
	"github.com/casperlundberg/autoscaler/internal/secret"
)

// Persisted is one target as it survives a restart.
type Persisted struct {
	Target    platform.Target
	Settings  config.Settings
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store is where registered targets live between restarts.
type Store interface {
	Load() ([]Persisted, error)
	Save(targets []Persisted) error
}

// MemoryStore keeps targets only for the life of the process. It is what a
// simulation harness wants, and what tests want; a service scaling real
// infrastructure wants FileStore.
type MemoryStore struct {
	mu      sync.Mutex
	targets []Persisted
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

// Load returns what was last saved.
func (m *MemoryStore) Load() ([]Persisted, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Persisted(nil), m.targets...), nil
}

// Save replaces the stored set.
func (m *MemoryStore) Save(targets []Persisted) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.targets = append([]Persisted(nil), targets...)
	return nil
}

// FileStore persists targets to a JSON file.
//
// The file holds access keys in the clear, and there is no way around that: a
// registry that stored redacted credentials would lose every key on restart
// and come back unable to scale anything. So the file is written 0600 and is
// expected to sit on a volume with the same protection as any other secret —
// a Kubernetes Secret mount, or an encrypted disk. The deployment
// documentation says so in the same terms.
type FileStore struct {
	mu   sync.Mutex
	path string
}

// NewFileStore prepares a store at a path, creating the directory if needed.
func NewFileStore(path string) (*FileStore, error) {
	if path == "" {
		return nil, fmt.Errorf("a path is required for the target store")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("preparing the target store directory: %w", err)
	}
	return &FileStore{path: path}, nil
}

// persistedTarget is the on-disk shape. Credentials are written through
// secret.Bundle's Export rather than its JSON rendering, which redacts — the
// one place in the service where that is the right thing to do.
type persistedTarget struct {
	ID   string        `json:"id"`
	Name string        `json:"name"`
	Kind platform.Kind `json:"kind"`

	// Mode has to survive too. The runner cycles autonomous targets and skips
	// driven ones, so a mode lost here is a target that comes back listed,
	// settings and keys intact, and never scaled again — with nothing
	// anywhere reporting it.
	Mode platform.Mode `json:"mode,omitempty"`

	Config      map[string]string `json:"config"`
	Credentials map[string]string `json:"credentials"`
	Settings    config.Settings   `json:"settings"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// Load reads the stored targets. A file that is not there yet is an empty
// registry, not an error: that is simply a first start.
func (f *FileStore) Load() ([]Persisted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	raw, err := os.ReadFile(f.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", f.path, err)
	}

	var stored []persistedTarget
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("%s is not a usable target store: %w", f.path, err)
	}

	out := make([]Persisted, 0, len(stored))
	for _, s := range stored {
		out = append(out, Persisted{
			Target: platform.Target{
				ID: s.ID, Name: s.Name, Kind: s.Kind, Mode: s.Mode, Config: s.Config,
				Credentials: secret.NewBundle(s.Credentials),
			},
			Settings:  s.Settings,
			CreatedAt: s.CreatedAt,
			UpdatedAt: s.UpdatedAt,
		})
	}
	return out, nil
}

// Save writes the targets, replacing the file atomically.
//
// Atomically because this file is the service's memory of what it is allowed
// to scale: a truncated write interrupted by a restart would leave it with
// half its targets, or none.
func (f *FileStore) Save(targets []Persisted) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	stored := make([]persistedTarget, 0, len(targets))
	for _, t := range targets {
		stored = append(stored, persistedTarget{
			ID: t.Target.ID, Name: t.Target.Name, Kind: t.Target.Kind,
			Mode:        t.Target.Mode,
			Config:      t.Target.Config,
			Credentials: t.Target.Credentials.Export(),
			Settings:    t.Settings,
			CreatedAt:   t.CreatedAt,
			UpdatedAt:   t.UpdatedAt,
		})
	}

	encoded, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the target store: %w", err)
	}

	temporary := f.path + ".tmp"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", temporary, err)
	}
	if err := os.Rename(temporary, f.path); err != nil {
		return fmt.Errorf("replacing %s: %w", f.path, err)
	}
	return nil
}
