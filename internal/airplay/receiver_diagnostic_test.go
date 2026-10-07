package airplay

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in hardware diagnostic. Sends no audio unless silence is explicitly enabled.
func TestReceiverTiming(t *testing.T) {
	dir := os.Getenv("HARMONIA_TEST_RECEIVER_DATA")
	if dir == "" {
		t.Skip("set HARMONIA_TEST_RECEIVER_DATA to diagnose the saved receiver")
	}
	var saved struct {
		Output string `json:"output"`
	}
	var identity struct {
		ID          string                 `json:"id"`
		Credentials map[string]Credentials `json:"credentials"`
	}
	for name, dst := range map[string]any{"remote.json": &saved, "airplay-identity.json": &identity} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, dst); err != nil {
			t.Fatal(err)
		}
	}
	if target := os.Getenv("HARMONIA_TEST_RECEIVER_ID"); target != "" {
		saved.Output = target
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	devices, err := Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, device := range devices {
		if device.ID != saved.Output {
			continue
		}
		var cred *Credentials
		if v, ok := identity.Credentials[device.ID]; ok {
			cred = &v
		}
		if os.Getenv("HARMONIA_TEST_RECEIVER_VOLUME") == "1" {
			volume, err := ReadVolume(ctx, device, identity.ID, cred)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("receiver current volume=%d%%", volume)
			return
		}
		s, err := Connect(ctx, device, identity.ID, cred)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		t.Logf("receiver=%s PTP=%v fallback=%v", device.Name, s.clock.ptp, s.clock.fallbackErr)
		timer := time.NewTimer(8 * time.Second)
		defer timer.Stop()
		<-timer.C
		t.Logf("timing request received=%v session error=%v", s.clock.lastProbe.Load() != 0, s.err())
		if os.Getenv("HARMONIA_TEST_RECEIVER_SILENCE") == "1" {
			if err := s.Stream(ctx, bytes.NewReader(make([]byte, SampleRate*4*3)), func(time.Duration) {}); err != nil {
				t.Fatal(err)
			}
			t.Logf("sent %d silence packets", s.counter)
			method, path := "POST", "/feedback"
			if s.legacy {
				method, path = "OPTIONS", "*"
			}
			response, err := s.ctrl.request(method, path, "", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(response.body) > 0 {
				value, err := unplist(response.body)
				if err != nil {
					t.Fatal(err)
				}
				if fields, ok := value.(map[string]any); ok {
					for key, value := range fields {
						switch value.(type) {
						case uint64, float64, bool, string:
							t.Logf("feedback %s=%v", key, value)
						default:
							if streams, ok := value.([]any); ok && key == "streams" {
								for _, stream := range streams {
									if fields, ok := stream.(map[string]any); ok {
										for field, v := range fields {
											switch v.(type) {
											case uint64, int64, float64, bool, string:
												t.Logf("stream %s=%v", field, v)
											}
										}
									}
								}
							} else {
								t.Logf("feedback %s (%T)", key, value)
							}
						}
					}
				}
			} else {
				t.Log("receiver feedback has no rendering statistics")
			}
		}
		if s.clock.lastProbe.Load() == 0 {
			t.Fatal("receiver did not send timing requests")
		}
		return
	}
	t.Fatal("saved receiver not discovered")
}
