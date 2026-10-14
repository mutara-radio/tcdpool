// Package pool runs one transcoder process for each (key, session) and
// connects it to at most one client at a time.
//
//   - A process starts when a client attaches with a new (key, session).
//   - When the client goes away, the process stays for the idle timeout.
//     A client that comes back with the same key and session in that
//     time gets the same process.
//   - A process that fails (exit, desync, stall) starts again with a
//     backoff, and the client is told.
//   - Limits: processes per key and processes per host. An idle process
//     is stopped to make room before a client is refused.
package pool

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbehnke/tcdpool/internal/backend"
	"github.com/dbehnke/tcdpool/internal/codec"
)

// Options configure a pool.
type Options struct {
	Backend      backend.Backend
	IdleTimeout  time.Duration
	MaxProcesses int // per host
	LateAfter    time.Duration
	MissingAfter time.Duration
	MinBackoff   time.Duration
	MaxBackoff   time.Duration
	MaxFailures  int // starts in a row with no good reply, then give up
	// MaxPending is the most frames in the transcoder at one time for
	// one process. More are refused (backlog).
	MaxPending int
	Logf       func(format string, args ...any)
}

// Errors from Attach and Send.
var (
	ErrKeyLimit     = errors.New("the key has its maximum number of processes")
	ErrHostFull     = errors.New("the host has its maximum number of processes")
	ErrBackendStart = errors.New("the transcoder did not start")
	ErrShuttingDown = errors.New("the server is shutting down")
	ErrNotReady     = errors.New("the transcoder is restarting")
	ErrBacklog      = errors.New("too many frames are waiting for the transcoder")
	ErrDetached     = errors.New("the session is not attached")
)

// Sink receives the events of an attachment. Its methods must not block.
type Sink interface {
	Reply(d Delivered)
	Restarting(reason string, retryIn time.Duration)
	Ready(restarted bool)
	// Failed: the attachment is closed. code is a protocol error code.
	Failed(code, message string)
}

// Delivered is one reply for the client.
type Delivered struct {
	Frame   *backend.Frame
	Want    codec.Set // what the client asked for
	Latency time.Duration
	Late    bool
}

// Key identifies a process.
type Key struct {
	KeyID   string
	Session string
}

// Pool is the set of processes.
type Pool struct {
	opt    Options
	mu     sync.Mutex
	procs  map[Key]*Proc
	closed bool
	seq    atomic.Uint64
}

// New makes a pool.
func New(opt Options) *Pool {
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	if opt.MaxPending == 0 {
		opt.MaxPending = 256
	}
	if opt.MaxFailures == 0 {
		opt.MaxFailures = 5
	}
	if opt.MinBackoff == 0 {
		opt.MinBackoff = 500 * time.Millisecond
	}
	if opt.MaxBackoff == 0 {
		opt.MaxBackoff = 30 * time.Second
	}
	if opt.MissingAfter == 0 {
		opt.MissingAfter = 2 * time.Second
	}
	if opt.LateAfter == 0 {
		opt.LateAfter = 60 * time.Millisecond
	}
	return &Pool{opt: opt, procs: map[Key]*Proc{}}
}

// Attachment is one client on one process.
type Attachment struct {
	proc *Proc
	sink Sink
}

// Attach connects a client to the process of (key, session), and starts
// the process if there is none. keyMax is the process limit of the key.
// reused is true when the process was already running.
func (p *Pool) Attach(ctx context.Context, k Key, keyMax int, label string, sink Sink) (a *Attachment, reused bool, err error) {
	a = &Attachment{sink: sink}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, false, ErrShuttingDown
	}
	if pr := p.procs[k]; pr != nil {
		old, ok := pr.attach(a)
		if ok {
			p.mu.Unlock()
			a.proc = pr
			if old != nil {
				old.sink.Failed("replaced", "another connection took this session")
			}
			p.opt.Logf("pool: %s: client attached to the running process", pr.id)
			return a, true, nil
		}
		// it is stopping; start a new one
		delete(p.procs, k)
	}
	if err := p.makeRoom(k.KeyID, keyMax); err != nil {
		p.mu.Unlock()
		return nil, false, err
	}
	pr := newProc(p, k, label, fmt.Sprintf("%s-%s-%d", k.KeyID, k.Session, p.seq.Add(1)))
	pr.att = a
	a.proc = pr
	p.procs[k] = pr
	p.mu.Unlock()

	inst, err := p.opt.Backend.Start(ctx, pr.id)
	if err != nil {
		p.mu.Lock()
		if p.procs[k] == pr {
			delete(p.procs, k)
		}
		p.mu.Unlock()
		pr.stop()
		return nil, false, fmt.Errorf("%w: %v", ErrBackendStart, err)
	}
	pr.started(inst)
	go pr.supervise(inst)
	p.opt.Logf("pool: %s: started, pid %d", pr.id, inst.PID())
	return a, false, nil
}

