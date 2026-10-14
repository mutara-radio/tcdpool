// Package keys keeps the access keys of tcdpool in a JSON file.
//
// A key has an ID and a secret. The client sends "Bearer <id>.<secret>".
// The file holds only the SHA-256 hash of the secret, so a copy of the
// file does not give access. The secret is shown once, when the key is
// made. Code in this package never logs a secret.
package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Key is one access key.
type Key struct {
	ID      string `json:"id"`
	Hash    string `json:"hash"` // "sha256:<hex>" of the secret
	Label   string `json:"label"`
	Enabled bool   `json:"enabled"`
	// MaxProcesses is the most tcd processes this key can have at one
	// time. 0 means the server default.
	MaxProcesses int       `json:"max_processes"`
	Created      time.Time `json:"created"`
}

type file struct {
	Keys []Key `json:"keys"`
}

var (
	// ErrUnauthorized: no token, a bad token, an unknown key or a wrong
	// secret. The caller must not tell the client which one.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrDisabled: the key is good but disabled.
	ErrDisabled = errors.New("key disabled")
)

var idRE = regexp.MustCompile(`^k[0-9a-f]{12}$`)

func hashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return "sha256:" + hex.EncodeToString(h[:])
}

func read(path string) (file, error) {
	var f file
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("keys: %s: %w", path, err)
	}
	for _, k := range f.Keys {
		if !idRE.MatchString(k.ID) || !strings.HasPrefix(k.Hash, "sha256:") || len(k.Hash) != 7+64 {
			return f, fmt.Errorf("keys: %s: bad key entry %q", path, k.ID)
		}
	}
	return f, nil
}

// write replaces the file in one step (temp file and rename), mode 0600.
func write(path string, f file) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".keys-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Create adds a new enabled key to the file and returns the key and the
// token "<id>.<secret>". The token is not stored; show it once.
func Create(path, label string, maxProcesses int) (Key, string, error) {
	if maxProcesses < 0 {
		return Key{}, "", errors.New("keys: max processes must be 0 or more")
	}
	f, err := read(path)
	if err != nil {
		return Key{}, "", err
	}
	var idb [6]byte
	var sec [32]byte
	rand.Read(idb[:])
	rand.Read(sec[:])
	secret := base64.RawURLEncoding.EncodeToString(sec[:])
	k := Key{
		ID:           "k" + hex.EncodeToString(idb[:]),
		Hash:         hashSecret(secret),
		Label:        label,
		Enabled:      true,
		MaxProcesses: maxProcesses,
		Created:      time.Now().UTC().Truncate(time.Second),
	}
	f.Keys = append(f.Keys, k)
	if err := write(path, f); err != nil {
		return Key{}, "", err
	}
	return k, k.ID + "." + secret, nil
}

// List returns the keys in the file.
func List(path string) ([]Key, error) {
	f, err := read(path)
	return f.Keys, err
}

// SetEnabled enables or disables a key.
func SetEnabled(path, id string, enabled bool) error {
	f, err := read(path)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(f.Keys, func(k Key) bool { return k.ID == id })
	if i < 0 {
		return fmt.Errorf("keys: no key %q", id)
	}
	f.Keys[i].Enabled = enabled
	return write(path, f)
}

// Store is the key file as the server sees it. It reads the file again
// when the file changes.
type Store struct {
	path  string
	mu    sync.RWMutex
	keys  map[string]Key
	mtime time.Time
	size  int64
}

// Open reads the key file.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	if _, err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload reads the file if it changed. It returns true when it read it.
func (s *Store) Reload() (bool, error) {
	st, err := os.Stat(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	s.mu.RLock()
	same := s.keys != nil && st != nil && st.ModTime().Equal(s.mtime) && st.Size() == s.size
	s.mu.RUnlock()
	if same {
		return false, nil
	}
	f, err := read(s.path)
	if err != nil {
		return false, err
	}
	m := make(map[string]Key, len(f.Keys))
	for _, k := range f.Keys {
		m[k.ID] = k
	}
	s.mu.Lock()
	s.keys = m
	if st != nil {
		s.mtime, s.size = st.ModTime(), st.Size()
	}
	s.mu.Unlock()
	return true, nil
}

// Get returns a key by ID.
func (s *Store) Get(id string) (Key, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keys[id]
	return k, ok
}

// dummyHash is compared when the key ID is unknown, so that the time of
// a check does not show whether an ID exists.
var dummyHash = hashSecret("no such key")

// Verify checks an Authorization header value ("Bearer <id>.<secret>").
func (s *Store) Verify(header string) (Key, error) {
	tok, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return Key{}, ErrUnauthorized
	}
	id, secret, ok := strings.Cut(strings.TrimSpace(tok), ".")
	if !ok || secret == "" {
		return Key{}, ErrUnauthorized
	}
	k, found := s.Get(id)
	want := dummyHash
	if found {
		want = k.Hash
	}
	got := hashSecret(secret)
	match := subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
	if !found || !match {
		return Key{}, ErrUnauthorized
	}
	if !k.Enabled {
		return k, ErrDisabled
	}
	return k, nil
}
