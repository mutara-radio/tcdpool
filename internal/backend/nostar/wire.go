// Package nostar is the backend for the nostar tcd transcoder (branch
// tcd-nohw, software vocoders), with the tcdpool patches in
// patches/tcd-nohw. See docs/tcd-wire.md for the link format.
package nostar

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// ECodecType values of tcd (TCPacketDef.h).
const (
	CodecNone   uint8 = 0
	CodecDStar  uint8 = 1
	CodecDMR    uint8 = 2
	CodecC21600 uint8 = 3
	CodecC23200 uint8 = 4
	CodecP25    uint8 = 5
	CodecUSRP   uint8 = 6
	CodecPing   uint8 = 0xff
)

// Offsets in the 376-byte wire packet (measured: g++ 14, aarch64 Linux;
// the same on x86-64). Bytes 374 and 375 are padding.
const (
	PacketSize = 376

	offSequence = 0
	offModule   = 4
	offIsLast   = 5
	offStreamID = 6
	offCodecIn  = 8
	offDStar    = 9
	offDMR      = 18
	offM17      = 27
	offP25      = 43
	offUSRP     = 54
	endFields   = 374
)

// Packet is the Go form of the tcd STCPacket.
type Packet struct {
	Sequence uint32
	Module   byte
	IsLast   bool
	StreamID uint16
	CodecIn  uint8
	DStar    [9]byte
	DMR      [9]byte
	M17      [16]byte
	P25      [11]byte
	USRP     [160]int16
}

// IsWireCodec reports whether c is a codec_in value a peer may send.
func IsWireCodec(c uint8) bool {
	switch c {
	case CodecDStar, CodecDMR, CodecC21600, CodecC23200, CodecP25, CodecUSRP, CodecPing:
		return true
	}
	return false
}

// Encode writes p in the explicit wire encoding (little-endian, padding
// zero) to b, which must be at least PacketSize bytes.
func (p *Packet) Encode(b []byte) {
	_ = b[PacketSize-1]
	clear(b[:PacketSize])
	binary.LittleEndian.PutUint32(b[offSequence:], p.Sequence)
	b[offModule] = p.Module
	if p.IsLast {
		b[offIsLast] = 1
	}
	binary.LittleEndian.PutUint16(b[offStreamID:], p.StreamID)
	b[offCodecIn] = p.CodecIn
	copy(b[offDStar:], p.DStar[:])
	copy(b[offDMR:], p.DMR[:])
	copy(b[offM17:], p.M17[:])
	copy(b[offP25:], p.P25[:])
	for i, s := range p.USRP {
		binary.LittleEndian.PutUint16(b[offUSRP+2*i:], uint16(s))
	}
}

// ErrBadPacket is wrapped by Decode errors.
var ErrBadPacket = errors.New("bad packet")

// Decode reads a wire packet. It refuses a codec_in that is not
// defined and an is_last that is not 0 or 1 (for a ping only codec_in is
// checked, the other fields of a ping have no meaning).
func (p *Packet) Decode(b []byte) error {
	if len(b) < PacketSize {
		return fmt.Errorf("%w: %d bytes, want %d", ErrBadPacket, len(b), PacketSize)
	}
	c := b[offCodecIn]
	if !IsWireCodec(c) {
		return fmt.Errorf("%w: codec_in %d is out of range", ErrBadPacket, c)
	}
	if c != CodecPing && b[offIsLast] > 1 {
		return fmt.Errorf("%w: is_last is %d, not 0 or 1", ErrBadPacket, b[offIsLast])
	}
	*p = Packet{}
	p.Sequence = binary.LittleEndian.Uint32(b[offSequence:])
	p.Module = b[offModule]
	p.IsLast = b[offIsLast] == 1
	p.StreamID = binary.LittleEndian.Uint16(b[offStreamID:])
	p.CodecIn = c
	copy(p.DStar[:], b[offDStar:])
	copy(p.DMR[:], b[offDMR:])
	copy(p.M17[:], b[offM17:])
	copy(p.P25[:], b[offP25:])
	for i := range p.USRP {
		p.USRP[i] = int16(binary.LittleEndian.Uint16(b[offUSRP+2*i:]))
	}
	return nil
}

// v2 framing (patches/tcd-nohw/0008, TCFraming.h).
const (
	V2Version     = 1
	V2HeaderSize  = 8
	V2TrailerSize = 4
	V2MinPayload  = PacketSize
	V2MaxPayload  = 1024
	V2FrameSize   = V2HeaderSize + PacketSize + V2TrailerSize // 388
	V2MaxBadRun   = 8
	V2MaxGarbage  = 64 * 1024
)

var v2Magic = []byte("TCD2")

// Requested-outputs mask bits of tcd (bit n = codec_in value n). The m17
// field is codec2 3200; the c2_1600 bit also selects it.
const (
	OutDStar uint8 = 1 << CodecDStar
	OutDMR   uint8 = 1 << CodecDMR
	OutC1600 uint8 = 1 << CodecC21600
	OutM17   uint8 = 1 << CodecC23200
	OutP25   uint8 = 1 << CodecP25
	OutUSRP  uint8 = 1 << CodecUSRP
	OutAll         = OutDStar | OutDMR | OutM17 | OutP25 | OutUSRP
)

// NormalizeOutputs applies the rules of the patched tcd: the c2_1600
// bit selects m17, unknown bits are dropped, and no known bit means all.
func NormalizeOutputs(m uint8) uint8 {
	if m&OutC1600 != 0 {
		m |= OutM17
	}
	m &= OutAll
	if m == 0 {
		return OutAll
	}
	return m
}