// makeRoom checks the limits and stops idle processes to make room.
// p.mu is held.
func (p *Pool) makeRoom(keyID string, keyMax int) error {
	var ofKey []*Proc
	for _, pr := range p.procs {
		if pr.key.KeyID == keyID {
			ofKey = append(ofKey, pr)
		}
	}
	if len(ofKey) >= keyMax {
		if !p.evictIdle(ofKey) {
			return ErrKeyLimit
		}
	}
	if len(p.procs) >= p.opt.MaxProcesses {
		all := make([]*Proc, 0, len(p.procs))
		for _, pr := range p.procs {
			all = append(all, pr)
		}
		if !p.evictIdle(all) {
			return ErrHostFull
		}
	}
	return nil
}

// evictIdle stops the process that has been idle the longest in the
// list. p.mu is held.
func (p *Pool) evictIdle(list []*Proc) bool {
	var best *Proc
	var bestSince time.Time
	for _, pr := range list {
		if since, idle := pr.idle(); idle && (best == nil || since.Before(bestSince)) {
			best, bestSince = pr, since
		}
	}
	if best == nil {
		return false
	}
	p.opt.Logf("pool: %s: stopped to make room (idle since %s)", best.id, bestSince.Format(time.TimeOnly))
	delete(p.procs, best.key)
	best.stop()
	return true
}

func (p *Pool) remove(pr *Proc) {
	p.mu.Lock()
	if p.procs[pr.key] == pr {
		delete(p.procs, pr.key)
	}
	p.mu.Unlock()
}

// StopKey stops every process of a key (for example when the key is
// disabled) and tells the clients.
func (p *Pool) StopKey(keyID, code, message string) int {
	p.mu.Lock()
	var list []*Proc
	for k, pr := range p.procs {
		if k.KeyID == keyID {
			list = append(list, pr)
			delete(p.procs, k)
		}
	}
	p.mu.Unlock()
	for _, pr := range list {
		if a := pr.detachAll(); a != nil {
			a.sink.Failed(code, message)
		}
		pr.stop()
	}
	return len(list)
}

// Close stops all processes.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	list := make([]*Proc, 0, len(p.procs))
	for _, pr := range p.procs {
		list = append(list, pr)
	}
	clear(p.procs)
	p.mu.Unlock()
	var wg sync.WaitGroup
	for _, pr := range list {
		if a := pr.detachAll(); a != nil {
			a.sink.Failed("shutting_down", "the server is shutting down")
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			pr.stopWait()
		}()
	}
	wg.Wait()
}

// Stats returns the stats of all processes, sorted by ID.
func (p *Pool) Stats() []ProcStats {
	p.mu.Lock()
	list := make([]*Proc, 0, len(p.procs))
	for _, pr := range p.procs {
		list = append(list, pr)
	}
	p.mu.Unlock()
	out := make([]ProcStats, 0, len(list))
	for _, pr := range list {
		out = append(out, pr.Stats())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Count returns the number of processes.
func (p *Pool) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.procs)
}

// Send passes a frame to the process. want is what the client wants in
// the reply.
func (a *Attachment) Send(f *backend.Frame, want codec.Set) error {
	return a.proc.send(a, f, want)
}

// Detach disconnects the client. The process stays for the idle
// timeout, or stops at once with stopNow.
func (a *Attachment) Detach(stopNow bool) {
	a.proc.detach(a, stopNow)
}

// Stats returns the stats of the process.
func (a *Attachment) Stats() ProcStats { return a.proc.Stats() }

// ID returns the process ID (for logs).
func (a *Attachment) ID() string { return a.proc.id }

type pkey struct {
	sid uint16
	seq uint32
}

type pend struct {
	sent    time.Time
	want    codec.Set
	in      codec.ID
	missing bool
}

// Proc is one transcoder process and its state.
type Proc struct {
	pool    *Pool
	key     Key
	label   string
	id      string
	created time.Time
	stopCh  chan struct{}
	done    chan struct{} // closed when supervise ends

	mu        sync.Mutex
	inst      backend.Instance
	att       *Attachment
	idleTimer *time.Timer
	idleSince time.Time
	pending   map[pkey]pend
	stopped   bool
	lastStart time.Time
	failures  int
	pid       int

	starts, crashes, desyncs, stalls      uint64
	framesIn, framesOut, late, missing    uint64
	dropped, unknownReplies, sendFailures uint64
	lat                                   latRing
}

