// Package colonyos speaks the ColonyOS RPC protocol.
//
// The protocol is small: every request is a JSON envelope carrying a payload
// type, the base64 payload, and a secp256k1 signature over that base64 string.
// The server authenticates by recovering the signer's identity from the
// signature, so a client needs nothing but the executor's private key.
//
// It is implemented here rather than by importing the ColonyOS client library
// because that library brings a server's worth of dependencies — etcd, gin,
// gRPC — for two calls. The signature scheme is checked against golden values
// produced by ColonyOS's own code, so wire compatibility is a tested property
// rather than a hope.
package colonyos

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/btcsuite/btcd/btcec/v2"
	becdsa "github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"golang.org/x/crypto/sha3"
)

// signatureLength is 64 bytes of signature plus one recovery byte.
const signatureLength = 65

// recoveryIDOffset is where the recovery byte sits in ColonyOS's layout.
const recoveryIDOffset = 64

// Identity is an executor's or user's ColonyOS credential.
type Identity struct {
	private *ecdsa.PrivateKey
	id      string
}

// NewIdentity parses a hex-encoded secp256k1 private key.
func NewIdentity(hexPrivateKey string) (*Identity, error) {
	decoded, err := hex.DecodeString(hexPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("private key is not hex: %w", err)
	}

	curve := btcec.S256()
	if bits := 8 * len(decoded); bits != curve.Params().BitSize {
		return nil, fmt.Errorf("private key must be 32 bytes (256 bits) of hex, got %d bits", bits)
	}

	private := new(ecdsa.PrivateKey)
	private.PublicKey.Curve = curve
	private.D = new(big.Int).SetBytes(decoded)
	if private.D.Sign() <= 0 || private.D.Cmp(curve.Params().N) >= 0 {
		return nil, fmt.Errorf("private key is out of range for secp256k1")
	}
	private.PublicKey.X, private.PublicKey.Y = curve.ScalarBaseMult(decoded)

	return &Identity{private: private, id: idFromPublicKey(publicKeyBytes(private))}, nil
}

// ID is the ColonyOS identifier this key authenticates as: the SHA3-256 of the
// hex-encoded uncompressed public key.
func (i *Identity) ID() string { return i.id }

// Sign produces the hex signature ColonyOS expects over a base64 payload.
func (i *Identity) Sign(payload string) (string, error) {
	hash := hashOf(payload)

	var key btcec.PrivateKey
	if overflow := key.Key.SetByteSlice(i.private.D.Bytes()); overflow || key.Key.IsZero() {
		return "", fmt.Errorf("private key is not usable for signing")
	}
	defer key.Zero()

	// SignCompact puts the recovery byte first and offsets it by 27; ColonyOS
	// moves it to the end and removes the offset. Getting this wrong produces
	// a signature that verifies nowhere, which is why it is pinned by a golden
	// test against ColonyOS's own output.
	compact, err := becdsa.SignCompact(&key, hash, false)
	if err != nil {
		return "", fmt.Errorf("signing: %w", err)
	}
	recovery := compact[0] - 27
	copy(compact, compact[1:])
	compact[recoveryIDOffset] = recovery

	return hex.EncodeToString(compact), nil
}

// RecoverID returns the identity that produced a signature over a payload.
// This is how a ColonyOS server authenticates a caller, and it lets a test
// stand up a server that authenticates for real rather than waving requests
// through.
func RecoverID(payload, hexSignature string) (string, error) {
	signature, err := hex.DecodeString(hexSignature)
	if err != nil {
		return "", fmt.Errorf("signature is not hex: %w", err)
	}
	if len(signature) != signatureLength {
		return "", fmt.Errorf("signature must be %d bytes, got %d", signatureLength, len(signature))
	}

	compact := make([]byte, signatureLength)
	compact[0] = signature[recoveryIDOffset] + 27
	copy(compact[1:], signature)

	public, _, err := becdsa.RecoverCompact(compact, hashOf(payload))
	if err != nil {
		return "", fmt.Errorf("recovering the signer: %w", err)
	}
	return idFromPublicKey(public.SerializeUncompressed()), nil
}

func hashOf(payload string) []byte {
	digest := sha3.New256()
	digest.Write([]byte(payload))
	return digest.Sum(nil)
}

func publicKeyBytes(private *ecdsa.PrivateKey) []byte {
	return elliptic.Marshal(btcec.S256(), private.PublicKey.X, private.PublicKey.Y)
}

func idFromPublicKey(public []byte) string {
	return hex.EncodeToString(hashOf(hex.EncodeToString(public)))
}