// EncodeV2 writes p as one v2 frame to b (at least V2FrameSize bytes),
// with the requested-outputs byte outputs.
func (p *Packet) EncodeV2(b []byte, outputs uint8) {
	_ = b[V2FrameSize-1]
	copy(b, v2Magic)
	b[4] = V2Version
	b[5] = outputs
	binary.LittleEndian.PutUint16(b[6:], PacketSize)
	p.Encode(b[V2HeaderSize:])
	crc := crc32.ChecksumIEEE(b[4 : V2HeaderSize+PacketSize])
	binary.LittleEndian.PutUint32(b[V2HeaderSize+PacketSize:], crc)
}

// V2Status is the result of V2Reader.Next.
type V2Status int

const (
	V2NeedMore V2Status = iota
	V2Packet
	V2Bad
	V2TooManyBad
)

// V2Reader reassembles v2 frames from a byte stream and skips bad
// frames. It follows the same rules as CV2Reader in the patched tcd.
type V2Reader struct {
	buf       []byte
	inGarbage bool
	badRun    int
	badTotal  uint64
	skipped   int    // bytes skipped since the last good frame
	skipTotal uint64 // all bytes skipped
	outputs   uint8  // outputs byte of the last good frame
}

// Append adds bytes from the link.
func (r *V2Reader) Append(b []byte) { r.buf = append(r.buf, b...) }

// BadRun is the count of bad frames since the last good one.
func (r *V2Reader) BadRun() int { return r.badRun }

// BadTotal is the count of all bad frames.
func (r *V2Reader) BadTotal() uint64 { return r.badTotal }

// SkipTotal is the count of all skipped bytes.
func (r *V2Reader) SkipTotal() uint64 { return r.skipTotal }

func (r *V2Reader) drop(n int) {
	r.buf = r.buf[n:]
	if len(r.buf) == 0 {
		r.buf = r.buf[:0:0]
	}
}

func (r *V2Reader) skip(n int) {
	r.drop(n)
	r.skipped += n
	r.skipTotal += uint64(n)
}

func (r *V2Reader) bad(why string) (V2Status, string) {
	r.badRun++
	r.badTotal++
	r.inGarbage = true
	if r.badRun >= V2MaxBadRun {
		return V2TooManyBad, why
	}
	return V2Bad, why
}

// Refuse counts a well-formed frame whose content the caller refused.
func (r *V2Reader) Refuse(why string) (V2Status, string) { return r.bad(why) }

// Next takes the next frame out of the buffer and returns its
// outputs byte with it. For V2Bad and V2TooManyBad the string says what
// was wrong. Call it until V2NeedMore.
func (r *V2Reader) Next(p *Packet) (V2Status, uint8, string) {
	st, why := r.next(p)
	return st, r.outputs, why
}

func (r *V2Reader) next(p *Packet) (V2Status, string) {
	r.outputs = 0
	for {
		avail := len(r.buf)
		if avail < 4 {
			return V2NeedMore, ""
		}
		i := bytes.Index(r.buf, v2Magic)
		if i != 0 {
			found := i > 0
			n := i
			if !found {
				n = avail - 3 // keep a tail that can start a magic
			}
			r.skip(n)
			if r.skipped > V2MaxGarbage {
				return V2TooManyBad, "too many bytes outside a frame"
			}
			if !r.inGarbage {
				return r.bad("bytes outside a frame")
			}
			if !found {
				return V2NeedMore, ""
			}
			continue
		}
		if avail < V2HeaderSize {
			return V2NeedMore, ""
		}
		if v := r.buf[4]; v != V2Version {
			r.skip(1)
			return r.bad(fmt.Sprintf("unknown frame version %d", v))
		}
		plen := int(binary.LittleEndian.Uint16(r.buf[6:]))
		if plen < V2MinPayload || plen > V2MaxPayload {
			r.skip(1)
			return r.bad(fmt.Sprintf("frame length %d out of range", plen))
		}
		total := V2HeaderSize + plen + V2TrailerSize
		if avail < total {
			return V2NeedMore, ""
		}
		want := binary.LittleEndian.Uint32(r.buf[V2HeaderSize+plen:])
		if crc32.ChecksumIEEE(r.buf[4:V2HeaderSize+plen]) != want {
			r.skip(1)
			return r.bad("frame CRC mismatch")
		}
		err := p.Decode(r.buf[V2HeaderSize : V2HeaderSize+plen])
		r.outputs = r.buf[5]
		r.drop(total)
		if err != nil {
			return r.bad(err.Error())
		}
		r.inGarbage = false
		r.badRun = 0
		r.skipped = 0
		return V2Packet, ""
	}
}

// KeepOnly sets every codec field that is not in the outputs mask to
// zero, as the patched tcd does for a reply. outputs is normalized first.
func (p *Packet) KeepOnly(outputs uint8) {
	m := NormalizeOutputs(outputs)
	if m&OutDStar == 0 {
		p.DStar = [9]byte{}
	}
	if m&OutDMR == 0 {
		p.DMR = [9]byte{}
	}
	if m&OutM17 == 0 {
		p.M17 = [16]byte{}
	}
	if m&OutP25 == 0 {
		p.P25 = [11]byte{}
	}
	if m&OutUSRP == 0 {
		p.USRP = [160]int16{}
	}
}