func newProc(p *Pool, k Key, label, id string) *Proc {
	return &Proc{
		pool:    p,
		key:     k,
		label:   label,
		id:      id,
		created: time.Now(),
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
		pending: map[pkey]pend{},
	}
}

func (pr *Proc) logf(format string, args ...any) {
	pr.pool.opt.Logf("pool: "+pr.id+": "+format, args...)
}

func (pr *Proc) started(inst backend.Instance) {
	pr.mu.Lock()
	pr.inst = inst
	pr.starts++
	pr.lastStart = time.Now()
	pr.pid = inst.PID()
	pr.mu.Unlock()
}

// attach makes a the client. It returns the old client, and false when
// the process is stopping.
func (pr *Proc) attach(a *Attachment) (*Attachment, bool) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.stopped {
		return nil, false
	}
	old := pr.att
	pr.att = a
	if pr.idleTimer != nil {
		pr.idleTimer.Stop()
		pr.idleTimer = nil
	}
	return old, true
}

func (pr *Proc) idle() (time.Time, bool) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.idleSince, pr.att == nil && !pr.stopped
}

func (pr *Proc) detachAll() *Attachment {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	a := pr.att
	pr.att = nil
	return a
}

func (pr *Proc) detach(a *Attachment, stopNow bool) {
	pr.mu.Lock()
	if pr.att != a {
		pr.mu.Unlock()
		return
	}
	pr.att = nil
	pr.idleSince = time.Now()
	if !stopNow && pr.pool.opt.IdleTimeout > 0 && !pr.stopped {
		pr.idleTimer = time.AfterFunc(pr.pool.opt.IdleTimeout, pr.idleExpired)
		pr.mu.Unlock()
		pr.logf("client left, idle for up to %s", pr.pool.opt.IdleTimeout)
		return
	}
	pr.mu.Unlock()
	pr.logf("client left, stopping")
	pr.pool.remove(pr)
	pr.stop()
}

func (pr *Proc) idleExpired() {
	pr.mu.Lock()
	expired := pr.att == nil && !pr.stopped
	pr.mu.Unlock()
	if !expired {
		return
	}
	pr.logf("idle timeout, stopping")
	pr.pool.remove(pr)
	pr.stop()
}

// stop ends the process; it does not wait.
func (pr *Proc) stop() {
	pr.mu.Lock()
	if pr.stopped {
		pr.mu.Unlock()
		return
	}
	pr.stopped = true
	close(pr.stopCh)
	if pr.idleTimer != nil {
		pr.idleTimer.Stop()
	}
	inst := pr.inst
	pr.mu.Unlock()
	if inst != nil {
		go inst.Stop() // Stop can wait up to 2 s for the process
	}
}

func (pr *Proc) stopWait() {
	pr.mu.Lock()
	inst := pr.inst
	pr.mu.Unlock()
	pr.stop()
	if inst != nil {
		inst.Stop() // safe to call again; waits for the end
	}
}

func (pr *Proc) send(a *Attachment, f *backend.Frame, want codec.Set) error {
	pr.mu.Lock()
	if pr.att != a {
		pr.mu.Unlock()
		return ErrDetached
	}
	inst := pr.inst
	if inst == nil || pr.stopped {
		pr.dropped++
		pr.mu.Unlock()
		return ErrNotReady
	}
	if len(pr.pending) >= pr.pool.opt.MaxPending {
		pr.dropped++
		pr.mu.Unlock()
		return ErrBacklog
	}
	k := pkey{f.StreamID, f.Seq}
	pr.pending[k] = pend{sent: time.Now(), want: want, in: f.In}
	pr.framesIn++
	pr.mu.Unlock()
	if err := inst.Send(f); err != nil {
		// The read loop sees the broken link and restarts the process.
		pr.mu.Lock()
		delete(pr.pending, k)
		pr.sendFailures++
		pr.mu.Unlock()
		return ErrNotReady
	}
	return nil
}

// errUnknownReply is a desync: tcd answered a frame that was not sent.
var errUnknownReply = fmt.Errorf("%w: reply for a frame that was not sent", backend.ErrDesync)

