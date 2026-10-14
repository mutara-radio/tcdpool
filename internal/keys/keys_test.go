package keys

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateVerify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	k, tok, err := Create(path, "N0CALL node 2100", 2)
	if err != nil {
		t.Fatal(err)
	}
	id, secret, _ := strings.Cut(tok, ".")
	if id != k.ID || len(secret) != 43 {
		t.Fatalf("token %q", tok)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), secret) {
		t.Fatal("the file holds the secret")
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Verify("Bearer " + tok)
	if err != nil || got.ID != k.ID || got.MaxProcesses != 2 || got.Label != "N0CALL node 2100" {
		t.Fatalf("verify: %v %+v", err, got)
	}

	bad := []string{
		"",
		tok,                              // no Bearer
		"Bearer " + id,                   // no secret
		"Bearer " + id + ".",             // empty secret
		"Bearer " + id + ".x" + secret,   // wrong secret
		"Bearer k000000000000." + secret, // unknown id
		"Basic " + tok,
	}
	for _, h := range bad {
		if _, err := s.Verify(h); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%q: %v", h, err)
		}
	}
}

func TestDisableReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	k, tok, _ := Create(path, "W1AW", 0)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled(path, k.ID, false); err != nil {
		t.Fatal(err)
	}
	// make sure the mtime differs on coarse file systems
	os.Chtimes(path, time.Now().Add(time.Second), time.Now().Add(time.Second))
	if changed, err := s.Reload(); !changed || err != nil {
		t.Fatalf("reload: %v %v", changed, err)
	}
	if _, err := s.Verify("Bearer " + tok); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled key: %v", err)
	}
	if changed, _ := s.Reload(); changed {
		t.Error("reload without a change read the file")
	}
	SetEnabled(path, k.ID, true)
	os.Chtimes(path, time.Now().Add(2*time.Second), time.Now().Add(2*time.Second))
	s.Reload()
	if _, err := s.Verify("Bearer " + tok); err != nil {
		t.Fatalf("enabled again: %v", err)
	}
	if err := SetEnabled(path, "k999999999999", false); err == nil {
		t.Error("unknown key accepted")
	}
	list, _ := List(path)
	if len(list) != 1 {
		t.Errorf("list: %d keys", len(list))
	}
}

func TestBadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	os.WriteFile(path, []byte(`{"keys":[{"id":"bad","hash":"x"}]}`), 0o600)
	if _, err := Open(path); err == nil {
		t.Error("bad entry accepted")
	}
	// a missing file is an empty key list
	s, err := Open(filepath.Join(t.TempDir(), "none.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify("Bearer k000000000000.abc"); !errors.Is(err, ErrUnauthorized) {
		t.Error(err)
	}
}
