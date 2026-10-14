package nostar

import (
	"testing"

	"github.com/dbehnke/tcdpool/internal/backend"
	"github.com/dbehnke/tcdpool/internal/codec"
)

func TestMaskMapping(t *testing.T) {
	for _, id := range codec.All() {
		s := codec.SetOf(id)
		if got := SetOfMask(MaskOf(s)); got != s {
			t.Errorf("%v: round trip %v", id, got)
		}
	}
	if MaskOf(codec.SetOf(codec.All()...)) != OutAll {
		t.Error("all codecs != OutAll")
	}
	if MaskOf(0) != 0 {
		t.Error("empty set must give 0 (all)")
	}
}

func TestToFromPacket(t *testing.T) {
	f := &backend.Frame{StreamID: 0x2100, Seq: 7, In: codec.C2, Want: codec.SetOf(codec.DMR)}
	copy(f.C2[:], []byte{1, 2, 3, 4, 5, 6, 7, 8})
	p, m, ok := ToPacket(f, 'A')
	if !ok || m != OutDMR || p.CodecIn != CodecC23200 || p.Module != 'A' {
		t.Fatalf("ToPacket: %+v %#x %v", p, m, ok)
	}
	// odd sequence: second half of the m17 field
	if p.M17[8] != 1 || p.M17[0] != 0 {
		t.Errorf("m17 half: %v", p.M17)
	}
	p.DMR = [9]byte{9, 9, 9}
	r := FromPacket(&p, OutDMR|OutM17)
	if r.Have != codec.SetOf(codec.DMR, codec.C2) || r.C2[0] != 1 || r.DMR[0] != 9 || r.In != codec.C2 {
		t.Errorf("FromPacket: %+v", r)
	}
	if FromPacket(&p, 0).Have != codec.SetOf(codec.All()...) {
		t.Error("raw reply must have all codecs")
	}
	if _, _, ok := ToPacket(&backend.Frame{In: codec.None}, 'A'); ok {
		t.Error("codec None accepted")
	}
}