// readLoop delivers replies until the instance fails.
func (pr *Proc) readLoop(inst backend.Instance) error {
	sweep := time.NewTicker(250 * time.Millisecond)
	defer sweep.Stop()
	type res struct {
		f   *backend.Frame
		err error
	}
	ch := make(chan res)
	quit := make(chan struct{})
	defer close(quit)
	go func() {
		for {
			f, err := inst.Recv()
			select {
			case ch <- res{f, err}:
			case <-quit:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-pr.stopCh:
			return nil
		case <-sweep.C:
			pr.sweep()
		case r := <-ch:
			if r.err != nil {
				return r.err
			}
			if err := pr.deliver(r.f); err != nil {
				return err
			}
		}
	}
}

// sweep counts frames with no reply as missing.
func (pr *Proc) sweep() {
	now := time.Now()
	pr.mu.Lock()
	defer pr.mu.Unlock()
	for k, v := range pr.pending {
		age := now.Sub(v.sent)
		if !v.missing && age > pr.pool.opt.MissingAfter {
			v.missing = true
			pr.pending[k] = v
			pr.missing++
		}
		// Keep the key for a while, so a very late reply is not taken
		// for a desync.
		if age > 15*pr.pool.opt.MissingAfter {
			delete(pr.pending, k)
		}
	}
}

func (pr *Proc) deliver(f *backend.Frame) error {
	now := time.Now()
	pr.mu.Lock()
	k := pkey{f.StreamID, f.Seq}
	v, ok := pr.pending[k]
	if !ok || v.in != f.In {
		pr.unknownReplies++
		pr.mu.Unlock()
		return errUnknownReply
	}
	delete(pr.pending, k)
	pr.failures = 0
	lat := now.Sub(v.sent)
	late := lat > pr.pool.opt.LateAfter
	pr.lat.add(lat)
	if late {
		pr.late++
	}
	a := pr.att
	if v.missing || a == nil {
		pr.dropped++
		pr.mu.Unlock()
		return nil
	}
	pr.framesOut++
	pr.mu.Unlock()
	a.sink.Reply(Delivered{Frame: f, Want: v.want, Latency: lat, Late: late})
	return nil
}

func (pr *Proc) backoff() time.Duration {
	o := pr.pool.opt
	d := o.MinBackoff
	for i := 1; i < pr.failures && d < o.MaxBackoff; i++ {
		d *= 2
	}
	return min(d, o.MaxBackoff)
}

// supervise watches the instance and restarts it when it fails.
func (pr *Proc) supervise(inst backend.Instance) {
	defer close(pr.done)
	for {
		err := pr.readLoop(inst)
		inst.Stop()
		pr.mu.Lock()
		pr.inst = nil
		for _, v := range pr.pending {
			if !v.missing {
				pr.missing++
			}
		}
		clear(pr.pending)
		if pr.stopped {
			pr.mu.Unlock()
			return
		}
		switch {
		case errors.Is(err, backend.ErrExited):
			pr.crashes++
		case errors.Is(err, backend.ErrDesync):
			pr.desyncs++
		case errors.Is(err, backend.ErrStalled):
			pr.stalls++
		default:
			pr.crashes++
		}
		pr.mu.Unlock()
		pr.logf("failed: %v", err)
		reason := err.Error()

		for {
			pr.mu.Lock()
			pr.failures++
			fails := pr.failures
			a := pr.att
			pr.mu.Unlock()
			if fails > pr.pool.opt.MaxFailures {
				pr.logf("gave up after %d failures in a row", fails-1)
				pr.pool.remove(pr)
				if a := pr.detachAll(); a != nil {
					a.sink.Failed("backend_failed", fmt.Sprintf("the transcoder failed %d times in a row: %s", fails-1, reason))
				}
				pr.stop()
				return
			}
			wait := pr.backoff()
			if a != nil {
				a.sink.Restarting(reason, wait)
			}
			select {
			case <-pr.stopCh:
				return
			case <-time.After(wait):
			}
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				select {
				case <-pr.stopCh:
					cancel()
				case <-ctx.Done():
				}
			}()
			next, err := pr.pool.opt.Backend.Start(ctx, pr.id)
			cancel()
			if err != nil {
				pr.logf("restart failed: %v", err)
				reason = err.Error()
				continue
			}
			pr.mu.Lock()
			if pr.stopped {
				pr.mu.Unlock()
				next.Stop()
				return
			}
			pr.mu.Unlock()
			pr.started(next)
			pr.logf("restarted, pid %d", next.PID())
			pr.mu.Lock()
			a = pr.att
			pr.mu.Unlock()
			if a != nil {
				a.sink.Ready(true)
			}
			inst = next
			break
		}
	}
}

