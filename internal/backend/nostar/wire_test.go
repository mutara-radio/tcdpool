package nostar

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"hash/crc32"
	"os"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

// vector is one line of testdata/tcd-wire-vectors.txt, which is made by
// tests/tcd-wire/genvectors.cpp from the C struct memory of tcd.
type vector map[string]string

func readVectors(t *testing.T) []vector {
	t.Helper()
	f, err := os.Open("../../../testdata/tcd-wire-vectors.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []vector
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		v := vector{}
		for _, kv := range strings.Fields(line) {
			k, val, _ := strings.Cut(kv, "=")
			v[k] = val
		}
		out = append(out, v)
	}
	if len(out) < 4 {
		t.Fatalf("only %d vectors", len(out))
	}
	return out
}

func (v vector) bytes(t *testing.T, k string) []byte {
	t.Helper()
	b, err := hex.DecodeString(v[k])
	if err != nil {
		t.Fatalf("%s: %s: %v", v["name"], k, err)
	}
	return b
}

func (v vector) num(t *testing.T, k string) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(v[k], 0, 64)
	if err != nil {
		t.Fatalf("%s: %s: %v", v["name"], k, err)
	}
	return n
}

func (v vector) packet(t *testing.T) Packet {
	p := Packet{
		Sequence: uint32(v.num(t, "seq")),
		Module:   v["module"][0],
		IsLast:   v["last"] == "1",
		StreamID: uint16(v.num(t, "sid")),
		CodecIn:  uint8(v.num(t, "codec")),
	}
	copy(p.DStar[:], v.bytes(t, "dstar"))
	copy(p.DMR[:], v.bytes(t, "dmr"))
	copy(p.M17[:], v.bytes(t, "m17"))
	copy(p.P25[:], v.bytes(t, "p25"))
	for i, s := range strings.Split(v["usrp"], ",") {
		n, err := strconv.ParseInt(s, 10, 16)
		if err != nil {
			t.Fatal(err)
		}
		p.USRP[i] = int16(n)
	}
	return p
}

// TestLayout checks the Go mirror of the C struct against the offsets
// measured with g++ (offsetof) on aarch64 and given in docs/tcd-wire.md.
func TestLayout(t *testing.T) {
	want := []struct {
		name      string
		off, size int
	}{
		{"sequence", 0, 4}, {"module", 4, 1}, {"is_last", 5, 1}, {"streamid", 6, 2},
		{"codec_in", 8, 1}, {"dstar", 9, 9}, {"dmr", 18, 9}, {"m17", 27, 16},
		{"p25", 43, 11}, {"usrp", 54, 320},
	}
	got := []int{offSequence, offModule, offIsLast, offStreamID, offCodecIn, offDStar, offDMR, offM17, offP25, offUSRP}
	for i, w := range want {
		if got[i] != w.off {
			t.Errorf("%s at %d, want %d", w.name, got[i], w.off)
		}
	}
	if offUSRP+320 != endFields || PacketSize != 376 {
		t.Errorf("end %d size %d", endFields, PacketSize)
	}
	// The Go struct is not the wire format, but a C-like struct with
	// the same fields must have the same size; this guards the table.
	type cLike struct {
		seq    uint32
		module byte
		isLast bool
		sid    uint16
		codec  uint8
		dstar  [9]byte
		dmr    [9]byte
		m17    [16]byte
		p25    [11]byte
		usrp   [160]int16
	}
	var c cLike
	if s := unsafe.Sizeof(c); s != PacketSize {
		t.Errorf("C-like struct size %d", s)
	}
	if o := unsafe.Offsetof(c.usrp); o != offUSRP {
		t.Errorf("C-like usrp offset %d", o)
	}
}

func TestGoldenVectors(t *testing.T) {
	for _, v := range readVectors(t) {
		name := v["name"]
		p := v.packet(t)
		raw := v.bytes(t, "raw")
		var enc [PacketSize]byte
		p.Encode(enc[:])
		if !bytes.Equal(enc[:], raw) {
			t.Errorf("%s: Encode differs from golden raw", name)
		}
		var d Packet
		if err := d.Decode(raw); err != nil {
			t.Errorf("%s: Decode: %v", name, err)
		} else if p.CodecIn != CodecPing && d != p {
			t.Errorf("%s: Decode(golden) fields differ", name)
		}
		// round trip: decode then encode gives the same bytes
		var re [PacketSize]byte
		d.Encode(re[:])
		if p.CodecIn != CodecPing && !bytes.Equal(re[:], raw) {
			t.Errorf("%s: round trip differs", name)
		}

		outputs := uint8(v.num(t, "outputs"))
		var f [V2FrameSize]byte
		p.EncodeV2(f[:], outputs)
		if !bytes.Equal(f[:], v.bytes(t, "v2")) {
			t.Errorf("%s: EncodeV2 differs from golden v2", name)
		}
		if p.CodecIn == CodecPing {
			continue
		}
		q := p
		q.KeepOnly(outputs)
		q.Encode(enc[:])
		if !bytes.Equal(enc[:], v.bytes(t, "reply")) {
			t.Errorf("%s: KeepOnly reply differs from golden reply", name)
		}
		if got, want := NormalizeOutputs(outputs), uint8(v.num(t, "reply_outputs")); got != want {
			t.Errorf("%s: reply outputs %#x, want %#x", name, got, want)
		}
	}
}

