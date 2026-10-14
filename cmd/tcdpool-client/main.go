// Command tcdpool-client is a test client for tcdpool. It sends test
// signals as PCM, gets them back encoded, sends the encoded frames back
// and gets PCM again (PCM -> codec -> PCM). It paces frames at 20 ms,
// measures latency, counts late and missing frames, checks the audio
// and writes WAV files.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/dbehnke/tcdpool/internal/codec"
	"github.com/dbehnke/tcdpool/internal/protocol"
)

type options struct {
	url, key, pin, session, out string
	insecure, allOutputs, stop  bool
	codecs                      []codec.ID
	signals                     []string
	frames, parallel            int
	timeout                     time.Duration
	jsonOut                     bool
}

func main() {
	var o options
	var codecs, signals string
	flag.StringVar(&o.url, "url", "wss://127.0.0.1:8443/v1/stream", "server URL")
	flag.StringVar(&o.key, "key", os.Getenv("TCDPOOL_KEY"), "access token <id>.<secret> (default: $TCDPOOL_KEY)")
	flag.StringVar(&o.pin, "pin", "", "accept only a server certificate with this SHA-256 (hex)")
	flag.BoolVar(&o.insecure, "insecure", false, "do not check the server certificate (development only)")
	flag.StringVar(&o.session, "session", "client", "session name prefix")
	flag.StringVar(&o.out, "out", "", "write WAV files to this directory")
	flag.StringVar(&codecs, "codecs", "dmr,dstar,codec2_3200", "codecs for the round trip")
	flag.StringVar(&signals, "signals", "tone1k,tone440,speech", "test signals")
	flag.IntVar(&o.frames, "frames", 250, "frames per signal (20 ms each)")
	flag.IntVar(&o.parallel, "parallel", 1, "number of clients at the same time, each with its own session")
	flag.BoolVar(&o.allOutputs, "all-outputs", false, "ask for all outputs (mask 0, hello outputs empty) to compare with a single output")
	flag.BoolVar(&o.stop, "stop", false, "stop the tcd process at the end (bye with stop)")
	flag.DurationVar(&o.timeout, "timeout", 2*time.Second, "a frame with no reply after this time is missing")
	flag.BoolVar(&o.jsonOut, "json", false, "print the results as JSON")
	flag.Parse()
	if o.key == "" {
		fatal(errors.New("give -key or set TCDPOOL_KEY"))
	}
	for _, n := range strings.Split(codecs, ",") {
		id, err := codec.Parse(n)
		if err != nil {
			fatal(err)
		}
		if id == codec.PCM {
			fatal(errors.New("pcm is the reference, not a codec under test"))
		}
		o.codecs = append(o.codecs, id)
	}
	for _, s := range strings.Split(signals, ",") {
		if _, ok := signalFuncs[s]; !ok {
			fatal(fmt.Errorf("unknown signal %q (tone1k, tone440, speech)", s))
		}
		o.signals = append(o.signals, s)
	}
	if o.out != "" {
		if err := os.MkdirAll(o.out, 0o755); err != nil {
			fatal(err)
		}
	}

	results := make([]*workerResult, o.parallel)
	var wg sync.WaitGroup
	for i := range o.parallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = runWorker(&o, i)
		}()
	}
	wg.Wait()

	if o.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(results)
	} else {
		printResults(&o, results)
	}
	for _, r := range results {
		if r.Error != "" || !r.Pass {
			os.Exit(1)
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "tcdpool-client:", err)
	os.Exit(2)
}

// ---- results ----

type latency struct {
	N     int     `json:"n"`
	MinMS float64 `json:"min_ms"`
	AvgMS float64 `json:"avg_ms"`
	P50MS float64 `json:"p50_ms"`
	P99MS float64 `json:"p99_ms"`
	MaxMS float64 `json:"max_ms"`
}

func latOf(d []time.Duration) latency {
	if len(d) == 0 {
		return latency{}
	}
	s := slices.Clone(d)
	slices.Sort(s)
	var sum time.Duration
	for _, v := range s {
		sum += v
	}
	ms := func(v time.Duration) float64 { return float64(v.Microseconds()) / 1000 }
	p := func(q float64) time.Duration { return s[min(len(s)-1, int(q*float64(len(s))))] }
	return latency{len(s), ms(s[0]), ms(sum / time.Duration(len(s))), ms(p(0.5)), ms(p(0.99)), ms(s[len(s)-1])}
}

