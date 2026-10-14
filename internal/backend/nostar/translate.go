package nostar

import (
	"github.com/dbehnke/tcdpool/internal/backend"
	"github.com/dbehnke/tcdpool/internal/codec"
)

// codecIn maps a tcdpool input codec to tcd's codec_in.
func codecIn(id codec.ID) (uint8, bool) {
	switch id {
	case codec.PCM:
		return CodecUSRP, true
	case codec.DStar:
		return CodecDStar, true
	case codec.DMR:
		return CodecDMR, true
	case codec.IMBE:
		return CodecP25, true
	case codec.C2:
		return CodecC23200, true
	}
	return 0, false
}

// codecFromWire maps tcd's codec_in back to a tcdpool codec.
func codecFromWire(c uint8) codec.ID {
	switch c {
	case CodecUSRP:
		return codec.PCM
	case CodecDStar:
		return codec.DStar
	case CodecDMR:
		return codec.DMR
	case CodecP25:
		return codec.IMBE
	case CodecC23200, CodecC21600:
		return codec.C2
	}
	return codec.None
}

// outBits pairs each tcdpool codec with its tcd output bit.
var outBits = []struct {
	id  codec.ID
	bit uint8
}{
	{codec.PCM, OutUSRP},
	{codec.DStar, OutDStar},
	{codec.DMR, OutDMR},
	{codec.IMBE, OutP25},
	{codec.C2, OutM17},
}

// MaskOf converts a tcdpool codec set to a tcd outputs mask. An empty
// set gives 0, which tcd reads as all codecs.
func MaskOf(s codec.Set) uint8 {
	var m uint8
	for _, b := range outBits {
		if s.Has(b.id) {
			m |= b.bit
		}
	}
	return m
}

// SetOfMask converts a tcd outputs mask to a tcdpool codec set.
func SetOfMask(m uint8) codec.Set {
	var s codec.Set
	for _, b := range outBits {
		if m&b.bit != 0 {
			s = s.Add(b.id)
		}
	}
	return s
}

// m17Half returns the half of the 16-byte M17 field that holds the
// Codec2 3200 frame for a sequence number. tcd pairs two 20 ms frames in
// one M17 payload: an even sequence uses bytes 0..7, an odd one 8..15.
func m17Half(seq uint32) int {
	if seq%2 == 1 {
		return 8
	}
	return 0
}

// ToPacket builds the tcd packet and the outputs mask for a client
// frame.
func ToPacket(f *backend.Frame, module byte) (Packet, uint8, bool) {
	c, ok := codecIn(f.In)
	if !ok {
		return Packet{}, 0, false
	}
	p := Packet{
		Sequence: f.Seq,
		Module:   module,
		IsLast:   f.Last,
		StreamID: f.StreamID,
		CodecIn:  c,
	}
	switch f.In {
	case codec.PCM:
		p.USRP = f.PCM
	case codec.DStar:
		p.DStar = f.DStar
	case codec.DMR:
		p.DMR = f.DMR
	case codec.IMBE:
		p.P25 = f.IMBE
	case codec.C2:
		h := m17Half(f.Seq)
		copy(p.M17[h:h+8], f.C2[:])
	}
	return p, MaskOf(f.Want), true
}

// FromPacket builds the reply frame from a tcd reply. outputs is the
// outputs byte of the v2 frame; 0 (raw framing) means every field holds
// data.
func FromPacket(p *Packet, outputs uint8) *backend.Frame {
	f := &backend.Frame{
		StreamID: p.StreamID,
		Seq:      p.Sequence,
		Last:     p.IsLast,
		In:       codecFromWire(p.CodecIn),
		Have:     SetOfMask(NormalizeOutputs(outputs)),
		PCM:      p.USRP,
		DStar:    p.DStar,
		DMR:      p.DMR,
		IMBE:     p.P25,
	}
	h := m17Half(p.Sequence)
	copy(f.C2[:], p.M17[h:h+8])
	return f
}
