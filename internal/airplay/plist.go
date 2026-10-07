package airplay

// The binary property-list subset used by AirPlay control messages. Decode is
// deliberately bounded: all values here come from an unauthenticated LAN peer.
import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"unicode/utf16"
)

func plist(v any) []byte {
	var objects [][]byte
	var add func(any) uint64
	add = func(v any) uint64 {
		var b []byte
		length := func(tag byte, n int) []byte {
			if n < 15 {
				return []byte{tag | byte(n)}
			}
			x := []byte{tag | 15, 0x13}
			return binary.BigEndian.AppendUint64(x, uint64(n))
		}
		switch x := v.(type) {
		case string:
			ascii := true
			for _, r := range x {
				if r > 127 {
					ascii = false
					break
				}
			}
			if ascii {
				b = append(length(0x50, len(x)), []byte(x)...)
			} else {
				u := utf16.Encode([]rune(x))
				b = length(0x60, len(u))
				for _, r := range u {
					b = binary.BigEndian.AppendUint16(b, r)
				}
			}
		case []byte:
			b = append(length(0x40, len(x)), x...)
		case bool:
			b = []byte{8}
			if x {
				b[0] = 9
			}
		case int:
			b = binary.BigEndian.AppendUint64([]byte{0x13}, uint64(x))
		case uint64:
			b = binary.BigEndian.AppendUint64([]byte{0x13}, x)
		case float64:
			b = binary.BigEndian.AppendUint64([]byte{0x23}, math.Float64bits(x))
		case []any:
			b = length(0xa0, len(x))
			for _, y := range x {
				b = binary.BigEndian.AppendUint64(b, add(y))
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b = length(0xd0, len(keys))
			for _, k := range keys {
				b = binary.BigEndian.AppendUint64(b, add(k))
			}
			for _, k := range keys {
				b = binary.BigEndian.AppendUint64(b, add(x[k]))
			}
		default:
			panic(fmt.Sprintf("unsupported internal plist type %T", v))
		}
		id := uint64(len(objects))
		objects = append(objects, b)
		return id
	}
	root := add(v)
	out := []byte("bplist00")
	offsets := make([]uint64, 0, len(objects))
	for _, b := range objects {
		offsets = append(offsets, uint64(len(out)))
		out = append(out, b...)
	}
	table := uint64(len(out))
	for _, off := range offsets {
		out = binary.BigEndian.AppendUint64(out, off)
	}
	out = append(out, 0, 0, 0, 0, 0, 0, 8, 8)
	out = binary.BigEndian.AppendUint64(out, uint64(len(objects)))
	out = binary.BigEndian.AppendUint64(out, root)
	out = binary.BigEndian.AppendUint64(out, table)
	return out
}

func unplist(b []byte) (any, error) {
	bad := errors.New("invalid or oversized AirPlay binary plist")
	if len(b) < 40 || len(b) > 2<<20 || string(b[:8]) != "bplist00" {
		return nil, bad
	}
	trailer := b[len(b)-32:]
	os, rs := int(trailer[6]), int(trailer[7])
	n := binary.BigEndian.Uint64(trailer[8:])
	root := binary.BigEndian.Uint64(trailer[16:])
	table := binary.BigEndian.Uint64(trailer[24:])
	if os < 1 || os > 8 || rs < 1 || rs > 8 || n == 0 || n > 65536 || root >= n || table < 8 || table > uint64(len(b)-32) || n*uint64(os) > uint64(len(b)-32)-table {
		return nil, bad
	}
	read := func(pos uint64, size int) (uint64, error) {
		if size < 1 || size > 8 || pos > uint64(len(b)) || uint64(size) > uint64(len(b))-pos {
			return 0, bad
		}
		var v uint64
		for _, c := range b[pos : pos+uint64(size)] {
			v = v<<8 | uint64(c)
		}
		return v, nil
	}
	budget := 65536
	var parse func(uint64, int) (any, error)
	parse = func(id uint64, depth int) (any, error) {
		budget--
		if id >= n || depth > 32 || budget < 0 {
			return nil, bad
		}
		p, e := read(table+id*uint64(os), os)
		if e != nil || p < 8 || p >= table {
			return nil, bad
		}
		tag := b[p]
		p++
		count := uint64(tag & 15)
		if tag>>4 >= 4 && count == 15 {
			if p >= table || b[p]>>4 != 1 || b[p]&15 > 3 {
				return nil, bad
			}
			size := 1 << uint(b[p]&15)
			p++
			count, e = read(p, size)
			p += uint64(size)
			if e != nil {
				return nil, e
			}
		}
		need := func(size uint64) bool { return p <= table && size <= table-p }
		switch tag >> 4 {
		case 0:
			if tag == 8 {
				return false, nil
			}
			if tag == 9 {
				return true, nil
			}
			return nil, bad
		case 1:
			if count > 3 || !need(1<<count) {
				return nil, bad
			}
			return read(p, 1<<count)
		case 2:
			if count != 3 || !need(8) {
				return nil, bad
			}
			v, _ := read(p, 8)
			return math.Float64frombits(v), nil
		case 4, 5, 6:
			width := uint64(1)
			if tag>>4 == 6 {
				width = 2
			}
			if count > uint64(len(b)) || !need(count*width) {
				return nil, bad
			}
			raw := b[p : p+count*width]
			if tag>>4 == 4 {
				return append([]byte(nil), raw...), nil
			}
			if width == 1 {
				return string(raw), nil
			}
			u := make([]uint16, count)
			for i := range u {
				u[i] = binary.BigEndian.Uint16(raw[i*2:])
			}
			return string(utf16.Decode(u)), nil
		case 10, 13:
			refs := count
			if tag>>4 == 13 {
				refs *= 2
			}
			if count > 65536 || !need(refs*uint64(rs)) {
				return nil, bad
			}
			child := func(i uint64) (any, error) {
				id, e := read(p+i*uint64(rs), rs)
				if e != nil {
					return nil, e
				}
				return parse(id, depth+1)
			}
			if tag>>4 == 10 {
				a := make([]any, count)
				for i := range a {
					a[i], e = child(uint64(i))
					if e != nil {
						return nil, e
					}
				}
				return a, nil
			}
			m := map[string]any{}
			for i := uint64(0); i < count; i++ {
				k, e := child(i)
				if e != nil {
					return nil, e
				}
				s, ok := k.(string)
				if !ok {
					return nil, bad
				}
				if _, exists := m[s]; exists {
					return nil, bad
				}
				m[s], e = child(i + count)
				if e != nil {
					return nil, e
				}
			}
			return m, nil
		}
		return nil, bad
	}
	return parse(root, 0)
}
