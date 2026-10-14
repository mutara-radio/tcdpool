// Package server is the tcdpool WebSocket server and the loopback admin
// endpoint.
package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/dbehnke/tcdpool/internal/backend"
	"github.com/dbehnke/tcdpool/internal/codec"
	"github.com/dbehnke/tcdpool/internal/keys"
	"github.com/dbehnke/tcdpool/internal/pool"
	"github.com/dbehnke/tcdpool/internal/protocol"
)

// Options configure a server.
type Options struct {
	Pool            *pool.Pool
	Keys            *keys.Store
	Backend         backend.Backend
	KeyMaxProcesses int
	IdleTimeout     time.Duration
	LateAfter       time.Duration
	Logf            func(format string, args ...any)
}

// Server serves clients.
type Server struct {
	opt      Options
	sessions atomic.Int64
}

// New makes a server.
func New(opt Options) *Server {
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	return &Server{opt: opt}
}

// Handler returns the client handler: /v1/stream and /healthz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/stream", s.stream)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	return mux
}

func httpError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(protocol.Error{Type: protocol.MsgError, Code: code, Message: msg, Fatal: true})
}

var sessionRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	key, err := s.opt.Keys.Verify(r.Header.Get("Authorization"))
	switch {
	case errors.Is(err, keys.ErrDisabled):
		s.opt.Logf("server: %s: refused, key %s is disabled", r.RemoteAddr, key.ID)
		httpError(w, http.StatusForbidden, protocol.ErrKeyDisabled, "the key is disabled")
		return
	case err != nil:
		s.opt.Logf("server: %s: refused, bad or unknown key", r.RemoteAddr)
		httpError(w, http.StatusUnauthorized, protocol.ErrUnauthorized, "send Authorization: Bearer <key id>.<secret>")
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		s.opt.Logf("server: %s: websocket: %v", r.RemoteAddr, err)
		return
	}
	c.SetReadLimit(4096)
	sess := &session{srv: s, c: c, key: key, remote: r.RemoteAddr, out: make(chan outMsg, 512), done: make(chan struct{})}
	s.sessions.Add(1)
	defer s.sessions.Add(-1)
	sess.run(r.Context())
}

type outMsg struct {
	typ   websocket.MessageType
	data  []byte
	close bool // close the connection after this message
	code  websocket.StatusCode
}

type session struct {
	srv    *Server
	c      *websocket.Conn
	key    keys.Key
	remote string
	name   string
	def    codec.Set // default outputs from hello
	att    *pool.Attachment

	out       chan outMsg
	done      chan struct{} // closed when the writer ends
	closeOnce sync.Once
	closing   atomic.Bool
	dropped   atomic.Uint64
}

func (ss *session) logf(format string, args ...any) {
	ss.srv.opt.Logf("server: %s %s/%s: "+format, append([]any{ss.remote, ss.key.ID, ss.name}, args...)...)
}

// queue puts a message on the writer queue. A voice frame is dropped
// when the queue is full; a control message waits a short time.
func (ss *session) queue(m outMsg) {
	if ss.closing.Load() {
		return
	}
	if m.typ == websocket.MessageBinary {
		select {
		case ss.out <- m:
		default:
			ss.dropped.Add(1)
		}
		return
	}
	select {
	case ss.out <- m:
	case <-time.After(time.Second):
	case <-ss.done:
	}
}

func (ss *session) sendJSON(v any) {
	b, _ := json.Marshal(v)
	ss.queue(outMsg{typ: websocket.MessageText, data: b})
}

func (ss *session) fail(code, msg string, status websocket.StatusCode) {
	b, _ := json.Marshal(protocol.Error{Type: protocol.MsgError, Code: code, Message: msg, Fatal: true})
	ss.queue(outMsg{typ: websocket.MessageText, data: b, close: true, code: status})
	ss.closing.Store(true)
}

func (ss *session) writer(ctx context.Context) {
	defer close(ss.done)
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-ss.out:
			wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := ss.c.Write(wctx, m.typ, m.data)
			cancel()
			if err != nil {
				ss.c.CloseNow()
				return
			}
			if m.close {
				ss.c.Close(m.code, "")
				return
			}
		}
	}
}

// pool.Sink

