package airplay

import (
	"bytes"
	"testing"
)

func TestSRPReceiverProofWithLeadingZeroPublicKey(t *testing.T) {
	// Private exponent 1 makes A=5, with 383 leading zero bytes on the wire.
	random := make([]byte, 32)
	random[31] = 1
	s, err := newSRPWithRandom([]byte("receiver-salt-123"), []byte{7}, "3939", bytes.NewReader(random))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.public) != 384 || s.public[0] != 0 || s.public[383] != 5 {
		t.Fatal("expected padded A=5")
	}
	// Apple HomeKitADK HAP_srp_proof_m2 includes all SRP_PUBLIC_KEY_BYTES.
	if err := s.verify(hash(s.public, s.proof, s.key)); err != nil {
		t.Fatal(err)
	}
	if err := s.verify(hash([]byte{5}, s.proof, s.key)); err == nil {
		t.Fatal("accepted truncated public key proof")
	}
	bad := hash(s.public, s.proof, s.key)
	bad[0] ^= 1
	if err := s.verify(bad); err == nil {
		t.Fatal("accepted corrupted proof")
	}
}