type stageResult struct {
	Name       string  `json:"name"` // e.g. pcm->dmr
	Sent       int     `json:"sent"`
	Replies    int     `json:"replies"`
	Missing    int     `json:"missing"`
	Late       int     `json:"late"`        // flagged late by the server
	ClientLate int     `json:"client_late"` // client round trip over 60 ms
	RTT        latency `json:"rtt"`         // client: send to reply
	Server     latency `json:"server"`      // server: time in tcd
	CPUSeconds float64 `json:"tcd_cpu_seconds"`
	Outputs    string  `json:"outputs"` // what the reply carried
}

type roundTrip struct {
	Codec  string      `json:"codec"`
	Signal string      `json:"signal"`
	Encode stageResult `json:"encode"`
	Decode stageResult `json:"decode"`
	Audio  audioCheck  `json:"audio"`
	WAVs   []string    `json:"wavs,omitempty"`
}

type workerResult struct {
	Worker  int            `json:"worker"`
	Session string         `json:"session"`
	PID     int            `json:"pid"`
	Reused  bool           `json:"reused"`
	Error   string         `json:"error,omitempty"`
	Events  []string       `json:"events,omitempty"`
	Trips   []*roundTrip   `json:"round_trips"`
	Stats   map[string]any `json:"final_stats,omitempty"`
	Pass    bool           `json:"pass"`
}

// ---- connection ----

type conn struct {
	c      *websocket.Conn
	mu     sync.Mutex
	wait   map[uint64]chan arrival
	stats  chan map[string]any
	events chan string
	closed chan struct{}
	err    error
}

type arrival struct {
	r  protocol.Reply
	at time.Time
}

func pkey(sid uint16, seq uint32) uint64 { return uint64(sid)<<32 | uint64(seq) }

