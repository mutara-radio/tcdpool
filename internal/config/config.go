// Package config reads the tcdpool configuration file (JSON).
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dbehnke/tcdpool/internal/backend/nostar"
)

// Duration is a time.Duration written as a string ("60s", "500ms").
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"60s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// D returns the time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// TLS settings.
type TLS struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
	// SelfSigned makes a new self-signed certificate at each start when
	// Cert and Key are empty. For development only.
	SelfSigned bool `json:"self_signed"`
	// Disable serves plain HTTP/WebSocket. For tests behind a TLS proxy
	// only.
	Disable bool `json:"disable"`
}

// Backend settings (nostar tcd).
type Backend struct {
	Binary         string       `json:"binary"`
	WorkDir        string       `json:"work_dir"`
	PortMin        int          `json:"port_min"`
	PortMax        int          `json:"port_max"`
	Framing        string       `json:"framing"`
	Gains          nostar.Gains `json:"gains"`
	ConnectTimeout Duration     `json:"connect_timeout"`
	Watchdog       Duration     `json:"watchdog"`
}

// Restart settings for a tcd process that failed.
type Restart struct {
	MinBackoff  Duration `json:"min_backoff"`
	MaxBackoff  Duration `json:"max_backoff"`
	MaxFailures int      `json:"max_failures"`
}

// Config is the whole file.
type Config struct {
	Listen      string  `json:"listen"`
	TLS         TLS     `json:"tls"`
	AdminListen string  `json:"admin_listen"`
	KeysFile    string  `json:"keys_file"`
	Backend     Backend `json:"backend"`
	// IdleTimeout: a tcd process with no client is stopped after this
	// time. A client that comes back in this time with the same key and
	// session name gets the same process.
	IdleTimeout Duration `json:"idle_timeout"`
	// MaxProcesses is the most tcd processes on this host.
	MaxProcesses int `json:"max_processes"`
	// KeyMaxProcesses is the most processes of one key when the key
	// does not set its own limit.
	KeyMaxProcesses int `json:"key_max_processes"`
	// LateAfter: a reply that takes longer is counted as late.
	LateAfter Duration `json:"late_after"`
	// MissingAfter: a frame with no reply after this time is counted as
	// missing.
	MissingAfter Duration `json:"missing_after"`
	Restart      Restart  `json:"restart"`
}

// Default returns the default configuration.
func Default() Config {
	return Config{
		Listen:      ":8443",
		AdminListen: "127.0.0.1:9180",
		KeysFile:    "keys.json",
		Backend: Backend{
			Binary:         "tcd",
			PortMin:        40000,
			PortMax:        48999,
			Framing:        "v2",
			ConnectTimeout: Duration(15 * time.Second),
			Watchdog:       Duration(20 * time.Second),
		},
		IdleTimeout:     Duration(60 * time.Second),
		MaxProcesses:    8,
		KeyMaxProcesses: 2,
		LateAfter:       Duration(60 * time.Millisecond),
		MissingAfter:    Duration(2 * time.Second),
		Restart: Restart{
			MinBackoff:  Duration(500 * time.Millisecond),
			MaxBackoff:  Duration(30 * time.Second),
			MaxFailures: 5,
		},
	}
}

// Load reads a file over the defaults. Unknown fields are an error.
func Load(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	return c, c.Validate()
}

// Validate checks the values.
func (c Config) Validate() error {
	var errs []error
	if c.Listen == "" {
		errs = append(errs, errors.New("listen is empty"))
	}
	if !c.TLS.Disable && !c.TLS.SelfSigned && (c.TLS.Cert == "" || c.TLS.Key == "") {
		errs = append(errs, errors.New("tls: give cert and key, or set self_signed"))
	}
	if c.KeysFile == "" {
		errs = append(errs, errors.New("keys_file is empty"))
	}
	if c.Backend.PortMin < 1025 || c.Backend.PortMax > 49000 || c.Backend.PortMin > c.Backend.PortMax {
		errs = append(errs, fmt.Errorf("backend port range %d-%d must be inside 1025-49000", c.Backend.PortMin, c.Backend.PortMax))
	}
	if c.MaxProcesses < 1 || c.KeyMaxProcesses < 1 {
		errs = append(errs, errors.New("max_processes and key_max_processes must be 1 or more"))
	}
	if c.IdleTimeout < 0 || c.LateAfter <= 0 || c.MissingAfter <= c.LateAfter {
		errs = append(errs, errors.New("idle_timeout must be 0 or more, and 0 < late_after < missing_after"))
	}
	if c.Restart.MinBackoff <= 0 || c.Restart.MaxBackoff < c.Restart.MinBackoff || c.Restart.MaxFailures < 1 {
		errs = append(errs, errors.New("restart: 0 < min_backoff <= max_backoff, max_failures >= 1"))
	}
	return errors.Join(errs...)
}
