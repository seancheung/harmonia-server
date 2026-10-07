package airplay

import (
	"context"
	"os"
	"testing"
)

// Explicitly opt in: this test sends discovery queries, never playback commands.
func TestDiscoverLAN(t *testing.T) {
	if os.Getenv("HARMONIA_TEST_DISCOVERY") != "1" {
		t.Skip("set HARMONIA_TEST_DISCOVERY=1 to query the LAN")
	}
	devices, err := Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, device := range devices {
		t.Logf("%s (%s:%d) %s %s", device.Name, device.Address, device.Port, device.Type, device.UnsupportedReason)
	}
	t.Logf("discovered %d AirPlay receivers", len(devices))
}
