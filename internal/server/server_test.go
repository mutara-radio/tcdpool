package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dbehnke/tcdpool/internal/backend/nostar"
	"github.com/dbehnke/tcdpool/internal/codec"
	"github.com/dbehnke/tcdpool/internal/faketcd"
	"github.com/dbehnke/tcdpool/internal/keys"
	"github.com/dbehnke/tcdpool/internal/pool"
	"github.com/dbehnke/tcdpool/internal/protocol"
)

func TestMain(m *testing.M) {
	if os.Getenv("TCDPOOL_FAKETCD") == "1" {
		os.Exit(faketcd.Main(os.Args[len(os.Args)-1:]))
	}
	os.Exit(m.Run())
}

type env struct {
	ts       *httptest.Server
	srv      *Server
	pool     *pool.Pool
	keysPath string
	token    string
	keyID    string
}

func setup(t *testing.T, keyMax int) *env {
	t.Helper()
	dir := t.TempDir()
	nb, err := nostar.New(nostar.Options{
		Binary: os.Args[0], WorkDir: dir, Framing: "v2",
		ConnectTimeout: 5 * time.Second, Watchdog: 2 * time.Second,
		Env:  []string{"TCDPOOL_FAKETCD=1", "TCDPOOL_FAKE_PING_MS=500"},
		Logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := pool.New(pool.Options{Backend: nb, IdleTimeout: time.Minute, MaxProcesses: 4, Logf: t.Logf})
	t.Cleanup(p.Close)
	kp := filepath.Join(dir, "keys.json")
	k, tok, err := keys.Create(kp, "N0CALL test", keyMax)
	if err != nil {
		t.Fatal(err)
	}
	ks, err := keys.Open(kp)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{Pool: p, Keys: ks, Backend: nb, KeyMaxProcesses: 2, IdleTimeout: time.Minute, LateAfter: 60 * time.Millisecond, Logf: t.Logf})
	ts := httptest.NewTLSServer(s.Handler())
	t.Cleanup(ts.Close)
	return &env{ts: ts, srv: s, pool: p, keysPath: kp, token: tok, keyID: k.ID}
}

func (e *env) dial(t *testing.T, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	c, resp, err := websocket.Dial(ctx, "wss"+e.ts.URL[5:]+"/v1/stream", &websocket.DialOptions{HTTPClient: e.ts.Client(), HTTPHeader: h})
	if c != nil {
		t.Cleanup(func() { c.CloseNow() })
	}
	return c, resp, err
}

func writeJSON(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// readMsg returns the next text message as a map, or a binary message.
func readMsg(t *testing.T, c *websocket.Conn) (map[string]any, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ == websocket.MessageBinary {
		return nil, b
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	return m, nil
}

func hello(t *testing.T, c *websocket.Conn, session string, outputs ...string) map[string]any {
	t.Helper()
	writeJSON(t, c, protocol.Hello{Type: "hello", Version: 1, Session: session, Outputs: outputs, Client: "test"})
	m, _ := readMsg(t, c)
	return m
}

func TestAuth(t *testing.T) {
	e := setup(t, 2)
	for name, tok := range map[string]string{"none": "", "wrong": e.keyID + ".nope", "unknown": "k000000000000.x"} {
		_, resp, err := e.dial(t, tok)
		if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: %v %v", name, err, resp)
		}
	}
	keys.SetEnabled(e.keysPath, e.keyID, false)
	os.Chtimes(e.keysPath, time.Now().Add(time.Second), time.Now().Add(time.Second))
	e.srv.opt.Keys.Reload()
	_, resp, err := e.dial(t, e.token)
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("disabled: %v %v", err, resp)
	}
}

func TestHelloErrors(t *testing.T) {
	e := setup(t, 2)
	cases := map[string]struct {
		msg  any
		code string
	}{
		"version": {protocol.Hello{Type: "hello", Version: 9, Session: "a"}, protocol.ErrUnsupportedVersion},
		"session": {protocol.Hello{Type: "hello", Version: 1, Session: "bad name!"}, protocol.ErrBadHello},
		"codec":   {protocol.Hello{Type: "hello", Version: 1, Session: "a", Outputs: []string{"mp3"}}, protocol.ErrUnsupportedCodec},
		"type":    {map[string]any{"type": "stats"}, protocol.ErrBadHello},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			conn, _, err := e.dial(t, e.token)
			if err != nil {
				t.Fatal(err)
			}
			writeJSON(t, conn, c.msg)
			m, _ := readMsg(t, conn)
			if m["type"] != "error" || m["code"] != c.code || m["fatal"] != true {
				t.Fatalf("got %v", m)
			}
		})
	}
}

func request(seq uint32, in codec.ID, outputs codec.Set) []byte {
	return protocol.AppendRequest(nil, &protocol.Request{StreamID: 0x2101, Seq: seq, In: in, Outputs: outputs, Payload: make([]byte, in.Size())})
}

