package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dbehnke/tcdpool/internal/keys"
)

func (e *env) status(t *testing.T, token string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", e.ts.URL+"/v1/status", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestStatusAuth(t *testing.T) {
	e := setup(t, 2)
	for name, tok := range map[string]string{"none": "", "wrong": e.keyID + ".nope"} {
		resp, body := e.status(t, tok)
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), `"unauthorized"`) {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	keys.SetEnabled(e.keysPath, e.keyID, false)
	os.Chtimes(e.keysPath, time.Now().Add(time.Second), time.Now().Add(time.Second))
	e.srv.opt.Keys.Reload()
	if resp, _ := e.status(t, e.token); resp.StatusCode != http.StatusForbidden {
		t.Errorf("disabled: %d", resp.StatusCode)
	}
}

func TestStatus(t *testing.T) {
	e := setup(t, 2)
	// a second key with one session: the first key must not see it
	_, tok2, err := keys.Create(e.keysPath, "W1AW other", 3)
	if err != nil {
		t.Fatal(err)
	}
	os.Chtimes(e.keysPath, time.Now().Add(time.Second), time.Now().Add(time.Second))
	e.srv.opt.Keys.Reload()
	c2, _, _ := e.dial(t, tok2)
	if m := hello(t, c2, "secret-session-ysf-club"); m["type"] != "ready" {
		t.Fatal(m)
	}
	c, _, _ := e.dial(t, e.token)
	if m := hello(t, c, "node-2100"); m["type"] != "ready" {
		t.Fatal(m)
	}

	resp, body := e.status(t, e.token)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "secret-session") || strings.Contains(string(body), "W1AW") ||
		strings.Contains(string(body), "node-2100") {
		t.Errorf("status shows session names or labels: %s", body)
	}
	var st Status
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if st.Version != "test" || st.Protocol != 1 || st.Backend == "" || st.CPUs < 1 || st.Time.IsZero() {
		t.Errorf("header fields: %+v", st)
	}
	if st.Processes != (ProcessStatus{Running: 2, Idle: 0, Max: 4}) {
		t.Errorf("processes: %+v", st.Processes)
	}
	if st.Key != (KeyStatus{ID: e.keyID, Sessions: 1, MaxSessions: 2}) {
		t.Errorf("key: %+v", st.Key)
	}
	if st.State != StateOK || st.Reasons != nil {
		t.Errorf("state %s %v", st.State, st.Reasons)
	}

	// the other key sees its own limit (3) and session count
	_, body = e.status(t, tok2)
	json.Unmarshal(body, &st)
	if st.Key.Sessions != 1 || st.Key.MaxSessions != 3 || st.Key.ID == e.keyID {
		t.Errorf("key 2: %+v", st.Key)
	}

	// a second session uses the last slot of the key: full for the key
	c3, _, _ := e.dial(t, e.token)
	hello(t, c3, "node-2101")
	_, body = e.status(t, e.token)
	json.Unmarshal(body, &st)
	if !st.Key.Full || st.Key.Sessions != 2 {
		t.Errorf("key full: %+v", st.Key)
	}
}

func TestStateOf(t *testing.T) {
	load := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		name  string
		st    Status
		state string
		why   []string
	}{
		{"empty", Status{Processes: ProcessStatus{Max: 8}}, StateOK, nil},
		{"some", Status{Processes: ProcessStatus{Running: 6, Max: 8}}, StateOK, nil},
		{"80%", Status{Processes: ProcessStatus{Running: 7, Max: 8}}, StateBusy, []string{"processes_busy"}},
		{"80% but idle", Status{Processes: ProcessStatus{Running: 7, Idle: 2, Max: 8}}, StateOK, nil},
		{"full", Status{Processes: ProcessStatus{Running: 8, Max: 8}}, StateFull, []string{"host_full"}},
		{"max but one idle", Status{Processes: ProcessStatus{Running: 8, Idle: 1, Max: 8}}, StateBusy, []string{"processes_busy"}},
		{"cpu", Status{Processes: ProcessStatus{Max: 8}, CPULoad: load(0.9)}, StateBusy, []string{"cpu_busy"}},
		{"late", Status{Processes: ProcessStatus{Max: 8}, LateRate: 0.05, Frames: 1000}, StateBusy, []string{"late_replies"}},
		{"late, few frames", Status{Processes: ProcessStatus{Max: 8}, LateRate: 0.5, Frames: 10}, StateOK, nil},
		{"all", Status{Processes: ProcessStatus{Running: 7, Max: 8}, CPULoad: load(1.2), LateRate: 0.02, Frames: 500}, StateBusy, []string{"processes_busy", "cpu_busy", "late_replies"}},
	} {
		state, why := stateOf(tc.st)
		if state != tc.state || !reflect.DeepEqual(why, tc.why) {
			t.Errorf("%s: %s %v, want %s %v", tc.name, state, why, tc.state, tc.why)
		}
	}
}
