package secret_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/casperlundberg/autoscaler/internal/secret"
)

func TestGetReturnsWhatWasPutIn(t *testing.T) {
	b := secret.NewBundle(map[string]string{"bearer_token": "s3cr3t"})

	got, ok := b.Get("bearer_token")
	if !ok || got != "s3cr3t" {
		t.Errorf("Get(bearer_token) = (%q, %v), want (\"s3cr3t\", true)", got, ok)
	}
	if _, ok := b.Get("absent"); ok {
		t.Error("Get(absent) reported present")
	}
}

func TestKeysAreSortedSoOutputIsStable(t *testing.T) {
	b := secret.NewBundle(map[string]string{"zeta": "1", "alpha": "2", "mu": "3"})

	got := b.Keys()
	want := []string{"alpha", "mu", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("Keys() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Keys() = %v, want %v", got, want)
		}
	}
}

// The single most important property in this package: a credential bundle
// cannot be serialised. Every path out of this service — an API response, a
// log line, a debug dump of a target — goes through encoding/json, so making
// MarshalJSON redact means there is no route by which a secret leaves.
func TestMarshallingNeverEmitsTheSecret(t *testing.T) {
	b := secret.NewBundle(map[string]string{"colonies_prvkey": "abcdef0123456789"})

	encoded, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	if strings.Contains(string(encoded), "abcdef0123456789") {
		t.Fatalf("Marshal() leaked the secret: %s", encoded)
	}
	if !strings.Contains(string(encoded), "colonies_prvkey") {
		t.Errorf("Marshal() = %s, want the key name kept so an operator can see "+
			"which credentials are set", encoded)
	}
}

func TestMarshallingAStructThatEmbedsABundleAlsoRedacts(t *testing.T) {
	wrapper := struct {
		Name        string        `json:"name"`
		Credentials secret.Bundle `json:"credentials"`
	}{
		Name:        "storhall",
		Credentials: secret.NewBundle(map[string]string{"bearer_token": "leak-me"}),
	}

	encoded, err := json.Marshal(wrapper)
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	if strings.Contains(string(encoded), "leak-me") {
		t.Fatalf("Marshal() leaked through the wrapper: %s", encoded)
	}
}

// A fingerprint answers "did this credential change?" without revealing what
// it is — which is the question an operator comparing two environments asks.
func TestFingerprintsIdentifyAValueWithoutRevealingIt(t *testing.T) {
	same := secret.NewBundle(map[string]string{"token": "value-one"})
	alsoSame := secret.NewBundle(map[string]string{"token": "value-one"})
	different := secret.NewBundle(map[string]string{"token": "value-two"})

	if same.Fingerprint("token") != alsoSame.Fingerprint("token") {
		t.Error("the same value produced different fingerprints")
	}
	if same.Fingerprint("token") == different.Fingerprint("token") {
		t.Error("different values produced the same fingerprint")
	}
	if fp := same.Fingerprint("token"); strings.Contains(fp, "value-one") {
		t.Errorf("Fingerprint() = %q, which contains the value itself", fp)
	}
	if got := same.Fingerprint("absent"); got != "" {
		t.Errorf("Fingerprint(absent) = %q, want empty", got)
	}
}

func TestUnmarshallingAcceptsPlainStringValues(t *testing.T) {
	var b secret.Bundle
	if err := json.Unmarshal([]byte(`{"bearer_token": "s3cr3t"}`), &b); err != nil {
		t.Fatalf("Unmarshal() = %v", err)
	}

	if got, _ := b.Get("bearer_token"); got != "s3cr3t" {
		t.Errorf("Get(bearer_token) = %q, want \"s3cr3t\"", got)
	}
}

// The round trip that matters: the UI GETs a target, edits the namespace, and
// PUTs the whole thing back. The credentials it read back were redacted, and
// sending them again must not wipe the real ones.
func TestARedactedValueSentBackMeansLeaveItAlone(t *testing.T) {
	stored := secret.NewBundle(map[string]string{
		"bearer_token":    "original-token",
		"colonies_prvkey": "original-key",
	})

	roundTripped, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}

	var submitted secret.Bundle
	if err := json.Unmarshal(roundTripped, &submitted); err != nil {
		t.Fatalf("Unmarshal() = %v", err)
	}

	merged := submitted.MergeOnto(stored)
	if got, _ := merged.Get("bearer_token"); got != "original-token" {
		t.Errorf("bearer_token = %q, want the stored value preserved", got)
	}
	if got, _ := merged.Get("colonies_prvkey"); got != "original-key" {
		t.Errorf("colonies_prvkey = %q, want the stored value preserved", got)
	}
}

func TestANewValueReplacesTheStoredOne(t *testing.T) {
	stored := secret.NewBundle(map[string]string{"bearer_token": "original"})

	var submitted secret.Bundle
	if err := json.Unmarshal([]byte(`{"bearer_token": "rotated"}`), &submitted); err != nil {
		t.Fatalf("Unmarshal() = %v", err)
	}

	if got, _ := submitted.MergeOnto(stored).Get("bearer_token"); got != "rotated" {
		t.Errorf("bearer_token = %q, want \"rotated\"", got)
	}
}

func TestAnEmptyStringClearsACredential(t *testing.T) {
	stored := secret.NewBundle(map[string]string{"registry_auth": "old"})

	var submitted secret.Bundle
	if err := json.Unmarshal([]byte(`{"registry_auth": ""}`), &submitted); err != nil {
		t.Fatalf("Unmarshal() = %v", err)
	}

	// Explicitly empty is a deliberate act — removing a credential a target no
	// longer needs — and has to be distinguishable from "not mentioned".
	merged := submitted.MergeOnto(stored)
	if got, ok := merged.Get("registry_auth"); ok && got != "" {
		t.Errorf("registry_auth = %q, want it cleared", got)
	}
}

func TestAKeyOmittedEntirelyIsDroppedNotKept(t *testing.T) {
	stored := secret.NewBundle(map[string]string{"a": "1", "b": "2"})

	var submitted secret.Bundle
	if err := json.Unmarshal([]byte(`{"a": "1-new"}`), &submitted); err != nil {
		t.Fatalf("Unmarshal() = %v", err)
	}

	// Only redaction markers mean "keep". A key the caller simply left out of
	// a full replacement is gone, which is what makes PUT a replace.
	merged := submitted.MergeOnto(stored)
	if _, ok := merged.Get("b"); ok {
		t.Error("b survived a replacement that did not mention it")
	}
}

func TestBundlesDoNotShareTheirBackingMap(t *testing.T) {
	source := map[string]string{"token": "value"}
	b := secret.NewBundle(source)

	source["token"] = "changed-behind-its-back"

	if got, _ := b.Get("token"); got != "value" {
		t.Errorf("Get(token) = %q, want the bundle to have copied its input", got)
	}
}

func TestAZeroBundleIsUsable(t *testing.T) {
	var b secret.Bundle

	if got, ok := b.Get("anything"); ok || got != "" {
		t.Errorf("Get() on a zero bundle = (%q, %v), want (\"\", false)", got, ok)
	}
	if len(b.Keys()) != 0 {
		t.Errorf("Keys() on a zero bundle = %v, want empty", b.Keys())
	}
	if _, err := json.Marshal(b); err != nil {
		t.Errorf("Marshal() on a zero bundle = %v", err)
	}
}
