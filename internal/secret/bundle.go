// Package secret holds the access keys a platform adapter needs to provision
// on an operator's behalf: Kubernetes bearer tokens, ColonyOS executor private
// keys, Docker client certificates, registry credentials.
//
// The design goal is narrow and absolute: material that enters this package
// does not come back out through any reporting path. It leaves only when an
// adapter deliberately asks for one named value in order to make a call.
package secret

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// Bundle is a set of named credentials.
//
// The zero value is usable and empty. Bundles are values, copied freely, and
// never share their backing map with anything the caller still holds.
type Bundle struct {
	values map[string]string

	// keep records keys the caller sent back in redacted form, which means
	// "leave whatever is stored alone". It is only ever populated by
	// UnmarshalJSON — a bundle built in code has nothing to preserve.
	keep map[string]bool
}

// NewBundle copies the given values into a bundle. The caller's map is not
// retained, so mutating it afterwards cannot change the credentials in use.
func NewBundle(values map[string]string) Bundle {
	copied := make(map[string]string, len(values))
	for k, v := range values {
		copied[k] = v
	}
	return Bundle{values: copied}
}

// Get returns one credential. This is the only way material leaves the bundle,
// and adapters call it at the moment they build a request.
func (b Bundle) Get(key string) (string, bool) {
	v, ok := b.values[key]
	return v, ok
}

// Keys is the credential names present, sorted. Names, never values — enough
// for an operator to see which keys a target has been given.
func (b Bundle) Keys() []string {
	out := make([]string, 0, len(b.values))
	for k := range b.values {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Fingerprint identifies a credential without revealing it, so two
// environments can be compared, or a rotation confirmed, from a log or an API
// response. Empty for a key that is not set.
func (b Bundle) Fingerprint(key string) string {
	v, ok := b.values[key]
	if !ok {
		return ""
	}
	sum := sha256.Sum256([]byte(v))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

// redactedValue is what a credential looks like on the way out.
type redactedValue struct {
	Present     bool   `json:"present"`
	Fingerprint string `json:"fingerprint"`
}

// MarshalJSON emits key names and fingerprints, never values.
//
// This is the load-bearing method in the package. Every route out of this
// service — an API response, a structured log line, a debug dump of a target —
// runs through encoding/json, so redacting here means there is no path by
// which a credential is disclosed, including ones added later by someone who
// never read this file.
func (b Bundle) MarshalJSON() ([]byte, error) {
	out := make(map[string]redactedValue, len(b.values))
	for k := range b.values {
		out[k] = redactedValue{Present: true, Fingerprint: b.Fingerprint(k)}
	}
	return json.Marshal(out)
}

// UnmarshalJSON accepts either a real credential, as a JSON string, or the
// redacted form this package emits, which means "keep what is stored".
//
// That second case is what makes the obvious UI flow safe: read a target, edit
// its namespace, send the whole thing back. The credentials that came back
// were redacted, and sending them again must not destroy the real ones.
func (b *Bundle) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("credentials must be an object of names to values: %w", err)
	}

	values := make(map[string]string, len(raw))
	keep := make(map[string]bool)

	for key, encoded := range raw {
		var literal string
		if err := json.Unmarshal(encoded, &literal); err == nil {
			values[key] = literal
			continue
		}

		var marker redactedValue
		if err := json.Unmarshal(encoded, &marker); err == nil && marker.Present {
			keep[key] = true
			continue
		}

		return fmt.Errorf("credential %q must be a string, or the redacted form "+
			"this API returns to mean 'unchanged'", key)
	}

	*b = Bundle{values: values, keep: keep}
	return nil
}

// MergeOnto resolves a submitted bundle against what is already stored.
//
// A key sent as a redaction marker keeps the stored value. A key sent as a
// string replaces it, including an empty string, which deliberately clears a
// credential a target no longer needs. A key not mentioned at all is dropped —
// that is what makes a full submission a replacement rather than an
// ever-growing accumulation of credentials nobody remembers adding.
func (b Bundle) MergeOnto(stored Bundle) Bundle {
	merged := make(map[string]string, len(b.values)+len(b.keep))
	for k, v := range b.values {
		merged[k] = v
	}
	for k := range b.keep {
		if v, ok := stored.Get(k); ok {
			merged[k] = v
		}
	}
	return Bundle{values: merged}
}
