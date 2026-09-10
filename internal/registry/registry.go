// Package registry holds the targets this service scales: which platform each
// one is on, the access keys to act on it, the settings it runs under, and
// what happened on its last cycle.
package registry

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/platform"
	"github.com/casperlundberg/autoscaler/internal/policy"
	"github.com/casperlundberg/autoscaler/internal/secret"
)

// Sentinel errors, so callers — the HTTP layer above all — can map a failure
// to the right status code without matching on message text.
var (
	ErrNotFound      = errors.New("no such target")
	ErrAlreadyExists = errors.New("a target with this id already exists")
)

// idPattern is what a target id has to look like.
//
// The id is not decorative: it becomes part of Deployment names, container
// names and Secret names on the platforms below. Accepting one that is illegal
// there would produce a target that registers cleanly and then fails on its
// first scale-up, which is the worst moment to discover it.
var idPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Snapshot is a consistent view of one target.
type Snapshot struct {
	Target   platform.Target `json:"target"`
	Settings config.Snapshot `json:"settings"`

	Loop policy.LoopState `json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// LastDecision, LastError and LastCycleAt are what the status view and the
	// UI read. A target that has been failing for an hour must be visible
	// without anyone going to the logs first.
	LastDecision *domain.Decision `json:"last_decision,omitempty"`
	LastError    string           `json:"last_error,omitempty"`
	LastCycleAt  time.Time        `json:"last_cycle_at,omitempty"`
}

// Cycle is what one control loop iteration reports back.
type Cycle struct {
	Loop     policy.LoopState
	Decision *domain.Decision
	Err      error
	At       time.Time
}

// record is the mutable state behind a Snapshot.
type record struct {
	target   platform.Target
	settings *config.Store

	loop         policy.LoopState
	lastDecision *domain.Decision
	lastError    string
	lastCycleAt  time.Time

	createdAt time.Time
	updatedAt time.Time
}

// Registry is the set of registered targets.
type Registry struct {
	mu        sync.RWMutex
	records   map[string]*record
	platforms *platform.Registry
	store     Store

	now func() time.Time
}

// New builds a registry, restoring whatever the store already holds.
func New(platforms *platform.Registry, store Store) (*Registry, error) {
	r := &Registry{
		records:   map[string]*record{},
		platforms: platforms,
		store:     store,
		now:       func() time.Time { return time.Now().UTC() },
	}

	persisted, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("restoring registered targets: %w", err)
	}
	for _, p := range persisted {
		settings, err := config.NewStore(p.Settings)
		if err != nil {
			return nil, fmt.Errorf("target %q was persisted with settings that are no "+
				"longer valid: %w", p.Target.ID, err)
		}
		r.records[p.Target.ID] = &record{
			target:    p.Target,
			settings:  settings,
			createdAt: p.CreatedAt,
			updatedAt: p.UpdatedAt,
		}
	}
	return r, nil
}

// Create registers a target, after proving both that its settings are usable
// and that its access keys actually work.
//
// Validating now rather than lazily is the whole point: a target with a
// mistyped Deployment name or an expired token should be rejected while
// someone is looking at the response, not discovered during the first burst it
// was supposed to absorb.
func (r *Registry) Create(ctx context.Context, target platform.Target,
	settings config.Settings) (Snapshot, error) {
	if err := validateID(target.ID); err != nil {
		return Snapshot{}, err
	}

	provisioner, err := r.platforms.Get(target.Kind)
	if err != nil {
		return Snapshot{}, err
	}
	store, err := config.NewStore(settings)
	if err != nil {
		return Snapshot{}, err
	}
	if err := provisioner.Validate(ctx, target); err != nil {
		return Snapshot{}, fmt.Errorf("target %q is not usable: %w", target.ID, err)
	}

	r.mu.Lock()
	if _, exists := r.records[target.ID]; exists {
		r.mu.Unlock()
		return Snapshot{}, fmt.Errorf("%w: %s", ErrAlreadyExists, target.ID)
	}
	now := r.now()
	r.records[target.ID] = &record{
		target: target, settings: store, createdAt: now, updatedAt: now,
	}
	r.mu.Unlock()

	if err := r.persist(); err != nil {
		return Snapshot{}, err
	}
	return r.Get(target.ID)
}

// Update replaces a target's platform settings, keeping its policy settings
// and its credentials.
//
// Keeping both is what makes the obvious UI flow safe. Editing a namespace
// must not silently reset the policy the target has been tuned to, and
// resubmitting the redacted credentials the API just handed out must not
// destroy the real ones.
func (r *Registry) Update(ctx context.Context, target platform.Target) (Snapshot, error) {
	provisioner, err := r.platforms.Get(target.Kind)
	if err != nil {
		return Snapshot{}, err
	}

	r.mu.RLock()
	existing, ok := r.records[target.ID]
	var stored secret.Bundle
	if ok {
		stored = existing.target.Credentials
	}
	r.mu.RUnlock()
	if !ok {
		return Snapshot{}, fmt.Errorf("%w: %s", ErrNotFound, target.ID)
	}

	target.Credentials = target.Credentials.MergeOnto(stored)
	if err := provisioner.Validate(ctx, target); err != nil {
		return Snapshot{}, fmt.Errorf("target %q is not usable: %w", target.ID, err)
	}

	r.mu.Lock()
	current, ok := r.records[target.ID]
	if !ok {
		r.mu.Unlock()
		return Snapshot{}, fmt.Errorf("%w: %s", ErrNotFound, target.ID)
	}
	current.target = target
	current.updatedAt = r.now()
	r.mu.Unlock()

	if err := r.persist(); err != nil {
		return Snapshot{}, err
	}
	return r.Get(target.ID)
}

// Get is one target's current state.
func (r *Registry) Get(id string) (Snapshot, error) {
	r.mu.RLock()
	rec, ok := r.records[id]
	r.mu.RUnlock()
	if !ok {
		return Snapshot{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return r.snapshot(rec), nil
}

// List is every target, ordered by id so a UI listing does not reshuffle
// between refreshes.
func (r *Registry) List() []Snapshot {
	r.mu.RLock()
	records := make([]*record, 0, len(r.records))
	for _, rec := range r.records {
		records = append(records, rec)
	}
	r.mu.RUnlock()

	sort.Slice(records, func(i, j int) bool { return records[i].target.ID < records[j].target.ID })

	out := make([]Snapshot, 0, len(records))
	for _, rec := range records {
		out = append(out, r.snapshot(rec))
	}
	return out
}

// Settings is a target's live settings store, which the API writes through and
// the control loop reads on every cycle. It is the store itself, not a copy:
// that is what makes a settings change take effect on the next cycle without
// anything being restarted or reloaded.
func (r *Registry) Settings(id string) (*config.Store, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rec, ok := r.records[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return rec.settings, nil
}

// ApplySettings changes a target's settings and makes the change durable
// before saying it succeeded.
//
// This is the only write path for settings, and it is synchronous on purpose.
// Persisting in the background would let an operator be told a change landed,
// see it take effect on the running loop, and then find it silently reverted
// by the next restart — with no reason to suspect anything had gone wrong.
func (r *Registry) ApplySettings(id string, patch []byte, expectedVersion *int64,
	actor string) (config.Snapshot, error) {
	store, err := r.Settings(id)
	if err != nil {
		return config.Snapshot{}, err
	}

	snapshot, err := store.Apply(patch, expectedVersion, actor)
	if err != nil {
		return config.Snapshot{}, err
	}
	if err := r.persist(); err != nil {
		return config.Snapshot{}, err
	}
	return snapshot, nil
}

// Credentials is a target's access keys, for the control loop to act with.
// Everything that renders a target for a caller uses the Target's own bundle,
// which redacts.
func (r *Registry) Credentials(id string) (secret.Bundle, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rec, ok := r.records[id]
	if !ok {
		return secret.Bundle{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return rec.target.Credentials, nil
}

// Delete removes a target.
func (r *Registry) Delete(id string) error {
	r.mu.Lock()
	if _, ok := r.records[id]; !ok {
		r.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	delete(r.records, id)
	r.mu.Unlock()

	return r.persist()
}

// RecordCycle stores what one control loop iteration produced.
//
// It is not persisted. Loop state is hysteresis memory measured in minutes,
// and writing the target file on every cycle of every target would be a lot of
// I/O to preserve something that is stale by the time a restart finishes.
func (r *Registry) RecordCycle(id string, cycle Cycle) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.records[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	rec.loop = cycle.Loop
	if cycle.Decision != nil {
		rec.lastDecision = cycle.Decision
	}
	if cycle.Err != nil {
		rec.lastError = cycle.Err.Error()
	} else {
		// Cleared by a cycle that worked, so the status view shows a problem
		// that is still happening rather than one that happened once.
		rec.lastError = ""
	}

	at := cycle.At
	if at.IsZero() {
		at = r.now()
	}
	rec.lastCycleAt = at
	return nil
}

func (r *Registry) snapshot(rec *record) Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return Snapshot{
		Target:       rec.target,
		Settings:     rec.settings.Current(),
		Loop:         rec.loop,
		CreatedAt:    rec.createdAt,
		UpdatedAt:    rec.updatedAt,
		LastDecision: rec.lastDecision,
		LastError:    rec.lastError,
		LastCycleAt:  rec.lastCycleAt,
	}
}

func (r *Registry) persist() error {
	r.mu.RLock()
	out := make([]Persisted, 0, len(r.records))
	for _, rec := range r.records {
		out = append(out, Persisted{
			Target:    rec.target,
			Settings:  rec.settings.Current().Settings,
			CreatedAt: rec.createdAt,
			UpdatedAt: rec.updatedAt,
		})
	}
	r.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Target.ID < out[j].Target.ID })
	return r.store.Save(out)
}

func validateID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("target id %q is not usable: it becomes part of Deployment, "+
			"container and Secret names, so it must be 1-63 characters of lowercase "+
			"letters, digits and hyphens, starting and ending with a letter or digit", id)
	}
	return nil
}
