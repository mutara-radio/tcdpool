package nostar

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dbehnke/tcdpool/internal/backend"
	"github.com/dbehnke/tcdpool/internal/codec"
)

// Gains are the tcd.ini gain values in dB (-24..24). 0 is neutral.
type Gains struct {
	DStarIn  int `json:"dstar_in"`
	DStarOut int `json:"dstar_out"`
	DMRIn    int `json:"dmr_in"`
	DMROut   int `json:"dmr_out"`
	USRPTx   int `json:"usrp_tx"`
	USRPRx   int `json:"usrp_rx"`
}

// Options configure the nostar backend.
type Options struct {
	Binary  string // path of the tcd binary
	Args    []string
	WorkDir string // tcd.ini files go here
	// PortMin and PortMax limit the loopback ports where tcdpool waits
	// for tcd. tcd accepts only 1025..49000.
	PortMin, PortMax int
	Framing          string // "v2" (default) or "raw"
	Gains            Gains
	// ConnectTimeout is the time tcd gets to start and connect.
	ConnectTimeout time.Duration
	// Watchdog: the link is stalled when tcd sends nothing (tcd sends a
	// ping about every 5 s) for this long.
	Watchdog time.Duration
	// Env is added to the environment of tcd (tests use it).
	Env []string
	// Logf logs a line. It must not be nil.
	Logf func(format string, args ...any)
}

// Backend starts nostar tcd processes.
type Backend struct {
	opt    Options
	module atomic.Uint32
}