func (ss *session) Reply(d pool.Delivered) {
	f := d.Frame
	r := protocol.Reply{
		StreamID:      f.StreamID,
		Seq:           f.Seq,
		Last:          f.Last,
		Late:          d.Late,
		In:            f.In,
		Outputs:       f.Have & d.Want,
		LatencyMicros: uint32(d.Latency.Microseconds()),
	}
	var pcm [codec.FrameSamples * 2]byte
	for _, id := range r.Outputs.IDs() {
		if id == codec.PCM {
			for i, v := range f.PCM {
				binary.LittleEndian.PutUint16(pcm[2*i:], uint16(v))
			}
			r.Payloads[id] = pcm[:]
		} else {
			r.Payloads[id] = f.Payload(id)
		}
	}
	ss.queue(outMsg{typ: websocket.MessageBinary, data: protocol.AppendReply(make([]byte, 0, protocol.ReplyHeaderSize+r.Outputs.PayloadSize()), &r)})
}

func (ss *session) Restarting(reason string, retryIn time.Duration) {
	ss.sendJSON(protocol.Restarting{Type: protocol.MsgRestarting, Reason: reason, RetryInMS: int(retryIn.Milliseconds())})
}

func (ss *session) Ready(restarted bool) { ss.sendJSON(ss.ready(true, restarted)) }

func (ss *session) Failed(code, message string) {
	status := websocket.StatusInternalError
	switch code {
	case protocol.ErrReplaced, protocol.ErrKeyRevoked:
		status = websocket.StatusPolicyViolation
	case protocol.ErrShuttingDown:
		status = websocket.StatusGoingAway
	}
	ss.fail(code, message, status)
}

func (ss *session) ready(reused, restarted bool) protocol.Ready {
	caps := ss.srv.opt.Backend.Capabilities()
	return protocol.Ready{
		Type:         protocol.MsgReady,
		Version:      protocol.Version,
		Session:      ss.name,
		Reused:       reused,
		Restarted:    restarted,
		Backend:      ss.srv.opt.Backend.Name(),
		Inputs:       caps.Inputs.Names(),
		Outputs:      ss.def.Names(),
		FrameMS:      20,
		IdleTimeoutS: int(ss.srv.opt.IdleTimeout.Seconds()),
		LateAfterMS:  int(ss.srv.opt.LateAfter.Milliseconds()),
	}
}

// hello reads and checks the hello message.
func (ss *session) hello(ctx context.Context) (protocol.Hello, string, string) {
	var h protocol.Hello
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	typ, b, err := ss.c.Read(hctx)
	if err != nil {
		return h, protocol.ErrBadHello, "no hello in 5 s"
	}
	if typ != websocket.MessageText || json.Unmarshal(b, &h) != nil || h.Type != protocol.MsgHello {
		return h, protocol.ErrBadHello, "the first message must be a JSON hello"
	}
	if h.Version != protocol.Version {
		return h, protocol.ErrUnsupportedVersion, fmt.Sprintf("protocol version %d is not supported; use %d", h.Version, protocol.Version)
	}
	if !sessionRE.MatchString(h.Session) {
		return h, protocol.ErrBadHello, "session must be 1-64 characters of A-Z a-z 0-9 . _ : -"
	}
	set, err := codec.ParseSet(h.Outputs)
	if err != nil {
		return h, protocol.ErrUnsupportedCodec, err.Error()
	}
	if caps := ss.srv.opt.Backend.Capabilities(); set&^caps.Outputs != 0 {
		return h, protocol.ErrUnsupportedCodec, fmt.Sprintf("outputs %v are not supported", (set &^ caps.Outputs).Names())
	}
	if set == 0 {
		set = ss.srv.opt.Backend.Capabilities().Outputs
	}
	ss.def = set
	return h, "", ""
}

func (ss *session) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go ss.writer(ctx)
	defer func() {
		// let the writer send a last error message
		select {
		case <-ss.done:
		case <-time.After(2 * time.Second):
		}
		ss.c.CloseNow()
	}()

	h, code, msg := ss.hello(ctx)
	if code != "" {
		ss.logf("refused: %s", msg)
		ss.fail(code, msg, websocket.StatusPolicyViolation)
		return
	}
	ss.name = h.Session
	keyMax := ss.key.MaxProcesses
	if keyMax == 0 {
		keyMax = ss.srv.opt.KeyMaxProcesses
	}
	att, reused, err := ss.srv.opt.Pool.Attach(ctx, pool.Key{KeyID: ss.key.ID, Session: h.Session}, keyMax, ss.key.Label, ss)
	if err != nil {
		code, status := protocol.ErrBackendStart, websocket.StatusInternalError
		switch {
		case errors.Is(err, pool.ErrKeyLimit):
			code, status = protocol.ErrKeyLimit, websocket.StatusPolicyViolation
			err = fmt.Errorf("%w (%d)", err, keyMax)
		case errors.Is(err, pool.ErrHostFull):
			code, status = protocol.ErrHostFull, websocket.StatusTryAgainLater
		case errors.Is(err, pool.ErrShuttingDown):
			code, status = protocol.ErrShuttingDown, websocket.StatusGoingAway
		}
		ss.logf("refused: %v", err)
		ss.fail(code, err.Error(), status)
		return
	}
	ss.att = att
	ss.logf("attached to %s (reused=%v, client %q)", att.ID(), reused, h.Client)
	ss.sendJSON(ss.ready(reused, false))

	stop := ss.readLoop(ctx)
	att.Detach(stop)
	ss.logf("detached (stop=%v, %d replies dropped in the send queue)", stop, ss.dropped.Load())
}

