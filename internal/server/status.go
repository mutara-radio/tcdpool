package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"time"

	"github.com/dbehnke/tcdpool/internal/keys"
	"github.com/dbehnke/tcdpool/internal/protocol"
)

// Status states (the "state" field of GET /v1/status).
const (
	StateOK   = "ok"
	StateBusy = "busy"
	StateFull = "full"
)

// Thresholds for the status state.
const (
	// BusyProcessShare: busy when this share of max_processes or more
	// is in use.
	BusyProcessShare = 0.8
	// BusyCPULoad: busy when the host load for each CPU is this or more.
	BusyCPULoad = 0.8
	// BusyLateRate: busy when this share of the replies or more was
	// late, and there are at least BusyLateMinFrames replies.
	BusyLateRate      = 0.02
	BusyLateMinFrames = 500
)

// Status is the reply of GET /v1/status. It shows the server and the
// key of the caller only: never other keys or their session names.
type Status struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	Backend  string `json:"backend"`
	// State is ok, busy or full. Reasons are short codes that tell why
	// it is not ok: processes_busy, cpu_busy, late_replies, host_full.
	State   string   `json:"state"`
	Reasons []string `json:"reasons,omitempty"`

	Processes ProcessStatus `json:"processes"`
	Key       KeyStatus     `json:"key"`

	// CPULoad is the 1-minute load average of the host divided by the
	// number of CPUs (Linux only; omitted elsewhere).
	CPULoad *float64 `json:"cpu_load,omitempty"`
	CPUs    int      `json:"cpus"`

	// LateRate is late replies / replies of all running processes,
	// since each process started. Frames is the replies count.
	LateRate float64 `json:"late_rate"`
	Frames   uint64  `json:"frames"`

	Time time.Time `json:"time"`
}

// ProcessStatus counts the tcd processes on the host.
type ProcessStatus struct {
	Running int `json:"running"` // all processes, idle ones too
	Idle    int `json:"idle"`    // no connection: the server can stop them to make room
	Max     int `json:"max"`
}

// KeyStatus counts the sessions (processes) of the caller's key.
type KeyStatus struct {
	ID          string `json:"id"`
	Sessions    int    `json:"sessions"`
	Idle        int    `json:"idle"`
	MaxSessions int    `json:"max_sessions"`
	// Full: no new session is possible for this key now (all its
	// sessions are in use and none is idle).
	Full bool `json:"full"`
}

// auth checks the Bearer key. It writes the HTTP error and returns
// false when the key is bad.
func (s *Server) auth(w http.ResponseWriter, r *http.Request) (keys.Key, bool) {
	key, err := s.opt.Keys.Verify(r.Header.Get("Authorization"))
	switch {
	case err == nil:
		return key, true
	case errors.Is(err, keys.ErrDisabled):
		s.opt.Logf("server: %s: refused, key %s is disabled", r.RemoteAddr, key.ID)
		httpError(w, http.StatusForbidden, protocol.ErrKeyDisabled, "the key is disabled")
	default:
		s.opt.Logf("server: %s: refused, bad or unknown key", r.RemoteAddr)
		httpError(w, http.StatusUnauthorized, protocol.ErrUnauthorized, "send Authorization: Bearer <key id>.<secret>")
	}
	return key, false
}

func (s *Server) keyMax(k keys.Key) int {
	if k.MaxProcesses > 0 {
		return k.MaxProcesses
	}
	return s.opt.KeyMaxProcesses
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	key, ok := s.auth(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(s.Status(key))
}

// Status makes the status for a key.
func (s *Server) Status(key keys.Key) Status {
	st := Status{
		Version:  s.opt.Version,
		Protocol: protocol.Version,
		Backend:  s.opt.Backend.Name(),
		CPUs:     runtime.NumCPU(),
		Time:     time.Now().UTC().Truncate(time.Second),
	}
	if st.Version == "" {
		st.Version = "dev"
	}
	st.Processes.Max = s.opt.MaxProcesses
	st.Key.ID = key.ID
	st.Key.MaxSessions = s.keyMax(key)
	var late uint64
	for _, p := range s.opt.Pool.Stats() {
		idle := p.State == "idle"
		st.Processes.Running++
		if idle {
			st.Processes.Idle++
		}
		if p.KeyID == key.ID {
			st.Key.Sessions++
			if idle {
				st.Key.Idle++
			}
		}
		late += p.Late
		st.Frames += p.FramesOut
	}
	if st.Frames > 0 {
		st.LateRate = round(float64(late)/float64(st.Frames), 4)
	}
	if l, ok := loadPerCPU(); ok {
		l = round(l, 2)
		st.CPULoad = &l
	}
	st.Key.Full = st.Key.Sessions >= st.Key.MaxSessions && st.Key.Idle == 0
	st.State, st.Reasons = stateOf(st)
	return st
}

// stateOf gives the overall state:
//   - full: the host has max_processes processes and none is idle, so a
//     new session gets host_full.
//   - busy: 80 % or more of max_processes in use, or a host load of 0.8
//     or more for each CPU, or 2 % or more late replies (after 500
//     replies).
//   - ok: else.
//
// The key limit does not change the state; see Key.Full.
func stateOf(st Status) (string, []string) {
	p := st.Processes
	if p.Max > 0 && p.Running >= p.Max && p.Idle == 0 {
		return StateFull, []string{"host_full"}
	}
	var why []string
	if p.Max > 0 && float64(p.Running-p.Idle) >= BusyProcessShare*float64(p.Max) {
		why = append(why, "processes_busy")
	}
	if st.CPULoad != nil && *st.CPULoad >= BusyCPULoad {
		why = append(why, "cpu_busy")
	}
	if st.Frames >= BusyLateMinFrames && st.LateRate >= BusyLateRate {
		why = append(why, "late_replies")
	}
	if len(why) > 0 {
		return StateBusy, why
	}
	return StateOK, nil
}

func round(v float64, places int) float64 {
	m := 1.0
	for range places {
		m *= 10
	}
	return float64(int64(v*m+0.5)) / m
}
