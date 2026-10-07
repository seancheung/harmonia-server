package airplay

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
)

func parseVolume(body []byte) (int, error) {
	for _, line := range strings.Split(string(body), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "volume") {
			continue
		}
		db, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || math.IsNaN(db) || math.IsInf(db, 0) || db > 0 || db < -144 {
			return 0, errors.New("invalid receiver volume")
		}
		if db <= -30 {
			return 0, nil
		}
		return int(math.Round((db + 30) / 0.3)), nil
	}
	return 0, errors.New("receiver did not report its volume")
}

// ReadVolume never sends RECORD, audio packets or volume writes. Receivers may
// require a temporary SETUP to expose their volume; RAOP also requires ANNOUNCE.
func ReadVolume(ctx context.Context, device Device, id string, cred *Credentials) (int, error) {
	if device.Type == "AirPlay 1" {
		s, err := connectRAOP(ctx, device, id, false)
		if err != nil {
			return 0, err
		}
		defer s.Close()
		m, err := s.ctrl.request("GET_PARAMETER", "", "text/parameters", []byte("volume\r\n"), nil)
		if err != nil {
			return 0, err
		}
		return parseVolume(m.body)
	}

	if cred != nil {
		id = cred.ID
	}
	c, err := dial(device, id)
	if err != nil {
		return 0, err
	}
	defer c.conn.Close()
	stop := context.AfterFunc(ctx, func() { c.conn.Close() })
	defer stop()
	secret, err := authenticate(c, cred)
	if err != nil {
		return 0, err
	}
	c.encrypt(secret)
	m, err := c.request("GET_PARAMETER", "", "text/parameters", []byte("volume\r\n"), nil)
	if err == nil {
		if volume, parseErr := parseVolume(m.body); parseErr == nil {
			return volume, nil
		}
	}

	if volume, err := readInitialVolume(c); err == nil {
		return volume, nil
	}
	local := c.conn.LocalAddr().(*net.TCPAddr).IP
	clock, err := startClock(local, net.ParseIP(device.Address), 0, false)
	if err != nil {
		return 0, err
	}
	defer clock.close()
	uuid := fmt.Sprintf("%s-%s-%s-%s-%s", Identity()[:8], Identity()[:4], Identity()[:4], Identity()[:4], Identity()[:12])
	var mac []string
	for i := 0; i+2 <= len(id); i += 2 {
		mac = append(mac, id[i:i+2])
	}
	_, err = c.property("SETUP", "", map[string]any{"deviceID": strings.Join(mac, ":"), "sessionUUID": uuid, "name": "Harmonia", "timingProtocol": "NTP", "timingPort": clock.event.LocalAddr().(*net.UDPAddr).Port})
	if err != nil {
		return 0, err
	}
	defer c.request("TEARDOWN", "", "", nil, nil)
	if volume, err := readInitialVolume(c); err == nil {
		return volume, nil
	}
	m, err = c.request("GET_PARAMETER", "", "text/parameters", []byte("volume\r\n"), nil)
	if err != nil {
		return 0, err
	}
	return parseVolume(m.body)
}

func readInitialVolume(c *control) (int, error) {
	info, err := c.request("GET", "/info", "", nil, map[string]string{"X-Apple-ProtocolVersion": "1"})
	if err != nil {
		return 0, err
	}
	value, err := unplist(info.body)
	if err != nil {
		return 0, err
	}
	if fields, ok := value.(map[string]any); ok {
		var db float64
		switch v := fields["initialVolume"].(type) {
		case float64:
			db = v
		case uint64:
			db = float64(int64(v))
		default:
			return 0, errors.New("receiver does not expose volume while stopped")
		}
		return parseVolume([]byte("volume: " + strconv.FormatFloat(db, 'f', -1, 64)))
	}
	return 0, errors.New("receiver does not expose volume while stopped")
}