func TestStream(t *testing.T) {
	e := setup(t, 2)
	c, _, err := e.dial(t, e.token)
	if err != nil {
		t.Fatal(err)
	}
	m := hello(t, c, "node-2100", "dmr", "pcm")
	if m["type"] != "ready" || m["reused"] != false || m["frame_ms"] != 20.0 {
		t.Fatalf("ready: %v", m)
	}
	ctx := context.Background()
	// explicit outputs
	c.Write(ctx, websocket.MessageBinary, request(1, codec.PCM, codec.SetOf(codec.DStar, codec.C2)))
	_, b := readMsg(t, c)
	r, err := protocol.DecodeReply(b)
	if err != nil || r.Seq != 1 || r.StreamID != 0x2101 || r.Outputs != codec.SetOf(codec.DStar, codec.C2) || r.LatencyMicros == 0 {
		t.Fatalf("reply %v %+v", err, r)
	}
	// mask 0: the session default, without the input codec
	c.Write(ctx, websocket.MessageBinary, request(2, codec.PCM, 0))
	_, b = readMsg(t, c)
	if r, _ := protocol.DecodeReply(b); r.Outputs != codec.SetOf(codec.DMR) {
		t.Fatalf("default outputs %v", r.Outputs.Names())
	}
	// only the input asked for: a reply with no payload
	c.Write(ctx, websocket.MessageBinary, request(3, codec.DMR, codec.SetOf(codec.DMR)))
	_, b = readMsg(t, c)
	if r, _ := protocol.DecodeReply(b); r.Outputs != 0 || r.Seq != 3 {
		t.Fatalf("input-only reply %+v", r)
	}
	// a bad frame gives a non-fatal error
	c.Write(ctx, websocket.MessageBinary, []byte{1, 1, 0})
	if m, _ := readMsg(t, c); m["code"] != protocol.ErrBadFrameCode || m["fatal"] != false {
		t.Fatalf("bad frame: %v", m)
	}
	writeJSON(t, c, map[string]string{"type": "stats"})
	m, _ = readMsg(t, c)
	if m["type"] != "stats" || m["frames_out"] != 3.0 || m["state"] != "running" {
		t.Fatalf("stats %v", m)
	}
	// reconnect: same process
	c.Close(websocket.StatusNormalClosure, "")
	time.Sleep(200 * time.Millisecond)
	c2, _, _ := e.dial(t, e.token)
	if m := hello(t, c2, "node-2100"); m["reused"] != true {
		t.Fatalf("reconnect: %v", m)
	}
	// bye with stop ends the process
	writeJSON(t, c2, protocol.Bye{Type: "bye", Stop: true})
	time.Sleep(500 * time.Millisecond)
	if e.pool.Count() != 0 {
		t.Fatal("bye stop did not stop the process")
	}
}

func TestLimitsAndRevoke(t *testing.T) {
	e := setup(t, 1)
	c1, _, _ := e.dial(t, e.token)
	if m := hello(t, c1, "node-2200"); m["type"] != "ready" {
		t.Fatal(m)
	}
	c2, _, _ := e.dial(t, e.token)
	if m := hello(t, c2, "node-2201"); m["code"] != protocol.ErrKeyLimit {
		t.Fatalf("second session: %v", m)
	}
	// a second connection to the same session replaces the first
	c3, _, _ := e.dial(t, e.token)
	if m := hello(t, c3, "node-2200"); m["reused"] != true {
		t.Fatal(m)
	}
	if m, _ := readMsg(t, c1); m["code"] != protocol.ErrReplaced {
		t.Fatalf("old connection: %v", m)
	}
	// disable the key: the session ends
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.srv.WatchKeys(ctx, 50*time.Millisecond)
	keys.SetEnabled(e.keysPath, e.keyID, false)
	os.Chtimes(e.keysPath, time.Now().Add(time.Second), time.Now().Add(time.Second))
	if m, _ := readMsg(t, c3); m["code"] != protocol.ErrKeyRevoked {
		t.Fatalf("revoked: %v", m)
	}
}

func TestAdmin(t *testing.T) {
	e := setup(t, 2)
	c, _, _ := e.dial(t, e.token)
	hello(t, c, "node-2350")
	h := e.srv.AdminHandler(4)
	for _, path := range []string{"/stats", "/metrics", "/healthz"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || rec.Body.Len() == 0 {
			t.Errorf("%s: %d", path, rec.Code)
		}
		if path == "/stats" {
			var v struct {
				Count int `json:"count"`
			}
			json.Unmarshal(rec.Body.Bytes(), &v)
			if v.Count != 1 {
				t.Errorf("stats count %d", v.Count)
			}
		}
	}
}
