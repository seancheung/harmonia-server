package airplay

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Device struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Address           string `json:"address"`
	Port              int    `json:"port"`
	Features          uint64 `json:"-"`
	RequiresAuth      bool   `json:"requires_auth"`
	Selected          bool   `json:"selected"`
	Type              string `json:"type"`
	RSA               bool   `json:"-"`
	UnsupportedReason string `json:"unsupported_reason,omitempty"`
}

func featureBits(v string) uint64 {
	parts := strings.Split(v, ",")
	var result uint64
	for i, s := range parts {
		if i > 1 {
			break
		}
		n, _ := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(s), "0x"), 16, 32)
		result |= n << uint(i*32)
	}
	return result
}
func dnsName(b []byte, p int) (string, int, error) {
	var labels []string
	next := -1
	for steps := 0; steps < 128; steps++ {
		if p < 0 || p >= len(b) {
			break
		}
		n := int(b[p])
		p++
		if n == 0 {
			if next < 0 {
				next = p
			}
			return strings.Join(labels, ".") + ".", next, nil
		}
		if n&192 == 192 {
			if p >= len(b) {
				break
			}
			if next < 0 {
				next = p + 1
			}
			p = (n&63)<<8 | int(b[p])
			continue
		}
		if n > 63 || p+n > len(b) {
			break
		}
		labels = append(labels, string(b[p:p+n]))
		p += n
	}
	return "", 0, errors.New("invalid DNS name")
}
func query(name string, typ uint16) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[4:], 1)
	for _, s := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if len(s) > 63 {
			return nil
		}
		b = append(b, byte(len(s)))
		b = append(b, s...)
	}
	b = append(b, 0)
	b = binary.BigEndian.AppendUint16(b, typ)
	return binary.BigEndian.AppendUint16(b, 0x8001)
}

type discoveredService struct {
	name, host string
	port       int
	txt        map[string]string
}

type dnsRecord struct {
	name   string
	typ    uint16
	ttl    uint32
	data   []byte
	target string
	port   int
}

func dnsRecords(b []byte) ([]dnsRecord, error) {
	if len(b) < 12 {
		return nil, errors.New("short DNS packet")
	}
	p := 12
	questions := int(binary.BigEndian.Uint16(b[4:]))
	count := int(binary.BigEndian.Uint16(b[6:])) + int(binary.BigEndian.Uint16(b[8:])) + int(binary.BigEndian.Uint16(b[10:]))
	if questions > 256 || count > 1024 {
		return nil, errors.New("oversized DNS packet")
	}
	for i := 0; i < questions; i++ {
		_, end, e := dnsName(b, p)
		if e != nil || end+4 > len(b) {
			return nil, errors.New("invalid DNS question")
		}
		p = end + 4
	}
	out := []dnsRecord{}
	for i := 0; i < count; i++ {
		name, end, e := dnsName(b, p)
		if e != nil || end+10 > len(b) {
			return nil, errors.New("invalid DNS record")
		}
		typ := binary.BigEndian.Uint16(b[end:])
		ttl := binary.BigEndian.Uint32(b[end+4:])
		n := int(binary.BigEndian.Uint16(b[end+8:]))
		p = end + 10
		if p+n > len(b) {
			return nil, errors.New("truncated DNS record")
		}
		r := dnsRecord{name: strings.ToLower(name), typ: typ, ttl: ttl, data: append([]byte{}, b[p:p+n]...)}
		if typ == 12 {
			r.target, _, e = dnsName(b, p)
		}
		if typ == 33 {
			if n < 7 {
				return nil, errors.New("short SRV")
			}
			r.port = int(binary.BigEndian.Uint16(b[p+4:]))
			r.target, _, e = dnsName(b, p+6)
		}
		if e != nil {
			return nil, e
		}
		out = append(out, r)
		p += n
	}
	return out, nil
}

