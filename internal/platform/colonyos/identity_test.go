package colonyos_test

import (
	"strings"
	"testing"

	"github.com/casperlundberg/autoscaler/internal/platform/colonyos"
)

// These values were produced by the ColonyOS implementation itself
// (internal/crypto in the colonies server source), for the private key and
// payload below. They are here so this reimplementation is checked against the
// protocol as it actually is, rather than against my reading of it: a
// signature scheme that is subtly wrong authenticates against nothing, and
// would only be discovered against a live server.
const (
	goldenPrivateKey = "fcc79953d8a751bf41db661592dc34d30004b1a651ffa0725b03ac227641499d"
	goldenID         = "039231c7644e04b6895471dd5335cf332681c54e27f81fac54f9067b3f2c0103"
	goldenPayload    = "eyJjb2xvbnluYW1lIjoiZGV2IiwiY291bnQiOjEwMCwic3RhdGUiOjAsImV4ZWN1dG9ydHlwZSI6ImJlbWlzIiwibGFiZWwiOiIiLCJpbml0aWF0b3IiOiIiLCJtc2d0eXBlIjoiZ2V0cHJvY2Vzc2VzbXNnIn0="
	goldenSignature  = "ca0d5189e09b53667f5341a2341cb29c9d06077be7c8e1bd11c3a83b12b543425e0d06316dc7308a01ba6d1615b00e5ac13d2bd0466cfad024bb0949c920415d01"
)

func TestTheIdentityDerivedFromAKeyMatchesColonyOS(t *testing.T) {
	identity, err := colonyos.NewIdentity(goldenPrivateKey)
	if err != nil {
		t.Fatalf("NewIdentity() = %v", err)
	}

	if got := identity.ID(); got != goldenID {
		t.Errorf("ID() = %q, want %q", got, goldenID)
	}
}

func TestASignatureMatchesColonyOSByteForByte(t *testing.T) {
	identity, err := colonyos.NewIdentity(goldenPrivateKey)
	if err != nil {
		t.Fatalf("NewIdentity() = %v", err)
	}

	got, err := identity.Sign(goldenPayload)
	if err != nil {
		t.Fatalf("Sign() = %v", err)
	}
	if got != goldenSignature {
		t.Errorf("Sign() = %q, want %q", got, goldenSignature)
	}
}

// Recovery is how a ColonyOS server authenticates a request: it recovers the
// signer's identity from the signature and checks it is a member. Having it
// here means the tests can stand up a server that authenticates for real.
func TestAnIdentityCanBeRecoveredFromItsOwnSignature(t *testing.T) {
	got, err := colonyos.RecoverID(goldenPayload, goldenSignature)
	if err != nil {
		t.Fatalf("RecoverID() = %v", err)
	}
	if got != goldenID {
		t.Errorf("RecoverID() = %q, want %q", got, goldenID)
	}
}

func TestATamperedPayloadRecoversADifferentIdentity(t *testing.T) {
	got, err := colonyos.RecoverID(goldenPayload+"x", goldenSignature)
	if err == nil && got == goldenID {
		t.Error("a tampered payload recovered the original identity")
	}
}

func TestAKeyOfTheWrongLengthIsRefused(t *testing.T) {
	_, err := colonyos.NewIdentity("abcd")
	if err == nil {
		t.Fatal("NewIdentity() = nil error for a short key")
	}
	if !strings.Contains(err.Error(), "32 bytes") && !strings.Contains(err.Error(), "256 bits") {
		t.Errorf("NewIdentity() = %q, want it to say what a valid key looks like", err)
	}
}

func TestAKeyThatIsNotHexIsRefused(t *testing.T) {
	if _, err := colonyos.NewIdentity(strings.Repeat("z", 64)); err == nil {
		t.Fatal("NewIdentity() = nil error for a non-hex key")
	}
}

func TestAZeroKeyIsRefused(t *testing.T) {
	if _, err := colonyos.NewIdentity(strings.Repeat("0", 64)); err == nil {
		t.Fatal("NewIdentity() = nil error for a zero key")
	}
}
