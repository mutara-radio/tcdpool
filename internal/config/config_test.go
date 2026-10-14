package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	c, err := Load(write(t, `{"tls":{"self_signed":true},"idle_timeout":"90s",
		"backend":{"binary":"/opt/tcdpool/tcd","framing":"raw","gains":{"dmr_in":-3}},
		"restart":{"min_backoff":"1s","max_backoff":"10s","max_failures":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.IdleTimeout.D() != 90*time.Second || c.Backend.Binary != "/opt/tcdpool/tcd" || c.Backend.Framing != "raw" ||
		c.Backend.Gains.DMRIn != -3 || c.Restart.MaxFailures != 3 {
		t.Fatalf("%+v", c)
	}
	// defaults stay
	if c.Listen != ":8443" || c.MaxProcesses != 8 || c.Backend.PortMin != 40000 || c.LateAfter.D() != 60*time.Millisecond {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestLoadErrors(t *testing.T) {
	for name, s := range map[string]string{
		"unknown field": `{"tls":{"self_signed":true},"listn":":1"}`,
		"no tls":        `{}`,
		"duration":      `{"tls":{"self_signed":true},"idle_timeout":60}`,
		"ports":         `{"tls":{"self_signed":true},"backend":{"port_min":80}}`,
		"admin":         `{"tls":{"self_signed":true},"admin_listen":"0.0.0.0:9180"}`,
		"late":          `{"tls":{"self_signed":true},"late_after":"3s"}`,
		"limits":        `{"tls":{"self_signed":true},"max_processes":0}`,
	} {
		if _, err := Load(write(t, s)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := Load(write(t, `{"tls":{"disable":true},"admin_listen":""}`)); err != nil {
		t.Errorf("admin off: %v", err)
	}
	if err := Default().Validate(); err == nil || !strings.Contains(err.Error(), "tls") {
		t.Errorf("default without tls: %v", err)
	}
}
