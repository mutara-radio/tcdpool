package protocol

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/dbehnke/tcdpool/internal/codec"
)

// The golden bytes are also in docs/protocol.md.
func TestRequestGolden(t *testing.T) {
	r := &Request{StreamID: 0x2101, Seq: 0x01020304, Last: true, In: codec.DMR,
		Outputs: codec.SetOf(codec.PCM), Payload: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9}}
	b := AppendRequest(nil, r)
	want := "0101" + "0121" + "04030201" + "01" + "03" + "02" + "00" + "010203040506070809"
	if hex.EncodeToString(b) != want {
		t.Fatalf("got  %x\nwant %s", b, want)
	}
	d, err := DecodeRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.StreamID != r.StreamID || d.Seq != r.Seq || !d.Last || d.In != r.In || d.Outputs != r.Outputs || !bytes.Equal(d.Payload, r.Payload) {
		t.Fatalf("decode %+v", d)
	}
}

func TestRequestErrors(t *testing.T) {
	good := AppendRequest(nil, &Request{In: codec.DStar, Payload: make([]byte, 9)})
	mod := func(i int, v byte) []byte { b := bytes.Clone(good); b[i] = v; return b }
	cases := map[string][]byte{
		"short":         good[:5],
		"version":       mod(0, 2),
		"type":          mod(1, TypeReply),
		"flags":         mod(8, 0x80),
		"reserved":      mod(11, 1),
		"codec 0":       mod(9, 0),
		"codec 9":       mod(9, 9),
		"mask bit 0":    mod(10, 0x01),
		"mask bit 7":    mod(10, 0x80),
		"payload short": good[:len(good)-1],
		"payload long":  append(bytes.Clone(good), 0),
	}
	for name, b := range cases {
		if _, err := DecodeRequest(b); !errors.Is(err, ErrBadFrame) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestReplyGolden(t *testing.T) {
	r := &Reply{StreamID: 0x2200, Seq: 7, Late: true, In: codec.PCM,
		Outputs: codec.SetOf(codec.DStar, codec.C2), LatencyMicros: 2500}
	r.Payloads[codec.DStar] = []byte{0xd1, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8, 0xd9}
	r.Payloads[codec.C2] = []byte{0xc1, 0xc2, 0xc3, 0xc4, 0xc5, 0xc6, 0xc7, 0xc8}
	b := AppendReply(nil, r)
	want := "0102" + "0022" + "07000000" + "02" + "01" + "24" + "00" + "c4090000" +
		"d1d2d3d4d5d6d7d8d9" + "c1c2c3c4c5c6c7c8"
	if hex.EncodeToString(b) != want {
		t.Fatalf("got  %x\nwant %s", b, want)
	}
	d, err := DecodeReply(b)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Late || d.Last || d.LatencyMicros != 2500 || !bytes.Equal(d.Payloads[codec.C2], r.Payloads[codec.C2]) || d.Payloads[codec.DMR] != nil {
		t.Fatalf("decode %+v", d)
	}
	if _, err := DecodeReply(b[:len(b)-1]); err == nil {
		t.Error("short reply accepted")
	}
	// PCM is 320 bytes, little-endian samples
	r2 := &Reply{Outputs: codec.SetOf(codec.PCM)}
	if n := len(AppendReply(nil, r2)); n != ReplyHeaderSize+320 {
		t.Errorf("PCM reply %d bytes", n)
	}
}
