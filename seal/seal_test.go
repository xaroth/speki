package seal

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func secret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func keyring(t *testing.T, yaml string) *Keyring {
	t.Helper()
	k, err := ParseKeyring([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	return k
}

type payload struct {
	User  string `json:"user"`
	Count int    `json:"count"`
}

func TestSealAndOpen(t *testing.T) {
	k := keyring(t, fmt.Sprintf("keys:\n  - id: a\n    secret: %s\n    primary: true\n", secret()))

	s, err := k.Seal(Access, payload{User: "octocat", Count: 3}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s, "v1.a.") || strings.Contains(s, "octocat") {
		t.Errorf("sealed = %q", s)
	}
	var got payload
	err = k.Open(Access, s, &got)
	if err != nil || got != (payload{User: "octocat", Count: 3}) {
		t.Errorf("Open = %+v, %v", got, err)
	}

	// Another purpose can't open it.
	err = k.Open(Refresh, s, &got)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("open as refresh: %v", err)
	}

	// Tampering is detected.
	raw := []byte(s)
	raw[len(raw)-3] ^= 1
	err = k.Open(Access, string(raw), &got)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("tampered: %v", err)
	}

	for _, bad := range []string{"", "v1", "v1.a", "v2.a.xyz", "v1.b.xyz", "v1.a.!!!", "v1.a.AAAA"} {
		if err := k.Open(Access, bad, &got); !errors.Is(err, ErrInvalid) {
			t.Errorf("Open(%q) = %v", bad, err)
		}
	}
}

func TestExpiry(t *testing.T) {
	k := keyring(t, fmt.Sprintf("keys:\n  - id: a\n    secret: %s\n    primary: true\n", secret()))
	now := time.Now()
	k.now = func() time.Time { return now }

	s, err := k.Seal(Code, "x", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var v string
	k.now = func() time.Time { return now.Add(time.Minute + 30*time.Second) } // within the skew
	if err := k.Open(Code, s, &v); err != nil {
		t.Errorf("within skew: %v", err)
	}
	k.now = func() time.Time { return now.Add(3 * time.Minute) }
	if err := k.Open(Code, s, &v); !errors.Is(err, ErrExpired) {
		t.Errorf("expired: %v", err)
	}

	forever, err := k.Seal(Client, "x", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	k.now = func() time.Time { return now.Add(10 * 365 * 24 * time.Hour) }
	if err := k.Open(Client, forever, &v); err != nil {
		t.Errorf("value without expiry: %v", err)
	}
}

func TestRotation(t *testing.T) {
	oldSecret, newSecret := secret(), secret()
	before := keyring(t, fmt.Sprintf("keys:\n  - id: old\n    secret: %s\n    primary: true\n", oldSecret))
	during := keyring(t, fmt.Sprintf("keys:\n  - id: new\n    secret: %s\n    primary: true\n  - id: old\n    secret: %s\n", newSecret, oldSecret))
	after := keyring(t, fmt.Sprintf("keys:\n  - id: new\n    secret: %s\n    primary: true\n", newSecret))

	s, err := before.Seal(Refresh, "token", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var v string
	if err := during.Open(Refresh, s, &v); err != nil || v != "token" {
		t.Errorf("during rotation: %q, %v", v, err)
	}
	fresh, _ := during.Seal(Refresh, "token", time.Time{})
	if !strings.HasPrefix(fresh, "v1.new.") {
		t.Errorf("new values use %q", fresh[:7])
	}
	if err := after.Open(Refresh, s, &v); !errors.Is(err, ErrInvalid) {
		t.Errorf("retired key still opens: %v", err)
	}
}

func TestParseKeyringRejects(t *testing.T) {
	good := secret()
	short := base64.StdEncoding.EncodeToString([]byte("too short"))
	tests := map[string]string{
		"no primary":     fmt.Sprintf("keys:\n  - id: a\n    secret: %s\n", good),
		"two primaries":  fmt.Sprintf("keys:\n  - id: a\n    secret: %s\n    primary: true\n  - id: b\n    secret: %s\n    primary: true\n", good, good),
		"short key":      fmt.Sprintf("keys:\n  - id: a\n    secret: %s\n    primary: true\n", short),
		"duplicate id":   fmt.Sprintf("keys:\n  - id: a\n    secret: %s\n    primary: true\n  - id: a\n    secret: %s\n", good, good),
		"not base64":     "keys:\n  - id: a\n    secret: '!!'\n    primary: true\n",
		"dot in id":      fmt.Sprintf("keys:\n  - id: a.b\n    secret: %s\n    primary: true\n", good),
		"not yaml":       "keys: [",
		"no keys at all": "",
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseKeyring([]byte(raw)); err == nil {
				t.Error("accepted")
			}
		})
	}
}