// readLoop handles client messages. It returns true when the client
// asked to stop the process.
func (ss *session) readLoop(ctx context.Context) bool {
	caps := ss.srv.opt.Backend.Capabilities()
	badFrames := 0
	for {
		typ, b, err := ss.c.Read(ctx)
		if err != nil || ss.closing.Load() {
			return false
		}
		if typ == websocket.MessageText {
			var m struct {
				Type string `json:"type"`
				Stop bool   `json:"stop"`
			}
			if json.Unmarshal(b, &m) != nil {
				continue
			}
			switch m.Type {
			case protocol.MsgStats:
				st := ss.att.Stats()
				ss.sendJSON(struct {
					Type string `json:"type"`
					pool.ProcStats
					QueueDropped uint64 `json:"queue_dropped"`
				}{protocol.MsgStats, st, ss.dropped.Load()})
			case protocol.MsgBye:
				return m.Stop
			}
			continue
		}
		req, err := protocol.DecodeRequest(b)
		if err == nil && !caps.Inputs.Has(req.In) {
			err = fmt.Errorf("input codec %v is not supported", req.In)
		}
		if err == nil && req.Outputs&^caps.Outputs != 0 {
			err = fmt.Errorf("outputs %v are not supported", (req.Outputs &^ caps.Outputs).Names())
		}
		if err != nil {
			// tell the client about the first bad frames, then be quiet
			if badFrames++; badFrames <= 3 {
				ss.sendJSON(protocol.Error{Type: protocol.MsgError, Code: protocol.ErrBadFrameCode, Message: err.Error()})
			}
			continue
		}
		want := req.Outputs
		if want == 0 {
			want = ss.def
		}
		want = want.Without(req.In)
		f := &backend.Frame{StreamID: req.StreamID, Seq: req.Seq, Last: req.Last, In: req.In, Want: want}
		if want == 0 {
			// Only the input was asked for: no transcoding. tcd reads an
			// empty mask as all codecs, so ask for the input only.
			f.Want = codec.SetOf(req.In)
		}
		if req.In == codec.PCM {
			for i := range f.PCM {
				f.PCM[i] = int16(binary.LittleEndian.Uint16(req.Payload[2*i:]))
			}
		} else {
			copy(f.Payload(req.In), req.Payload)
		}
		switch err := ss.att.Send(f, want); {
		case errors.Is(err, pool.ErrDetached):
			return false
		case errors.Is(err, pool.ErrDuplicate):
			if badFrames++; badFrames <= 3 {
				ss.sendJSON(protocol.Error{Type: protocol.MsgError, Code: protocol.ErrBadFrameCode,
					Message: fmt.Sprintf("stream %#04x seq %d: %v", req.StreamID, req.Seq, err)})
			}
		case errors.Is(err, pool.ErrBacklog):
			if badFrames++; badFrames <= 3 {
				ss.sendJSON(protocol.Error{Type: protocol.MsgError, Code: protocol.ErrBacklog, Message: err.Error()})
			}
		}
	}
}

// Sessions returns the number of open client connections.
func (s *Server) Sessions() int64 { return s.sessions.Load() }

// WatchKeys reads the key file again when it changes, and stops the
// processes of keys that are now disabled or gone.
func (s *Server) WatchKeys(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		changed, err := s.opt.Keys.Reload()
		if err != nil {
			s.opt.Logf("server: keys file: %v (the old keys stay in use)", err)
			continue
		}
		if !changed {
			continue
		}
		s.opt.Logf("server: keys file changed, read again")
		seen := map[string]bool{}
		for _, st := range s.opt.Pool.Stats() {
			if seen[st.KeyID] {
				continue
			}
			seen[st.KeyID] = true
			if k, ok := s.opt.Keys.Get(st.KeyID); !ok || !k.Enabled {
				n := s.opt.Pool.StopKey(st.KeyID, protocol.ErrKeyRevoked, "the key was disabled or removed")
				s.opt.Logf("server: key %s disabled or removed, stopped %d processes", st.KeyID, n)
			}
		}
	}
}
