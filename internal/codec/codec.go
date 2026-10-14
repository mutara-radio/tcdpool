// Package codec names the voice codecs that tcdpool carries and the
// size of one 20 ms frame of each.
package codec

import (
	"fmt"
	"strings"
)

// ID is a tcdpool codec number. These numbers are part of the client
// wire protocol (docs/protocol.md). They do not depend on any backend.
type ID uint8

const (
	None  ID = 0
	PCM   ID = 1 // 160 samples, 8 kHz, signed 16-bit, little-endian (320 bytes)
	DStar ID = 2 // D-Star AMBE+ (AMBE 2400 with FEC), 9 bytes
	DMR   ID = 3 // AMBE+2 (DMR, YSF DN, NXDN), 72 bits with FEC, 9 bytes
	IMBE  ID = 4 // P25 phase 1 IMBE, 88 bits, 11 bytes
	C2    ID = 5 // Codec2 3200, one 20 ms frame, 8 bytes

	maxID = C2
)

// FrameSamples is the PCM sample count of one 20 ms frame at 8 kHz.
const FrameSamples = 160

var names = map[ID]string{
	PCM:   "pcm",
	DStar: "dstar",
	DMR:   "dmr",
	IMBE:  "imbe",
	C2:    "codec2_3200",
}

var sizes = map[ID]int{
	PCM:   FrameSamples * 2,
	DStar: 9,
	DMR:   9,
	IMBE:  11,
	C2:    8,
}

// All returns every known codec in ID order.
func All() []ID { return []ID{PCM, DStar, DMR, IMBE, C2} }

// Valid reports whether id is a known codec.
func (id ID) Valid() bool { return id >= PCM && id <= maxID }

// Size returns the payload size of one frame, or 0 for an unknown codec.
func (id ID) Size() int { return sizes[id] }

func (id ID) String() string {
	if n, ok := names[id]; ok {
		return n
	}
	return fmt.Sprintf("codec(%d)", uint8(id))
}

// Parse converts a codec name to its ID. It accepts a few aliases.
func Parse(s string) (ID, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pcm", "usrp":
		return PCM, nil
	case "dstar", "ambe", "ambe+":
		return DStar, nil
	case "dmr", "ambe2", "ambe+2", "ysf":
		return DMR, nil
	case "imbe", "p25":
		return IMBE, nil
	case "codec2_3200", "codec2", "c2", "c2_3200", "m17":
		return C2, nil
	}
	return None, fmt.Errorf("unknown codec %q", s)
}

// Set is a bit set of codecs: bit n is codec ID n.
type Set uint8

// SetOf builds a set.
func SetOf(ids ...ID) Set {
	var s Set
	for _, id := range ids {
		s |= 1 << id
	}
	return s
}

// Has reports whether id is in the set.
func (s Set) Has(id ID) bool { return id.Valid() && s&(1<<id) != 0 }

// Add returns the set with id added.
func (s Set) Add(id ID) Set { return s | 1<<id }

// Without returns the set without id.
func (s Set) Without(id ID) Set { return s &^ (1 << id) }

// IDs returns the members in ID order.
func (s Set) IDs() []ID {
	var out []ID
	for _, id := range All() {
		if s.Has(id) {
			out = append(out, id)
		}
	}
	return out
}

// Names returns the member names in ID order.
func (s Set) Names() []string {
	var out []string
	for _, id := range s.IDs() {
		out = append(out, id.String())
	}
	return out
}

// Valid reports whether the set holds only known codecs.
func (s Set) Valid() bool { return s&^SetOf(All()...) == 0 }

// ParseSet converts names to a set.
func ParseSet(names []string) (Set, error) {
	var s Set
	for _, n := range names {
		id, err := Parse(n)
		if err != nil {
			return 0, err
		}
		s = s.Add(id)
	}
	return s, nil
}

// PayloadSize returns the total size of the payloads for the set.
func (s Set) PayloadSize() int {
	n := 0
	for _, id := range s.IDs() {
		n += id.Size()
	}
	return n
}
