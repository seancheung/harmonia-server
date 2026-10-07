package airplay

import "testing"

func TestVolumeParsing(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{{"volume: -144\r\n", 0}, {"volume: -30\r\n", 0}, {"volume: -15\r\n", 50}, {"volume: 0\r\n", 100}, {"Volume: -28.2\r\n", 6}} {
		got, err := parseVolume([]byte(tc.body))
		if err != nil || got != tc.want {
			t.Fatalf("%q: got %d %v", tc.body, got, err)
		}
	}
	for _, body := range []string{"", "volume: NaN", "volume: +Inf", "volume: 2", "volume: -200", "volume: bad"} {
		if _, err := parseVolume([]byte(body)); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
}
