package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ErrVersionConflict is returned when a write carries an expected version that
// is no longer current — someone else changed the settings in between.
var ErrVersionConflict = errors.New("settings were changed by someone else")

// historyLimit is how many changes are kept in memory. Enough to answer "what
// just happened and who did it" during an incident, which is what this log is
// for; durable audit belongs in whatever collects the service's logs.
const historyLimit = 200

// Snapshot is one version of the settings, with the provenance needed to
// explain a decision taken under it.
type Snapshot struct {
	Version   int64     `json:"version"`
	Settings  Settings  `json:"settings"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// FieldChange is one field that moved, rendered as it appears on the wire so
// the log reads the same as the API.
type FieldChange struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// Change is one accepted write.
type Change struct {
	Version int64         `json:"version"`
	At      time.Time     `json:"at"`
	Actor   string        `json:"actor,omitempty"`
	Fields  []FieldChange `json:"fields"`
}

// Store holds the settings the service is running under and replaces them
// while it runs.
//
// The swap is the whole point. Readers take a consistent copy of one version;
// a writer builds a complete candidate, validates it, and only then makes it
// current. There is no moment at which a decision could be taken under half of
// one document and half of another — which is exactly what a naive
// field-by-field mutable config would allow, and exactly the kind of bug that
// only appears under load.
type Store struct {
	mu      sync.RWMutex
	current Snapshot
	history []Change

	subscribers map[int]chan Snapshot
	nextSub     int

	// now is injectable so tests do not have to sleep.
	now func() time.Time
}

// NewStore validates the initial settings and refuses to start under settings
// the engine could not run on. Failing at startup is far better than accepting
// them and failing on the first decision cycle.
func NewStore(initial Settings) (*Store, error) {
	if err := initial.Validate(); err != nil {
		return nil, fmt.Errorf("initial settings are not usable: %w", err)
	}
	now := time.Now
	return &Store{
		current: Snapshot{
			Version:   1,
			Settings:  initial.Clone(),
			UpdatedAt: now().UTC(),
			UpdatedBy: "startup",
		},
		subscribers: map[int]chan Snapshot{},
		now:         now,
	}, nil
}

// Current is the settings in force, as a copy. Callers get a document they can
// hold and read at their leisure without it changing underneath them, and
// without being able to reach back and change what the engine is running on.
func (s *Store) Current() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snapshot := s.current
	snapshot.Settings = s.current.Settings.Clone()
	return snapshot
}

// Apply merges a JSON document onto the current settings and makes the result
// current, if it validates.
//
// expectedVersion, when given, makes this a compare-and-swap: the write is
// refused if the settings moved since the caller read them. The settings
// editor sends it, so two people tuning the same target cannot silently
// clobber each other. A scripted rollout that means "set this regardless"
// passes nil.
func (s *Store) Apply(patch []byte, expectedVersion *int64, actor string) (Snapshot, error) {
	s.mu.Lock()

	if expectedVersion != nil && *expectedVersion != s.current.Version {
		current := s.current.Version
		s.mu.Unlock()
		return Snapshot{}, fmt.Errorf("%w: you edited version %d, current is %d",
			ErrVersionConflict, *expectedVersion, current)
	}

	candidate := s.current.Settings.Clone()
	if err := json.Unmarshal(patch, &candidate); err != nil {
		s.mu.Unlock()
		return Snapshot{}, fmt.Errorf("settings document rejected: %w", err)
	}
	if err := candidate.Validate(); err != nil {
		s.mu.Unlock()
		return Snapshot{}, fmt.Errorf("settings document rejected: %w", err)
	}

	fields, err := diff(s.current.Settings, candidate)
	if err != nil {
		s.mu.Unlock()
		return Snapshot{}, err
	}

	next := Snapshot{
		Version:   s.current.Version + 1,
		Settings:  candidate,
		UpdatedAt: s.now().UTC(),
		UpdatedBy: actor,
	}
	s.current = next

	s.history = append(s.history, Change{
		Version: next.Version,
		At:      next.UpdatedAt,
		Actor:   actor,
		Fields:  fields,
	})
	if len(s.history) > historyLimit {
		s.history = s.history[len(s.history)-historyLimit:]
	}

	// Copy the subscriber set under the lock, publish outside it: a slow
	// reader must never be able to hold the settings API closed.
	targets := make([]chan Snapshot, 0, len(s.subscribers))
	for _, ch := range s.subscribers {
		targets = append(targets, ch)
	}
	s.mu.Unlock()

	published := next
	published.Settings = next.Settings.Clone()
	for _, ch := range targets {
		publish(ch, published)
	}

	return s.Current(), nil
}

// publish delivers the newest snapshot without ever blocking. If a subscriber
// has not drained the previous one, that one is dropped: a settings feed only
// ever needs the latest value, and dropping stale versions is better than
// stalling the writer or growing an unbounded queue behind a dead UI.
func publish(ch chan Snapshot, snapshot Snapshot) {
	for {
		select {
		case ch <- snapshot:
			return
		default:
		}
		select {
		case <-ch:
		default:
			// Drained by the subscriber between the two selects; try again.
		}
	}
}

// History is the most recent accepted changes, newest first.
func (s *Store) History(limit int) []Change {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > len(s.history) {
		limit = len(s.history)
	}
	out := make([]Change, 0, limit)
	for i := len(s.history) - 1; i >= len(s.history)-limit; i-- {
		out = append(out, s.history[i])
	}
	return out
}

// Subscribe returns a channel of accepted settings versions and a function
// that stops the subscription and closes the channel. The returned function is
// safe to call more than once.
func (s *Store) Subscribe() (<-chan Snapshot, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := s.nextSub
	s.nextSub++
	ch := make(chan Snapshot, 1)
	s.subscribers[id] = ch

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if existing, ok := s.subscribers[id]; ok {
				delete(s.subscribers, id)
				close(existing)
			}
		})
	}
}

// diff reports which wire fields moved between two documents.
//
// It works on the JSON rendering rather than on the Go struct, so the audit
// log names fields exactly as the API and the settings editor do. A field
// added to Settings is covered automatically, with no second list to keep in
// step.
func diff(before, after Settings) ([]FieldChange, error) {
	beforeFields, err := flatten(before)
	if err != nil {
		return nil, err
	}
	afterFields, err := flatten(after)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(afterFields))
	for name := range afterFields {
		names = append(names, name)
	}
	for name := range beforeFields {
		if _, ok := afterFields[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var changes []FieldChange
	for _, name := range names {
		if beforeFields[name] != afterFields[name] {
			changes = append(changes, FieldChange{
				Field: name,
				From:  beforeFields[name],
				To:    afterFields[name],
			})
		}
	}
	return changes, nil
}

func flatten(s Settings) (map[string]string, error) {
	encoded, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("rendering settings for the change log: %w", err)
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &generic); err != nil {
		return nil, fmt.Errorf("reading back settings for the change log: %w", err)
	}

	out := make(map[string]string, len(generic))
	for name, raw := range generic {
		out[name] = string(raw)
	}
	return out, nil
}
