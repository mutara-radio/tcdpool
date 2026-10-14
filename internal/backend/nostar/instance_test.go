package nostar_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/dbehnke/tcdpool/internal/backend"
	"github.com/dbehnke/tcdpool/internal/backend/nostar"
	"github.com/dbehnke/tcdpool/internal/codec"
	"github.com/dbehnke/tcdpool/internal/faketcd"
)

// The test binary runs as the fake tcd when the env var is set.
func TestMain(m *testing.M) {
	if os.Getenv("TCDPOOL_FAKETCD") == "1" {
		os.Exit(faketcd.Main(os.Args[len(os.Args)-1:]))
	}
	os.Exit(m.Run())
}

func newBackend(t *testing.T, framing string, env ...string) *nostar.Backend {
	t.Helper()
	b, err := nostar.New(nostar.Options{
		Binary:         os.Args[0],
		WorkDir:        t.TempDir(),
		Framing:        framing,
		ConnectTimeout: 5 * time.Second,
		Watchdog:       2 * time.Second,
		Env:            append([]string{"TCDPOOL_FAKETCD=1", "TCDPOOL_FAKE_PING_MS=200"}, env...),
		Logf:           t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func start(t *testing.T, b *nostar.Backend) backend.Instance {
	t.Helper()
	in, err := b.Start(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.Stop)
	return in
}

func pcm(seq uint32, want codec.Set) *backend.Frame {
	f := &backend.Frame{StreamID: 0x2100, Seq: seq, In: codec.PCM, Want: want}
	for i := range f.PCM {
		f.PCM[i] = int16(i * 10)
	}
	return f
}

func cmdFrame(seq uint32, cmd, arg int) *backend.Frame {
	f := pcm(seq, 0)
	f.PCM[0], f.PCM[1], f.PCM[2] = faketcd.CmdMagic, int16(cmd), int16(arg)
	return f
}

func TestRoundTrip(t *testing.T) {
	for _, framing := range []string{"v2", "raw"} {
		t.Run(framing, func(t *testing.T) {
			in := start(t, newBackend(t, framing))
			want := codec.SetOf(codec.DMR)
			if err := in.Send(pcm(1, want)); err != nil {
				t.Fatal(err)
			}
			r, err := in.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if r.Seq != 1 || r.StreamID != 0x2100 || r.In != codec.PCM {
				t.Fatalf("reply %+v", r)
			}
			if framing == "v2" && r.Have != want {
				t.Errorf("v2 Have %v, want %v", r.Have.Names(), want.Names())
			}
			if framing == "raw" && r.Have != codec.SetOf(codec.All()...) {
				t.Errorf("raw Have %v", r.Have.Names())
			}
			if r.DMR == [9]byte{} {
				t.Error("no DMR data")
			}
			// pings are counted and not returned
			time.Sleep(500 * time.Millisecond)
			in.Send(pcm(2, want))
			if r, err := in.Recv(); err != nil || r.Seq != 2 {
				t.Fatalf("second reply: %v", err)
			}
			if in.LinkStats().Pings == 0 {
				t.Error("no pings counted")
			}
		})
	}
}

func expectErr(t *testing.T, in backend.Instance, target error) {
	t.Helper()
	for {
		_, err := in.Recv()
		if err == nil {
			continue
		}
		if !errors.Is(err, target) {
			t.Fatalf("error %v, want %v", err, target)
		}
		t.Logf("got expected error: %v", err)
		return
	}
}

func TestCrash(t *testing.T) {
	in := start(t, newBackend(t, "v2"))
	in.Send(cmdFrame(1, faketcd.CmdCrash, 0))
	expectErr(t, in, backend.ErrExited)
}

func TestExitAtStart(t *testing.T) {
	b := newBackend(t, "v2", "TCDPOOL_FAKE_EXIT_AT_START=1")
	_, err := b.Start(context.Background(), "x")
	if !errors.Is(err, backend.ErrExited) {
		t.Fatalf("err %v", err)
	}
}

func TestHangIsStalled(t *testing.T) {
	in := start(t, newBackend(t, "v2"))
	in.Send(cmdFrame(1, faketcd.CmdHang, 0))
	expectErr(t, in, backend.ErrStalled)
}

func TestSplitWrite(t *testing.T) {
	for _, framing := range []string{"v2", "raw"} {
		t.Run(framing, func(t *testing.T) {
			in := start(t, newBackend(t, framing))
			in.Send(cmdFrame(5, faketcd.CmdSplitWrite, 0))
			r, err := in.Recv()
			if err != nil || r.Seq != 5 {
				t.Fatalf("%v %+v", err, r)
			}
		})
	}
}

// Raw framing has no way to find the next packet after junk, so junk,
// a wrong module letter or a bad codec is a desync.
func TestRawDesync(t *testing.T) {
	for name, cmd := range map[string]int{"garbage": faketcd.CmdGarbage, "wrong_module": faketcd.CmdWrongModule, "bad_codec": faketcd.CmdBadCodec} {
		t.Run(name, func(t *testing.T) {
			in := start(t, newBackend(t, "raw"))
			in.Send(cmdFrame(1, cmd, 0))
			expectErr(t, in, backend.ErrDesync)
		})
	}
}

// v2 framing skips a bad frame and finds the next good one.
func TestV2Resync(t *testing.T) {
	cases := map[string]struct {
		cmd       int
		replyLost bool // the reply to the bad frame is lost, not repaired
	}{
		"garbage_between_frames": {faketcd.CmdGarbage, false},
		"bad_crc":                {faketcd.CmdBadCRC, true},
		"wrong_version":          {faketcd.CmdWrongVersion, true},
		"wrong_module":           {faketcd.CmdWrongModule, true},
		"bad_codec":              {faketcd.CmdBadCodec, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in := start(t, newBackend(t, "v2"))
			in.Send(cmdFrame(1, c.cmd, 0))
			in.Send(pcm(2, 0))
			r, err := in.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if c.replyLost {
				if r.Seq != 2 {
					t.Fatalf("got seq %d, want 2", r.Seq)
				}
				if in.LinkStats().BadFrames != 1 {
					t.Errorf("bad frames %d, want 1", in.LinkStats().BadFrames)
				}
			} else {
				if r.Seq != 1 {
					t.Fatalf("got seq %d, want 1", r.Seq)
				}
				if r2, err := in.Recv(); err != nil || r2.Seq != 2 {
					t.Fatalf("second reply %v %+v", err, r2)
				}
				if in.LinkStats().SkipBytes == 0 {
					t.Error("no skipped bytes counted")
				}
			}
		})
	}
}

func TestV2TooManyBad(t *testing.T) {
	in := start(t, newBackend(t, "v2"))
	for i := range nostar.V2MaxBadRun {
		in.Send(cmdFrame(uint32(i), faketcd.CmdBadCRC, 0))
	}
	expectErr(t, in, backend.ErrDesync)
}

func TestModuleLetters(t *testing.T) {
	b := newBackend(t, "v2")
	a := start(t, b)
	c := start(t, b)
	if a.PID() == c.PID() || a.PID() == 0 {
		t.Fatal("two instances share a process")
	}
	// each instance answers on its own link
	a.Send(pcm(1, 0))
	c.Send(pcm(2, 0))
	ra, err1 := a.Recv()
	rc, err2 := c.Recv()
	if err1 != nil || err2 != nil || ra.Seq != 1 || rc.Seq != 2 {
		t.Fatalf("%v %v", err1, err2)
	}
}

func TestBadOptions(t *testing.T) {
	if _, err := nostar.New(nostar.Options{Binary: "x", PortMin: 100, PortMax: 200}); err == nil {
		t.Error("port range below 1025 accepted")
	}
	if _, err := nostar.New(nostar.Options{Binary: "x", Framing: "v3"}); err == nil {
		t.Error("framing v3 accepted")
	}
	if _, err := nostar.New(nostar.Options{Binary: "x", Gains: nostar.Gains{DMRIn: 30}}); err == nil {
		t.Error("gain 30 accepted")
	}
}