func TestDecodeRefuses(t *testing.T) {
	v := readVectors(t)[0]
	raw := v.bytes(t, "raw")
	var d Packet
	raw[5] = 2
	if d.Decode(raw) == nil {
		t.Error("is_last=2 accepted")
	}
	raw[5] = 0
	for _, c := range []byte{0, 7, 0x80} {
		raw[8] = c
		if d.Decode(raw) == nil {
			t.Errorf("codec_in=%d accepted", c)
		}
	}
	raw[8] = CodecUSRP
	raw[374], raw[375] = 0xAA, 0x55
	if err := d.Decode(raw); err != nil {
		t.Errorf("padding not ignored: %v", err)
	}
	if d.Decode(raw[:100]) == nil {
		t.Error("short packet accepted")
	}
}

type feed struct {
	packets []Packet
	outputs []uint8
	bad     int
	tooMany bool
}

func feedChunks(b []byte, chunk int) feed {
	var r V2Reader
	var out feed
	for i := 0; i < len(b) && !out.tooMany; i += chunk {
		r.Append(b[i:min(i+chunk, len(b))])
		for {
			var p Packet
			st, o, _ := r.Next(&p)
			if st == V2NeedMore {
				break
			}
			if st == V2Packet {
				out.packets = append(out.packets, p)
				out.outputs = append(out.outputs, o)
				continue
			}
			out.bad++
			if st == V2TooManyBad {
				out.tooMany = true
				break
			}
		}
	}
	return out
}

func TestV2ReaderSplit(t *testing.T) {
	vs := readVectors(t)
	var stream []byte
	for _, v := range vs {
		stream = append(stream, v.bytes(t, "v2")...)
	}
	for _, chunk := range []int{4096, 1, 3, 7, 100, 387} {
		f := feedChunks(stream, chunk)
		if len(f.packets) != len(vs) || f.bad != 0 {
			t.Errorf("chunk %d: %d packets, %d bad", chunk, len(f.packets), f.bad)
			continue
		}
		for i, v := range vs {
			if want := uint8(v.num(t, "outputs")); f.outputs[i] != want {
				t.Errorf("chunk %d: packet %d outputs %#x, want %#x", chunk, i, f.outputs[i], want)
			}
		}
	}
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestV2ReaderBad(t *testing.T) {
	good := readVectors(t)[0].bytes(t, "v2")
	flip := func(i int, x byte) []byte { b := bytes.Clone(good); b[i] ^= x; return b }

	if f := feedChunks(cat(good, flip(100, 1), good), 64); len(f.packets) != 2 || f.bad != 1 || f.tooMany {
		t.Errorf("bad CRC: %+v", f.bad)
	}
	junk := []byte{0x00, 0x11, 'T', 'C', 'D', 0x99, 0x42, 0x13}
	if f := feedChunks(cat(good, junk, good, junk, junk, good), 5); len(f.packets) != 3 || f.bad != 2 {
		t.Errorf("garbage: %d packets %d bad", len(f.packets), f.bad)
	}
	ver := bytes.Clone(good)
	ver[4] = 2
	if f := feedChunks(cat(ver, good), 4096); len(f.packets) != 1 || f.bad != 1 {
		t.Errorf("version: %d packets %d bad", len(f.packets), f.bad)
	}
	ln := bytes.Clone(good)
	ln[6], ln[7] = 0x10, 0
	if f := feedChunks(cat(ln, good), 4096); len(f.packets) != 1 || f.bad != 1 {
		t.Errorf("length: %d packets %d bad", len(f.packets), f.bad)
	}
	// a longer payload is accepted
	long := append(bytes.Clone(good[:V2HeaderSize+PacketSize]), 1, 2, 3, 4)
	long[6], long[7] = byte((PacketSize+4)&0xff), byte((PacketSize+4)>>8)
	c := crc32.ChecksumIEEE(long[4:])
	long = append(long, byte(c), byte(c>>8), byte(c>>16), byte(c>>24))
	if f := feedChunks(long, 4096); len(f.packets) != 1 || f.bad != 0 {
		t.Errorf("longer payload: %d packets %d bad", len(f.packets), f.bad)
	}
	var s []byte
	for range V2MaxBadRun {
		s = append(s, flip(200, 0xff)...)
	}
	if f := feedChunks(cat(s, good), 4096); !f.tooMany || len(f.packets) != 0 {
		t.Errorf("N bad: tooMany=%v packets=%d", f.tooMany, len(f.packets))
	}
	s = nil
	for range V2MaxBadRun - 1 {
		s = append(s, flip(200, 0xff)...)
	}
	if f := feedChunks(cat(s, good), 4096); f.tooMany || len(f.packets) != 1 {
		t.Errorf("N-1 bad then good: tooMany=%v packets=%d", f.tooMany, len(f.packets))
	}
	if crc32.ChecksumIEEE([]byte("123456789")) != 0xCBF43926 {
		t.Error("CRC check value")
	}
}

func TestOutputMask(t *testing.T) {
	if OutAll != 0x76 {
		t.Errorf("OutAll %#x", OutAll)
	}
	cases := map[uint8]uint8{0: 0x76, 0x01: 0x76, 0xff: 0x76, OutC1600: OutM17, OutDMR: OutDMR}
	for in, want := range cases {
		if got := NormalizeOutputs(in); got != want {
			t.Errorf("NormalizeOutputs(%#x) = %#x, want %#x", in, got, want)
		}
	}
}