// Legacy-unicast mDNS queries use an ephemeral source port so discovery does
// not compete with the operating system's Bonjour/Avahi listener on UDP 5353.
func Discover(ctx context.Context) ([]Device, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	type result struct {
		devices []Device
		err     error
	}
	results := make(chan result, len(interfaces))
	count := 0
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 || iface.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		hasIPv4 := false
		for _, address := range addresses {
			if ip, ok := address.(*net.IPNet); ok && ip.IP.To4() != nil {
				hasIPv4 = true
				break
			}
		}
		if !hasIPv4 {
			continue
		}
		count++
		go func(iface net.Interface) {
			devices, err := discoverInterface(ctx, &iface)
			if err != nil {
				err = fmt.Errorf("%s: %w", iface.Name, err)
			}
			results <- result{devices, err}
		}(iface)
	}
	if count == 0 {
		return nil, errors.New("no active multicast IPv4 LAN interface")
	}
	out := []Device{}
	seen := map[string]int{}
	var failures []error
	succeeded := false
	for i := 0; i < count; i++ {
		r := <-results
		if r.err != nil {
			failures = append(failures, r.err)
			continue
		}
		succeeded = true
		for _, d := range r.devices {
			if index, ok := seen[d.ID]; ok {
				out[index] = preferDevice(out[index], d)
			} else {
				seen[d.ID] = len(out)
				out = append(out, d)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !succeeded {
		return nil, errors.Join(failures...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func discoverInterface(ctx context.Context, iface *net.Interface) ([]Device, error) {
	// Explicitly select the outgoing interface: a VPN can own the default
	// multicast route even when receivers are reachable on the local network.
	conn, e := net.ListenMulticastUDP("udp4", iface, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251)})
	if e != nil {
		return nil, e
	}
	defer conn.Close()
	// ListenMulticastUDP disables loopback by default, hiding receivers on this host.
	if e = enableMulticastLoopback(conn); e != nil {
		return nil, e
	}
	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	asked := map[string]bool{}
	var sendError error
	ask := func(name string, typ uint16) {
		key := name + strconv.Itoa(int(typ))
		if !asked[key] {
			asked[key] = true
			if _, err := conn.WriteToUDP(query(name, typ), group); err != nil {
				sendError = err
			}
		}
	}
	ask("_airplay._tcp.local.", 12)
	ask("_raop._tcp.local.", 12)
	if sendError != nil {
		return nil, sendError
	}
	services := map[string]*discoveredService{}
	hosts := map[string]string{}
	deadline := time.Now().Add(1800 * time.Millisecond)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	buffer := make([]byte, 65535)
	for time.Now().Before(deadline) {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		_ = conn.SetReadDeadline(minTime(deadline, time.Now().Add(150*time.Millisecond)))
		n, _, e := conn.ReadFromUDP(buffer)
		if e != nil {
			if ne, ok := e.(net.Error); ok && ne.Timeout() {
				continue
			}
			return nil, e
		}
		records, e := dnsRecords(buffer[:n])
		if e != nil {
			continue
		}
		for _, r := range records {
			if r.ttl == 0 {
				continue
			}
			switch r.typ {
			case 12:
				if r.name == "_airplay._tcp.local." || r.name == "_raop._tcp.local." {
					key := strings.ToLower(r.target)
					if services[key] == nil {
						services[key] = &discoveredService{name: strings.TrimSuffix(r.target, "."+r.name), txt: map[string]string{}}
					}
					ask(r.target, 33)
					ask(r.target, 16)
				}
			case 33, 16:
				if !strings.HasSuffix(r.name, "._airplay._tcp.local.") && !strings.HasSuffix(r.name, "._raop._tcp.local.") {
					continue
				}
				s := services[r.name]
				if s == nil {
					s = &discoveredService{name: strings.TrimSuffix(r.name, "._airplay._tcp.local."), txt: map[string]string{}}
					services[r.name] = s
				}
				if r.typ == 33 {
					s.host = strings.ToLower(r.target)
					s.port = r.port
					ask(r.target, 1)
				} else {
					data := r.data
					for len(data) > 0 {
						n := int(data[0])
						data = data[1:]
						if n > len(data) {
							break
						}
						k, v, _ := strings.Cut(string(data[:n]), "=")
						s.txt[strings.ToLower(k)] = v
						data = data[n:]
					}
				}
			case 1:
				if len(r.data) == 4 {
					hosts[r.name] = net.IP(r.data).String()
				}
			}
		}
	}
	return devicesFromServices(services, hosts), nil
}

func csvHas(value, want string) bool {
	for _, item := range strings.Split(value, ",") {
		if strings.TrimSpace(item) == want {
			return true
		}
	}
	return false
}
func deviceID(value string) string {
	compact := strings.ToLower(strings.ReplaceAll(value, ":", ""))
	if len(compact) != 12 {
		return strings.ToLower(value)
	}
	if _, err := strconv.ParseUint(compact, 16, 64); err != nil {
		return strings.ToLower(value)
	}
	var parts []string
	for i := 0; i < 12; i += 2 {
		parts = append(parts, compact[i:i+2])
	}
	return strings.Join(parts, ":")
}
func preferDevice(a, b Device) Device {
	if (b.Type == "AirPlay 2" && a.Type != "AirPlay 2") || (a.UnsupportedReason != "" && b.UnsupportedReason == "") {
		return b
	}
	return a
}
func devicesFromServices(services map[string]*discoveredService, hosts map[string]string) []Device {
	merged := map[string]Device{}
	for key, s := range services {
		address := hosts[s.host]
		if address == "" || s.port == 0 {
			continue
		}
		features := featureBits(s.txt["features"])
		if features == 0 {
			features = featureBits(s.txt["ft"])
		}
		raop := strings.HasSuffix(key, "._raop._tcp.local.")
		name := strings.TrimSuffix(s.name, "._raop._tcp.local.")
		id := s.txt["deviceid"]
		if raop {
			if prefix, suffix, ok := strings.Cut(name, "@"); ok {
				name = suffix
				if id == "" {
					id = prefix
				}
			}
		}
		if id == "" {
			id = key
		}
		id = deviceID(id)
		d := Device{ID: id, Name: name, Address: address, Port: s.port, Features: features, Type: "AirPlay 2"}
		if raop {
			d.Type = "AirPlay 1"
			switch {
			case s.txt["cn"] != "" && !csvHas(s.txt["cn"], "1"):
				d.UnsupportedReason = "Receiver does not support ALAC audio"
			case s.txt["tp"] != "" && !csvHas(strings.ToUpper(s.txt["tp"]), "UDP"):
				d.UnsupportedReason = "Receiver does not support UDP audio"
			case strings.EqualFold(s.txt["pw"], "true"):
				d.UnsupportedReason = "Password-protected AirPlay 1 receivers are not supported yet"
			case csvHas(s.txt["et"], "1"):
				d.RSA = true
			case s.txt["et"] != "" && !csvHas(s.txt["et"], "0"):
				d.UnsupportedReason = "Receiver requires unsupported FairPlay authentication"
			}
		} else if features&(1<<48|1<<38) == 0 {
			d.Type = "AirPlay"
			d.UnsupportedReason = "Receiver does not advertise a supported audio transport"
		} else {
			flags := featureBits(s.txt["flags"])
			if flags == 0 {
				flags = featureBits(s.txt["sf"])
			}
			d.RequiresAuth = flags&8 != 0 || strings.EqualFold(s.txt["pw"], "true")
		}
		if old, ok := merged[id]; ok {
			d = preferDevice(old, d)
		}
		merged[id] = d
	}
	out := make([]Device, 0, len(merged))
	for _, d := range merged {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
