package pool

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dbehnke/tcdpool/internal/backend"
	"github.com/dbehnke/tcdpool/internal/backend/nostar"
	"github.com/dbehnke/tcdpool/internal/codec"
	"github.com/dbehnke/tcdpool/internal/faketcd"
)

func TestMain(m *testing.M) {
	if os.Getenv("TCDPOOL_FAKETCD") == "1" {
		os.Exit(faketcd.Main(os.Args[len(os.Args)-1:]))
	}
	os.Exit(m.Run())
}

type event struct {
	kind      string // reply, restarting, ready, failed
	d         Delivered
	code      string
	restarted bool
}

type sink struct{ ch chan event }

func newSink() *sink { return &sink{ch: make(chan event, 1000)} }

func (s *sink) Reply(d Delivered) { s.ch <- event{kind: "reply", d: d} }
func (s *sink) Restarting(reason string, _ time.Duration) {
	s.ch <- event{kind: "restarting", code: reason}
}
func (s *sink) Ready(restarted bool)  { s.ch <- event{kind: "ready", restarted: restarted} }
func (s *sink) Failed(code, _ string) { s.ch <- event{kind: "failed", code: code} }

func (s *sink) wait(t *testing.T, kind string, within time.Duration) event {
	t.Helper()
	end := time.After(within)
	for {
		select {
		case e := <-s.ch:
			if e.kind == kind {
				return e
			}
		case <-end:
			t.Fatalf("no %s event in %s", kind, within)
		}
	}
}

// countingBackend wraps a backend; it can refuse starts after the first n.
type countingBackend struct {
	backend.Backend
	starts    atomic.Int32
	failAfter int32
}

func (b *countingBackend) Start(ctx context.Context, id string) (backend.Instance, error) {
	n := b.starts.Add(1)
	if b.failAfter > 0 && n > b.failAfter {
		return nil, errors.New("test: start refused")
	}
	return b.Backend.Start(ctx, id)
}

