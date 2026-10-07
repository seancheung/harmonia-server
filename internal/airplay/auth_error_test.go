package airplay

import (
	"errors"
	"testing"
)

func TestAuthenticationFailureNeedsPairing(t *testing.T) {
	_, err := untlv([]byte{6, 1, 2, 7, 1, 2})
	if !errors.Is(err, ErrPairingRequired) {
		t.Fatalf("authentication rejection lost: %v", err)
	}
	for _, data := range [][]byte{{7, 1, 6}, {7, 1, 3}, {7, 9}} {
		_, err := untlv(data)
		if err == nil || errors.Is(err, ErrPairingRequired) {
			t.Fatalf("busy/backoff/malformed response should not request PIN: %v", err)
		}
	}
}
