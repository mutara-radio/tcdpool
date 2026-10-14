package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// AdminHandler returns the admin handler: /healthz, /stats (JSON) and
// /metrics (Prometheus text). Serve it on loopback only: it shows key
// IDs, labels and session names (never secrets).
func (s *Server) AdminHandler(maxProcesses int) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		st := s.opt.Pool.Stats()
		enc.Encode(map[string]any{
			"backend":       s.opt.Backend.Name(),
			"processes":     st,
			"count":         len(st),
			"max_processes": maxProcesses,
			"sessions":      s.Sessions(),
		})
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		st := s.opt.Pool.Stats()
		fmt.Fprintf(w, "# HELP tcdpool_processes Transcoder processes.\n# TYPE tcdpool_processes gauge\ntcdpool_processes %d\n", len(st))
		fmt.Fprintf(w, "# HELP tcdpool_max_processes Process limit of the host.\n# TYPE tcdpool_max_processes gauge\ntcdpool_max_processes %d\n", maxProcesses)
		fmt.Fprintf(w, "# HELP tcdpool_sessions Open client connections.\n# TYPE tcdpool_sessions gauge\ntcdpool_sessions %d\n", s.Sessions())
		type metric struct {
			name, typ, help string
			v               func(i int) float64
		}
		ms := []metric{
			{"uptime_seconds", "gauge", "Time since the last start of the process.", func(i int) float64 { return st[i].UptimeS }},
			{"cpu_seconds_total", "counter", "CPU time of the process.", func(i int) float64 { return st[i].CPUSeconds }},
			{"starts_total", "counter", "Process starts.", func(i int) float64 { return float64(st[i].Starts) }},
			{"crashes_total", "counter", "Process exits that were not asked for.", func(i int) float64 { return float64(st[i].Crashes) }},
			{"desyncs_total", "counter", "Restarts because the link went out of step.", func(i int) float64 { return float64(st[i].Desyncs) }},
			{"stalls_total", "counter", "Restarts because the link stalled.", func(i int) float64 { return float64(st[i].Stalls) }},
			{"frames_in_total", "counter", "Frames sent to the process.", func(i int) float64 { return float64(st[i].FramesIn) }},
			{"frames_out_total", "counter", "Replies sent to the client.", func(i int) float64 { return float64(st[i].FramesOut) }},
			{"late_total", "counter", "Late replies.", func(i int) float64 { return float64(st[i].Late) }},
			{"missing_total", "counter", "Frames with no reply.", func(i int) float64 { return float64(st[i].Missing) }},
			{"dropped_total", "counter", "Frames or replies dropped.", func(i int) float64 { return float64(st[i].Dropped) }},
			{"link_bad_frames_total", "counter", "Bad v2 frames skipped.", func(i int) float64 { return float64(st[i].Link.BadFrames) }},
			{"latency_p99_seconds", "gauge", "99th percentile transcode latency.", func(i int) float64 { return st[i].Latency.P99MS / 1000 }},
			{"latency_avg_seconds", "gauge", "Average transcode latency.", func(i int) float64 { return st[i].Latency.AvgMS / 1000 }},
		}
		for _, m := range ms {
			fmt.Fprintf(w, "# HELP tcdpool_process_%s %s\n# TYPE tcdpool_process_%s %s\n", m.name, m.help, m.name, m.typ)
			for i := range st {
				fmt.Fprintf(w, "tcdpool_process_%s{id=%q,key_id=%q,session=%q,state=%q} %g\n", m.name, label(st[i].ID), label(st[i].KeyID), label(st[i].Session), st[i].State, m.v(i))
			}
		}
	})
	return mux
}

func label(s string) string { return strings.ReplaceAll(s, "\n", " ") }