// LatencyStats summarizes the transcode latency.
type LatencyStats struct {
	Count uint64  `json:"count"`
	MinMS float64 `json:"min_ms"`
	AvgMS float64 `json:"avg_ms"`
	P50MS float64 `json:"p50_ms"`
	P99MS float64 `json:"p99_ms"`
	MaxMS float64 `json:"max_ms"`
}

// LinkStats are the counters of the transcoder link.
type LinkStats struct {
	Pings     uint64 `json:"pings"`
	BadFrames uint64 `json:"bad_frames"`
	SkipBytes uint64 `json:"skip_bytes"`
}

// ProcStats are the stats of one process.
type ProcStats struct {
	ID             string       `json:"id"`
	KeyID          string       `json:"key_id"`
	Label          string       `json:"label,omitempty"`
	Session        string       `json:"session"`
	Backend        string       `json:"backend"`
	PID            int          `json:"pid"`
	State          string       `json:"state"` // running, idle, restarting, stopped
	AgeS           float64      `json:"age_s"`
	UptimeS        float64      `json:"uptime_s"` // since the last start
	CPUSeconds     float64      `json:"cpu_seconds"`
	Starts         uint64       `json:"starts"`
	Crashes        uint64       `json:"crashes"`
	Desyncs        uint64       `json:"desyncs"`
	Stalls         uint64       `json:"stalls"`
	FramesIn       uint64       `json:"frames_in"`
	FramesOut      uint64       `json:"frames_out"`
	Late           uint64       `json:"late"`
	Missing        uint64       `json:"missing"`
	Dropped        uint64       `json:"dropped"`
	UnknownReplies uint64       `json:"unknown_replies"`
	SendFailures   uint64       `json:"send_failures"`
	Pending        int          `json:"pending"`
	Latency        LatencyStats `json:"latency"`
	Link           LinkStats    `json:"link"`
}

// Stats returns the stats of the process.
func (pr *Proc) Stats() ProcStats {
	pr.mu.Lock()
	s := ProcStats{
		ID:             pr.id,
		KeyID:          pr.key.KeyID,
		Label:          pr.label,
		Session:        pr.key.Session,
		Backend:        pr.pool.opt.Backend.Name(),
		PID:            pr.pid,
		AgeS:           time.Since(pr.created).Seconds(),
		Starts:         pr.starts,
		Crashes:        pr.crashes,
		Desyncs:        pr.desyncs,
		Stalls:         pr.stalls,
		FramesIn:       pr.framesIn,
		FramesOut:      pr.framesOut,
		Late:           pr.late,
		Missing:        pr.missing,
		Dropped:        pr.dropped,
		UnknownReplies: pr.unknownReplies,
		SendFailures:   pr.sendFailures,
		Pending:        len(pr.pending),
		Latency:        pr.lat.stats(),
	}
	inst := pr.inst
	switch {
	case pr.stopped:
		s.State = "stopped"
	case inst == nil:
		s.State = "restarting"
	case pr.att == nil:
		s.State = "idle"
	default:
		s.State = "running"
	}
	if inst != nil {
		s.UptimeS = time.Since(pr.lastStart).Seconds()
	}
	pr.mu.Unlock()
	if inst != nil {
		ls := inst.LinkStats()
		s.Link = LinkStats{Pings: ls.Pings, BadFrames: ls.BadFrames, SkipBytes: ls.SkipBytes}
		s.CPUSeconds = cpuSeconds(s.PID)
	}
	return s
}

// latRing keeps the last latencies for percentiles.
type latRing struct {
	buf   [2048]time.Duration
	n     uint64
	sum   time.Duration
	min   time.Duration
	max   time.Duration
	count uint64
}

func (r *latRing) add(d time.Duration) {
	r.buf[r.n%uint64(len(r.buf))] = d
	r.n++
	r.count++
	r.sum += d
	if r.count == 1 || d < r.min {
		r.min = d
	}
	if d > r.max {
		r.max = d
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func (r *latRing) stats() LatencyStats {
	if r.count == 0 {
		return LatencyStats{}
	}
	n := min(r.n, uint64(len(r.buf)))
	s := slices.Clone(r.buf[:n])
	slices.Sort(s)
	pct := func(p float64) time.Duration { return s[min(int(float64(len(s))*p), len(s)-1)] }
	return LatencyStats{
		Count: r.count,
		MinMS: ms(r.min),
		AvgMS: ms(r.sum / time.Duration(r.count)),
		P50MS: ms(pct(0.50)),
		P99MS: ms(pct(0.99)),
		MaxMS: ms(r.max),
	}
}