// New returns a nostar backend.
func New(opt Options) (*Backend, error) {
	if opt.Binary == "" {
		return nil, errors.New("nostar: no tcd binary")
	}
	if opt.PortMin == 0 {
		opt.PortMin, opt.PortMax = 40000, 48999
	}
	if opt.PortMin < 1025 || opt.PortMax > 49000 || opt.PortMin > opt.PortMax {
		return nil, fmt.Errorf("nostar: port range %d-%d must be inside 1025-49000 (a tcd limit)", opt.PortMin, opt.PortMax)
	}
	switch opt.Framing {
	case "":
		opt.Framing = "v2"
	case "v2", "raw":
	default:
		return nil, fmt.Errorf("nostar: framing %q must be v2 or raw", opt.Framing)
	}
	if opt.ConnectTimeout == 0 {
		opt.ConnectTimeout = 15 * time.Second
	}
	if opt.Watchdog == 0 {
		opt.Watchdog = 20 * time.Second
	}
	if opt.WorkDir == "" {
		opt.WorkDir = os.TempDir()
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	for _, g := range []int{opt.Gains.DStarIn, opt.Gains.DStarOut, opt.Gains.DMRIn, opt.Gains.DMROut, opt.Gains.USRPTx, opt.Gains.USRPRx} {
		if g < -24 || g > 24 {
			return nil, fmt.Errorf("nostar: gain %d dB is outside -24..24", g)
		}
	}
	if err := os.MkdirAll(opt.WorkDir, 0o700); err != nil {
		return nil, err
	}
	return &Backend{opt: opt}, nil
}

// Name implements backend.Backend.
func (b *Backend) Name() string { return "nostar-tcd" }

// Capabilities implements backend.Backend.
func (b *Backend) Capabilities() backend.Capabilities {
	all := codec.SetOf(codec.All()...)
	return backend.Capabilities{Inputs: all, Outputs: all}
}

func (b *Backend) nextModule() byte {
	return byte('A' + (b.module.Add(1)-1)%26)
}

// listen opens a loopback listener on a free port in the range, from a
// random start.
func (b *Backend) listen() (*net.TCPListener, int, error) {
	n := b.opt.PortMax - b.opt.PortMin + 1
	start := rand.IntN(n)
	var lastErr error
	for i := 0; i < n && i < 200; i++ {
		port := b.opt.PortMin + (start+i)%n
		l, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if err == nil {
			return l, port, nil
		}
		lastErr = err
	}
	return nil, 0, fmt.Errorf("nostar: no free port in %d-%d: %w", b.opt.PortMin, b.opt.PortMax, lastErr)
}

func (b *Backend) writeIni(path string, port int, module byte) error {
	g := b.opt.Gains
	ini := fmt.Sprintf(`# Written by tcdpool. Each start writes it again.
Port = %d
ServerAddress = 127.0.0.1
Modules = %c
Framing = %s
DStarGainIn = %d
DStarGainOut = %d
DmrYsfGainIn = %d
DmrYsfGainOut = %d
UsrpTxGain = %d
UsrpRxGain = %d
`, port, module, b.opt.Framing, g.DStarIn, g.DStarOut, g.DMRIn, g.DMROut, g.USRPTx, g.USRPRx)
	return os.WriteFile(path, []byte(ini), 0o600)
}

func safeName(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

// Start implements backend.Backend.
func (b *Backend) Start(ctx context.Context, id string) (backend.Instance, error) {
	l, port, err := b.listen()
	if err != nil {
		return nil, err
	}
	defer l.Close()
	module := b.nextModule()
	ini := filepath.Join(b.opt.WorkDir, fmt.Sprintf("%s-%d.ini", safeName(id), port))
	if err := b.writeIni(ini, port, module); err != nil {
		return nil, err
	}

	cmd := exec.Command(b.opt.Binary, append(append([]string{}, b.opt.Args...), ini)...)
	cmd.Env = append(os.Environ(), b.opt.Env...)
	cmd.SysProcAttr = sysProcAttr()
	out, err := cmd.StdoutPipe()
	if err != nil {
		os.Remove(ini)
		return nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		os.Remove(ini)
		return nil, fmt.Errorf("nostar: start %s: %w", b.opt.Binary, err)
	}
	in := &instance{
		id:     id,
		cmd:    cmd,
		module: module,
		v2:     b.opt.Framing == "v2",
		ini:    ini,
		opt:    &b.opt,
		exited: make(chan struct{}),
	}
	logDone := make(chan struct{})
	go in.logLines(out, logDone)
	go func() {
		<-logDone // read all output before Wait closes the pipe
		in.exitErr = cmd.Wait()
		close(in.exited)
	}()

	conn, err := in.accept(ctx, l)
	if err != nil {
		in.Stop()
		return nil, err
	}
	in.conn = conn
	in.lastPacket.Store(time.Now().UnixNano())
	return in, nil
}

type instance struct {
	id     string
	cmd    *exec.Cmd
	conn   *net.TCPConn
	module byte
	v2     bool
	ini    string
	opt    *Options

	exited  chan struct{}
	exitErr error

	wmu  sync.Mutex
	wbuf [V2FrameSize]byte

	rx    V2Reader
	rbuf  [4096]byte
	raw   [PacketSize]byte
	inBad bool // in a burst of bad v2 frames (log once per burst)

	pings      atomic.Uint64
	badFrames  atomic.Uint64
	skipBytes  atomic.Uint64
	lastPacket atomic.Int64

	stopOnce sync.Once
}

// logLines copies the tcd output to the log, at most 20 lines a second,
// so a looping tcd cannot flood the log.
func (in *instance) logLines(r io.Reader, done chan<- struct{}) {
	defer close(done)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 64*1024)
	var windowStart time.Time
	n, dropped := 0, 0
	for sc.Scan() {
		now := time.Now()
		if now.Sub(windowStart) > time.Second {
			if dropped > 0 {
				in.opt.Logf("tcd %s: %d lines not shown", in.id, dropped)
			}
			windowStart, n, dropped = now, 0, 0
		}
		if n++; n > 20 {
			dropped++
			continue
		}
		in.opt.Logf("tcd %s: %s", in.id, sc.Text())
	}
}

func (in *instance) accept(ctx context.Context, l *net.TCPListener) (*net.TCPConn, error) {
	type res struct {
		c   *net.TCPConn
		err error
	}
	ch := make(chan res, 1)
	l.SetDeadline(time.Now().Add(in.opt.ConnectTimeout))
	go func() {
		c, err := l.AcceptTCP()
		ch <- res{c, err}
	}()
	var r res
	select {
	case r = <-ch:
	case <-in.exited:
		l.Close()
		return nil, fmt.Errorf("%w before it connected: %v", backend.ErrExited, in.exitErr)
	case <-ctx.Done():
		l.Close()
		return nil, ctx.Err()
	}
	if r.err != nil {
		return nil, fmt.Errorf("nostar: tcd did not connect in %s: %w", in.opt.ConnectTimeout, r.err)
	}
	c := r.c
	c.SetNoDelay(true)
	c.SetReadDeadline(time.Now().Add(in.opt.ConnectTimeout))
	var letter [1]byte
	if _, err := io.ReadFull(c, letter[:]); err != nil {
		c.Close()
		return nil, fmt.Errorf("nostar: no module letter from tcd: %w", err)
	}
	if letter[0] != in.module {
		c.Close()
		return nil, fmt.Errorf("%w: tcd sent module %q, want %q", backend.ErrDesync, letter[0], in.module)
	}
	c.SetReadDeadline(time.Time{})
	return c, nil
}

// PID implements backend.Instance.
func (in *instance) PID() int {
	if in.cmd.Process == nil {
		return 0
	}
	return in.cmd.Process.Pid
}

// LinkStats implements backend.Instance.
func (in *instance) LinkStats() backend.LinkStats {
	return backend.LinkStats{
		Pings:      in.pings.Load(),
		BadFrames:  in.badFrames.Load(),
		SkipBytes:  in.skipBytes.Load(),
		LastPacket: time.Unix(0, in.lastPacket.Load()),
	}
}

// Send implements backend.Instance.
func (in *instance) Send(f *backend.Frame) error {
	p, mask, ok := ToPacket(f, in.module)
	if !ok {
		return fmt.Errorf("nostar: input codec %v not supported", f.In)
	}
	in.wmu.Lock()
	defer in.wmu.Unlock()
	var b []byte
	if in.v2 {
		p.EncodeV2(in.wbuf[:], mask)
		b = in.wbuf[:V2FrameSize]
	} else {
		p.Encode(in.wbuf[:])
		b = in.wbuf[:PacketSize]
	}
	in.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, err := in.conn.Write(b)
	if err != nil {
		return in.linkErr(err)
	}
	return nil
}

// linkErr classifies a link error.
func (in *instance) linkErr(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		select {
		case <-in.exited:
			return fmt.Errorf("%w: %v", backend.ErrExited, in.exitErr)
		default:
		}
		return fmt.Errorf("%w: nothing from tcd for %s", backend.ErrStalled, in.opt.Watchdog)
	}
	// The link closed. If tcd ended, say so; it ends a moment after it
	// closes its socket.
	select {
	case <-in.exited:
		return fmt.Errorf("%w: %v", backend.ErrExited, in.exitErr)
	case <-time.After(300 * time.Millisecond):
	}
	return fmt.Errorf("%w: link closed: %v", backend.ErrDesync, err)
}