func testPool(t *testing.T, framing string, mod func(*Options)) (*Pool, *countingBackend) {
	t.Helper()
	nb, err := nostar.New(nostar.Options{
		Binary:         os.Args[0],
		WorkDir:        t.TempDir(),
		Framing:        framing,
		ConnectTimeout: 5 * time.Second,
		Watchdog:       time.Second,
		Env:            []string{"TCDPOOL_FAKETCD=1", "TCDPOOL_FAKE_PING_MS=200"},
		Logf:           t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	cb := &countingBackend{Backend: nb}
	opt := Options{
		Backend:      cb,
		IdleTimeout:  time.Minute,
		MaxProcesses: 8,
		LateAfter:    60 * time.Millisecond,
		MissingAfter: 400 * time.Millisecond,
		MinBackoff:   50 * time.Millisecond,
		MaxBackoff:   200 * time.Millisecond,
		MaxFailures:  3,
		Logf:         t.Logf,
	}
	if mod != nil {
		mod(&opt)
	}
	p := New(opt)
	t.Cleanup(p.Close)
	return p, cb
}

func attach(t *testing.T, p *Pool, keyID, session string, keyMax int) (*Attachment, *sink, bool) {
	t.Helper()
	s := newSink()
	a, reused, err := p.Attach(context.Background(), Key{keyID, session}, keyMax, "", s)
	if err != nil {
		t.Fatal(err)
	}
	return a, s, reused
}

func pcm(seq uint32) *backend.Frame {
	return &backend.Frame{StreamID: 0x2100, Seq: seq, In: codec.PCM}
}

func cmd(seq uint32, c, arg int) *backend.Frame {
	f := pcm(seq)
	f.PCM[0], f.PCM[1], f.PCM[2] = faketcd.CmdMagic, int16(c), int16(arg)
	return f
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func TestSendReply(t *testing.T) {
	p, _ := testPool(t, "v2", nil)
	a, s, reused := attach(t, p, "k1", "s1", 2)
	if reused {
		t.Fatal("new process reported as reused")
	}
	want := codec.SetOf(codec.DMR)
	f := pcm(1)
	f.Want = want
	if err := a.Send(f, want); err != nil {
		t.Fatal(err)
	}
	e := s.wait(t, "reply", 3*time.Second)
	if e.d.Frame.Seq != 1 || e.d.Want != want || e.d.Frame.Have != want || e.d.Latency <= 0 {
		t.Fatalf("reply %+v", e.d)
	}
	st := a.Stats()
	if st.FramesIn != 1 || st.FramesOut != 1 || st.State != "running" || st.Latency.Count != 1 || st.PID == 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestReuseAndIdleStop(t *testing.T) {
	p, _ := testPool(t, "v2", func(o *Options) { o.IdleTimeout = 500 * time.Millisecond })
	a, _, _ := attach(t, p, "k1", "s1", 2)
	pid := a.Stats().PID
	a.Detach(false)
	if st := p.Stats(); len(st) != 1 || st[0].State != "idle" {
		t.Fatalf("after detach: %+v", st)
	}
	b, s, reused := attach(t, p, "k1", "s1", 2)
	if !reused || b.Stats().PID != pid {
		t.Fatalf("reconnect did not reuse the process (reused=%v)", reused)
	}
	b.Send(pcm(2), 0)
	s.wait(t, "reply", 3*time.Second)
	b.Detach(false)
	time.Sleep(1200 * time.Millisecond)
	if p.Count() != 0 {
		t.Fatalf("%d processes after the idle timeout", p.Count())
	}
	time.Sleep(300 * time.Millisecond)
	if alive(pid) {
		t.Fatalf("process %d still alive after the idle timeout", pid)
	}
}

func TestByeStop(t *testing.T) {
	p, _ := testPool(t, "v2", nil)
	a, _, _ := attach(t, p, "k1", "s1", 2)
	pid := a.Stats().PID
	a.Detach(true)
	time.Sleep(500 * time.Millisecond)
	if p.Count() != 0 || alive(pid) {
		t.Fatal("process not stopped at once")
	}
}

func TestCrashRestart(t *testing.T) {
	p, _ := testPool(t, "v2", nil)
	a, s, _ := attach(t, p, "k1", "s1", 2)
	pid := a.Stats().PID
	a.Send(cmd(1, faketcd.CmdCrash, 0), 0)
	s.wait(t, "restarting", 3*time.Second)
	if e := s.wait(t, "ready", 5*time.Second); !e.restarted {
		t.Fatal("ready after restart without restarted=true")
	}
	st := a.Stats()
	if st.Crashes != 1 || st.Starts != 2 || st.PID == pid || st.Missing != 1 {
		t.Fatalf("stats %+v", st)
	}
	a.Send(pcm(2), 0)
	if e := s.wait(t, "reply", 3*time.Second); e.d.Frame.Seq != 2 {
		t.Fatal("wrong reply after restart")
	}
}

func TestDesyncRestart(t *testing.T) {
	cases := []struct {
		name, framing string
		cmd           int
		count         func(ProcStats) uint64
	}{
		{"unknown_seq", "v2", faketcd.CmdUnknownSeq, func(s ProcStats) uint64 { return s.Desyncs }},
		{"raw_garbage", "raw", faketcd.CmdGarbage, func(s ProcStats) uint64 { return s.Desyncs }},
		{"raw_wrong_module", "raw", faketcd.CmdWrongModule, func(s ProcStats) uint64 { return s.Desyncs }},
		{"raw_bad_codec", "raw", faketcd.CmdBadCodec, func(s ProcStats) uint64 { return s.Desyncs }},
		{"hang", "v2", faketcd.CmdHang, func(s ProcStats) uint64 { return s.Stalls }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, _ := testPool(t, c.framing, nil)
			a, s, _ := attach(t, p, "k1", "s1", 2)
			a.Send(cmd(1, c.cmd, 0), 0)
			s.wait(t, "restarting", 4*time.Second)
			s.wait(t, "ready", 5*time.Second)
			if n := c.count(a.Stats()); n != 1 {
				t.Fatalf("count %d, stats %+v", n, a.Stats())
			}
			a.Send(pcm(2), 0)
			s.wait(t, "reply", 3*time.Second)
		})
	}
}

// v2 framing skips a bad frame: no restart, the frame is missing.
func TestV2BadFrameNoRestart(t *testing.T) {
	p, _ := testPool(t, "v2", nil)
	a, s, _ := attach(t, p, "k1", "s1", 2)
	for i, c := range []int{faketcd.CmdBadCRC, faketcd.CmdWrongVersion, faketcd.CmdWrongModule, faketcd.CmdSplitWrite, faketcd.CmdGarbage} {
		a.Send(cmd(uint32(10+i), c, 0), 0)
	}
	a.Send(pcm(20), 0)
	got := map[uint32]bool{}
	for len(got) < 3 {
		got[s.wait(t, "reply", 3*time.Second).d.Frame.Seq] = true
	}
	if !got[13] || !got[14] || !got[20] {
		t.Fatalf("replies %v", got)
	}
	time.Sleep(700 * time.Millisecond)
	st := a.Stats()
	if st.Starts != 1 || st.Desyncs != 0 || st.Link.BadFrames != 4 || st.Missing != 3 {
		t.Fatalf("stats %+v", st)
	}
}

func TestGiveUp(t *testing.T) {
	p, cb := testPool(t, "v2", nil)
	cb.failAfter = 1
	a, s, _ := attach(t, p, "k1", "s1", 2)
	a.Send(cmd(1, faketcd.CmdCrash, 0), 0)
	if e := s.wait(t, "failed", 5*time.Second); e.code != "backend_failed" {
		t.Fatalf("code %s", e.code)
	}
	if p.Count() != 0 {
		t.Fatal("failed process still in the pool")
	}
	if n := cb.starts.Load(); n != 4 { // 1 good + 3 refused
		t.Errorf("%d starts", n)
	}
}

func TestLimits(t *testing.T) {
	p, _ := testPool(t, "v2", func(o *Options) { o.MaxProcesses = 3 })
	s1, _, _ := attach(t, p, "k1", "s1", 2)
	attach(t, p, "k1", "s2", 2)
	if _, _, err := p.Attach(context.Background(), Key{"k1", "s3"}, 2, "", newSink()); !errors.Is(err, ErrKeyLimit) {
		t.Fatalf("third process of a key: %v", err)
	}
	// an idle process of the key makes room
	s1.Detach(false)
	attach(t, p, "k1", "s3", 2)
	if p.Count() != 2 {
		t.Fatalf("%d processes", p.Count())
	}
	attach(t, p, "k2", "s1", 2)
	if _, _, err := p.Attach(context.Background(), Key{"k3", "s1"}, 2, "", newSink()); !errors.Is(err, ErrHostFull) {
		t.Fatalf("host full: %v", err)
	}
	// one process each, all different
	pids := map[int]bool{}
	for _, st := range p.Stats() {
		pids[st.PID] = true
	}
	if len(pids) != 3 {
		t.Fatalf("pids %v", pids)
	}
}

func TestReplacedAndStopKey(t *testing.T) {
	p, _ := testPool(t, "v2", nil)
	_, s1, _ := attach(t, p, "k1", "s1", 2)
	b, _, reused := attach(t, p, "k1", "s1", 2)
	if !reused {
		t.Fatal("not reused")
	}
	if e := s1.wait(t, "failed", time.Second); e.code != "replaced" {
		t.Fatalf("old client: %s", e.code)
	}
	pid := b.Stats().PID
	if n := p.StopKey("k1", "key_revoked", "disabled"); n != 1 {
		t.Fatalf("StopKey stopped %d", n)
	}
	time.Sleep(500 * time.Millisecond)
	if alive(pid) || p.Count() != 0 {
		t.Fatal("process of a revoked key still runs")
	}
}

func TestLateAndMissing(t *testing.T) {
	p, _ := testPool(t, "v2", nil)
	a, s, _ := attach(t, p, "k1", "s1", 2)
	a.Send(cmd(1, faketcd.CmdSlow, 150), 0)
	if e := s.wait(t, "reply", 3*time.Second); !e.d.Late {
		t.Fatalf("150 ms reply not late: %v", e.d.Latency)
	}
	a.Send(cmd(2, faketcd.CmdSlow, 900), 0)
	time.Sleep(1500 * time.Millisecond)
	st := a.Stats()
	if st.Late != 2 || st.Missing != 1 || st.FramesOut != 1 || st.Dropped != 1 || st.Desyncs != 0 {
		t.Fatalf("stats %+v", st)
	}
}