func dial(o *options, session string, outputs []string) (*conn, protocol.Ready, error) {
	var ready protocol.Ready
	tc := &tls.Config{InsecureSkipVerify: o.insecure || o.pin != ""}
	if o.pin != "" {
		want := strings.ToLower(strings.ReplaceAll(o.pin, ":", ""))
		tc.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no certificate")
			}
			h := sha256.Sum256(raw[0])
			if hex.EncodeToString(h[:]) != want {
				return errors.New("the server certificate does not match -pin")
			}
			return nil
		}
	}
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tc}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, o.url, &websocket.DialOptions{
		HTTPClient: hc,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + o.key}},
	})
	if err != nil {
		if resp != nil {
			return nil, ready, fmt.Errorf("connect: %s: %w", resp.Status, err)
		}
		return nil, ready, fmt.Errorf("connect: %w", err)
	}
	c.SetReadLimit(1 << 16)
	h, _ := json.Marshal(protocol.Hello{Type: protocol.MsgHello, Version: protocol.Version, Session: session, Outputs: outputs, Client: "tcdpool-client"})
	if err := c.Write(ctx, websocket.MessageText, h); err != nil {
		c.CloseNow()
		return nil, ready, err
	}
	// the start of tcd can take some seconds
	_, b, err := c.Read(ctx)
	if err != nil {
		c.CloseNow()
		return nil, ready, fmt.Errorf("hello: %w", err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	if m["type"] != protocol.MsgReady {
		c.CloseNow()
		return nil, ready, fmt.Errorf("refused: %v: %v", m["code"], m["message"])
	}
	json.Unmarshal(b, &ready)
	cn := &conn{c: c, wait: map[uint64]chan arrival{}, stats: make(chan map[string]any, 1), events: make(chan string, 64), closed: make(chan struct{})}
	go cn.reader()
	return cn, ready, nil
}

func (cn *conn) reader() {
	defer close(cn.closed)
	for {
		typ, b, err := cn.c.Read(context.Background())
		if err != nil {
			cn.err = err
			return
		}
		at := time.Now()
		if typ == websocket.MessageBinary {
			r, err := protocol.DecodeReply(b)
			if err != nil {
				cn.event("bad reply: " + err.Error())
				continue
			}
			// the payloads point into b; b is not used again
			cn.mu.Lock()
			ch := cn.wait[pkey(r.StreamID, r.Seq)]
			delete(cn.wait, pkey(r.StreamID, r.Seq))
			cn.mu.Unlock()
			if ch != nil {
				ch <- arrival{r, at}
			} else {
				cn.event(fmt.Sprintf("reply with no request: stream %#x seq %d", r.StreamID, r.Seq))
			}
			continue
		}
		var m map[string]any
		json.Unmarshal(b, &m)
		switch m["type"] {
		case protocol.MsgStats:
			select {
			case cn.stats <- m:
			default:
			}
		default:
			cn.event(string(b))
		}
	}
}

func (cn *conn) event(s string) {
	select {
	case cn.events <- s:
	default:
	}
}

func (cn *conn) getStats() map[string]any {
	cn.c.Write(context.Background(), websocket.MessageText, []byte(`{"type":"stats"}`))
	select {
	case m := <-cn.stats:
		return m
	case <-time.After(3 * time.Second):
		return nil
	case <-cn.closed:
		return nil
	}
}

func cpu(m map[string]any) float64 {
	v, _ := m["cpu_seconds"].(float64)
	return v
}

// stage sends frames at 20 ms and collects the replies.
func (cn *conn) stage(o *options, name string, sid uint16, in codec.ID, out codec.Set, frames [][]byte) (stageResult, [][]byte) {
	res := stageResult{Name: name, Sent: len(frames)}
	type slot struct {
		ch   chan arrival
		sent time.Time
	}
	slots := make([]slot, len(frames))
	before := cn.getStats()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	buf := make([]byte, 0, 512)
	for i, f := range frames {
		if i > 0 {
			<-tick.C
		}
		ch := make(chan arrival, 1)
		cn.mu.Lock()
		cn.wait[pkey(sid, uint32(i))] = ch
		cn.mu.Unlock()
		buf = protocol.AppendRequest(buf[:0], &protocol.Request{StreamID: sid, Seq: uint32(i), Last: i == len(frames)-1, In: in, Outputs: out, Payload: f})
		slots[i] = slot{ch, time.Now()}
		if err := cn.c.Write(context.Background(), websocket.MessageBinary, buf); err != nil {
			cn.event("write: " + err.Error())
			break
		}
	}
	outs := make([][]byte, len(frames))
	var rtt, srv []time.Duration
	deadline := time.Now().Add(o.timeout)
	var seen codec.Set
	want := out
	if want == 0 {
		want = codec.SetOf(codec.All()...).Without(in)
	}
	for i, s := range slots {
		if s.ch == nil {
			res.Missing++
			continue
		}
		select {
		case a := <-s.ch:
			res.Replies++
			d := a.at.Sub(s.sent)
			rtt = append(rtt, d)
			srv = append(srv, time.Duration(a.r.LatencyMicros)*time.Microsecond)
			if a.r.Late {
				res.Late++
			}
			if d > 60*time.Millisecond {
				res.ClientLate++
			}
			seen |= a.r.Outputs
			pick := codec.PCM
			if in == codec.PCM {
				pick = firstOther(want)
			}
			if p := a.r.Payloads[pick]; p != nil {
				outs[i] = slices.Clone(p)
			}
		case <-time.After(time.Until(deadline)):
			res.Missing++
			cn.mu.Lock()
			delete(cn.wait, pkey(sid, uint32(i)))
			cn.mu.Unlock()
		}
		if time.Now().After(deadline) {
			deadline = time.Now()
		}
	}
	res.RTT = latOf(rtt)
	res.Server = latOf(srv)
	res.Outputs = strings.Join(seen.Names(), ",")
	if before != nil {
		if after := cn.getStats(); after != nil {
			res.CPUSeconds = cpu(after) - cpu(before)
		}
	}
	return res, outs
}

func firstOther(s codec.Set) codec.ID {
	for _, id := range s.IDs() {
		if id != codec.PCM {
			return id
		}
	}
	return codec.None
}

// ---- the work of one client ----

func runWorker(o *options, n int) *workerResult {
	session := fmt.Sprintf("%s-%d", o.session, n+1)
	wr := &workerResult{Worker: n + 1, Session: session}
	var outputs []string
	if !o.allOutputs {
		outputs = []string{"pcm"} // the default for a mask-0 frame; each frame sets its own mask
	}
	cn, ready, err := dial(o, session, outputs)
	if err != nil {
		wr.Error = err.Error()
		return wr
	}
	defer cn.c.CloseNow()
	wr.Reused = ready.Reused
	if st := cn.getStats(); st != nil {
		if pid, ok := st["pid"].(float64); ok {
			wr.PID = int(pid)
		}
	}
	wr.Pass = true
	sid := uint16(0x2100 + n*0x10)
	for _, c := range o.codecs {
		for _, sig := range o.signals {
			pcm := signalFuncs[sig](o.frames * codec.FrameSamples)
			in := make([][]byte, o.frames)
			for i := range in {
				in[i] = pcmBytes(pcm[i*codec.FrameSamples : (i+1)*codec.FrameSamples])
			}
			rt := &roundTrip{Codec: c.String(), Signal: sig}
			encOut, decOut := codec.SetOf(c), codec.SetOf(codec.PCM)
			if o.allOutputs {
				encOut, decOut = 0, 0
			}
			var enc [][]byte
			rt.Encode, enc = cn.stage(o, "pcm->"+c.String(), sid, codec.PCM, encOut, in)
			sid++
			for i := range enc {
				if enc[i] == nil {
					enc[i] = make([]byte, c.Size())
				}
			}
			var dec [][]byte
			rt.Decode, dec = cn.stage(o, c.String()+"->pcm", sid, c, decOut, enc)
			sid++
			outPCM := make([]int16, 0, len(pcm))
			for _, f := range dec {
				if f == nil {
					f = make([]byte, codec.FrameSamples*2)
				}
				outPCM = append(outPCM, pcmSamples(f)...)
			}
			rt.Audio = check(sig, pcm, outPCM)
			if !rt.Audio.Pass || rt.Encode.Missing+rt.Decode.Missing > 0 {
				wr.Pass = false
			}
			if o.out != "" {
				base := filepath.Join(o.out, fmt.Sprintf("%s-%s-%s", session, c, sig))
				if writeWAV(base+"-in.wav", pcm) == nil && writeWAV(base+"-out.wav", outPCM) == nil {
					rt.WAVs = []string{base + "-in.wav", base + "-out.wav"}
				}
			}
			wr.Trips = append(wr.Trips, rt)
		}
	}
	wr.Stats = cn.getStats()
	b, _ := json.Marshal(protocol.Bye{Type: protocol.MsgBye, Stop: o.stop})
	cn.c.Write(context.Background(), websocket.MessageText, b)
	cn.c.Close(websocket.StatusNormalClosure, "")
	for {
		select {
		case e := <-cn.events:
			wr.Events = append(wr.Events, e)
			continue
		default:
		}
		break
	}
	return wr
}

func printResults(o *options, rs []*workerResult) {
	for _, r := range rs {
		fmt.Printf("== client %d session %s", r.Worker, r.Session)
		if r.Error != "" {
			fmt.Printf(": %s\n", r.Error)
			continue
		}
		fmt.Printf(" tcd pid %d reused %v\n", r.PID, r.Reused)
		fmt.Printf("%-18s %-8s %5s %5s %4s %4s  %-30s  %-30s %8s  %s\n", "stage", "signal", "sent", "repl", "miss", "late",
			"client rtt ms min/avg/p99/max", "server tcd ms min/avg/p99/max", "cpu s", "outputs")
		for _, t := range r.Trips {
			for _, s := range []stageResult{t.Encode, t.Decode} {
				fmt.Printf("%-18s %-8s %5d %5d %4d %4d  %-30s  %-30s %8.3f  %s\n", s.Name, t.Signal, s.Sent, s.Replies, s.Missing, s.Late,
					fmt.Sprintf("%.2f/%.2f/%.2f/%.2f", s.RTT.MinMS, s.RTT.AvgMS, s.RTT.P99MS, s.RTT.MaxMS),
					fmt.Sprintf("%.2f/%.2f/%.2f/%.2f", s.Server.MinMS, s.Server.AvgMS, s.Server.P99MS, s.Server.MaxMS),
					s.CPUSeconds, s.Outputs)
			}
			a := t.Audio
			fmt.Printf("   audio %s/%s: delay %d samples, corr %.3f, envelope corr %.3f, energy ratio %.2f", t.Codec, t.Signal, a.DelaySamples, a.Correlation, a.EnvelopeCorr, a.EnergyRatio)
			if a.TonePurity > 0 {
				fmt.Printf(", tone purity %.2f", a.TonePurity)
			}
			fmt.Printf(" -> %s\n", map[bool]string{true: "PASS", false: "FAIL"}[a.Pass])
		}
		for _, e := range r.Events {
			fmt.Printf("   event: %s\n", e)
		}
	}
	if o.out != "" {
		fmt.Printf("WAV files in %s\n", o.out)
	}
}