func (in *instance) checkModule(p *Packet) bool {
	return p.CodecIn == CodecPing || p.Module == in.module
}

// Recv implements backend.Instance. Only one goroutine may call it.
func (in *instance) Recv() (*backend.Frame, error) {
	for {
		if in.v2 {
			f, err, ok := in.nextV2()
			if ok {
				return f, err
			}
		} else {
			in.conn.SetReadDeadline(time.Now().Add(in.opt.Watchdog))
			if _, err := io.ReadFull(in.conn, in.raw[:]); err != nil {
				return nil, in.linkErr(err)
			}
			var p Packet
			if err := p.Decode(in.raw[:]); err != nil {
				return nil, fmt.Errorf("%w: %v", backend.ErrDesync, err)
			}
			in.lastPacket.Store(time.Now().UnixNano())
			if !in.checkModule(&p) {
				return nil, fmt.Errorf("%w: packet for module %q on the link of %q", backend.ErrDesync, p.Module, in.module)
			}
			if p.CodecIn == CodecPing {
				in.pings.Add(1)
				continue
			}
			return FromPacket(&p, 0), nil
		}
	}
}

// nextV2 returns a frame or an error with ok, or reads more bytes and
// returns ok=false.
func (in *instance) nextV2() (*backend.Frame, error, bool) {
	for {
		var p Packet
		st, outputs, why := in.rx.Next(&p)
		in.skipBytes.Store(in.rx.SkipTotal())
		switch st {
		case V2Packet:
			if !in.checkModule(&p) {
				st, why = in.rx.Refuse(fmt.Sprintf("packet for module %q", p.Module))
				break
			}
			in.lastPacket.Store(time.Now().UnixNano())
			if in.inBad {
				in.inBad = false
				in.opt.Logf("tcd %s: v2 link in step again", in.id)
			}
			if p.CodecIn == CodecPing {
				in.pings.Add(1)
				continue
			}
			return FromPacket(&p, outputs), nil, true
		case V2NeedMore:
			in.conn.SetReadDeadline(time.Now().Add(in.opt.Watchdog))
			n, err := in.conn.Read(in.rbuf[:])
			if n > 0 {
				in.rx.Append(in.rbuf[:n])
			}
			if err != nil {
				return nil, in.linkErr(err), true
			}
			continue
		}
		// V2Bad or V2TooManyBad
		in.badFrames.Add(1)
		if !in.inBad {
			in.inBad = true
			in.opt.Logf("tcd %s: bad v2 frame: %s (%d bad so far)", in.id, why, in.rx.BadTotal())
		}
		if st == V2TooManyBad {
			return nil, fmt.Errorf("%w: %d bad v2 frames in a row: %s", backend.ErrDesync, in.rx.BadRun(), why), true
		}
	}
}

// Stop implements backend.Instance.
func (in *instance) Stop() {
	in.stopOnce.Do(func() {
		if in.conn != nil {
			in.conn.Close()
		}
		if in.cmd.Process != nil {
			in.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-in.exited:
			case <-time.After(2 * time.Second):
				in.cmd.Process.Kill()
				<-in.exited
			}
		}
		os.Remove(in.ini)
	})
}
