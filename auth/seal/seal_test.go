package seal_test

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eve-online-tools/speki/auth/seal"
)

func newSecret(tb testing.TB) string {
	tb.Helper()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// keyFile writes a key file with the primary key first.
func keyFile(primaryID, primarySecret string, others ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "keys:\n  - id: %s\n    secret: %s\n    primary: true\n", primaryID, primarySecret)
	for i := 0; i+1 < len(others); i += 2 {
		fmt.Fprintf(&b, "  - id: %s\n    secret: %s\n", others[i], others[i+1])
	}
	return b.String()
}

func newKeyring(tb testing.TB, raw string) *seal.Keyring {
	tb.Helper()
	k, err := seal.ParseKeyring([]byte(raw))
	if err != nil {
		tb.Fatalf("ParseKeyring: %v", err)
	}
	return k
}

type payload struct {
	User  string `json:"user"`
	Count int    `json:"count"`
}

func TestSealAndOpen(t *testing.T) {
	t.Parallel()
	k := newKeyring(t, keyFile("a", newSecret(t)))
	want := payload{User: "octocat", Count: 3}
	sealed, err := k.Seal(seal.PurposeAccess, want, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !strings.HasPrefix(sealed, "v1.a.") || strings.Contains(sealed, "octocat") {
		t.Errorf("got sealed %q, want prefix v1.a. and no plain text", sealed)
	}
	tampered := []byte(sealed)
	tampered[len(tampered)-3] ^= 1

	testCases := []struct {
		name    string
		purpose seal.Purpose
		sealed  string
		wantErr error
	}{
		{name: "same purpose", purpose: seal.PurposeAccess, sealed: sealed},
		{name: "other purpose", purpose: seal.PurposeRefresh, sealed: sealed, wantErr: seal.ErrInvalid},
		{name: "tampered", purpose: seal.PurposeAccess, sealed: string(tampered), wantErr: seal.ErrInvalid},
		{name: "empty", purpose: seal.PurposeAccess, sealed: "", wantErr: seal.ErrInvalid},
		{name: "no key id", purpose: seal.PurposeAccess, sealed: "v1", wantErr: seal.ErrInvalid},
		{name: "no data", purpose: seal.PurposeAccess, sealed: "v1.a", wantErr: seal.ErrInvalid},
		{name: "other version", purpose: seal.PurposeAccess, sealed: "v2.a.xyz", wantErr: seal.ErrInvalid},
		{name: "unknown key", purpose: seal.PurposeAccess, sealed: "v1.b.xyz", wantErr: seal.ErrInvalid},
		{name: "not base64", purpose: seal.PurposeAccess, sealed: "v1.a.!!!", wantErr: seal.ErrInvalid},
		{name: "shorter than a nonce", purpose: seal.PurposeAccess, sealed: "v1.a.AAAA", wantErr: seal.ErrInvalid},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var got payload
			err := k.Open(testCase.purpose, testCase.sealed, &got)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("got error %v, want %v", err, testCase.wantErr)
			}
			if testCase.wantErr == nil && got != want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestOpenIntoWrongType(t *testing.T) {
	t.Parallel()
	k := newKeyring(t, keyFile("a", newSecret(t)))
	sealed, err := k.Seal(seal.PurposeCode, "text", time.Time{})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	var n int
	err = k.Open(seal.PurposeCode, sealed, &n)
	if !errors.Is(err, seal.ErrInvalid) {
		t.Errorf("got error %v, want %v", err, seal.ErrInvalid)
	}
}

func TestExpiry(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		ttl     time.Duration // 0 never expires
		wait    time.Duration
		wantErr error
	}{
		{name: "within the skew", ttl: time.Minute, wait: time.Minute + 30*time.Second},
		{name: "past the skew", ttl: time.Minute, wait: 3 * time.Minute, wantErr: seal.ErrExpired},
		{name: "no expiry", wait: 10 * 365 * 24 * time.Hour},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				k := newKeyring(t, keyFile("a", newSecret(t)))
				var expires time.Time
				if testCase.ttl > 0 {
					expires = time.Now().Add(testCase.ttl)
				}
				sealed, err := k.Seal(seal.PurposeCode, "x", expires)
				if err != nil {
					t.Fatalf("Seal: %v", err)
				}
				time.Sleep(testCase.wait)
				var v string
				err = k.Open(seal.PurposeCode, sealed, &v)
				if !errors.Is(err, testCase.wantErr) {
					t.Errorf("got error %v, want %v", err, testCase.wantErr)
				}
			})
		})
	}
}

func TestRotation(t *testing.T) {
	t.Parallel()
	oldKey, newKey := newSecret(t), newSecret(t)
	before := newKeyring(t, keyFile("old", oldKey))
	during := newKeyring(t, keyFile("new", newKey, "old", oldKey))
	after := newKeyring(t, keyFile("new", newKey))

	sealed, err := before.Seal(seal.PurposeRefresh, "token", time.Time{})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	var v string
	err = during.Open(seal.PurposeRefresh, sealed, &v)
	if err != nil || v != "token" {
		t.Errorf("during rotation: got %q, %v, want %q, nil", v, err, "token")
	}
	fresh, err := during.Seal(seal.PurposeRefresh, "token", time.Time{})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !strings.HasPrefix(fresh, "v1.new.") {
		t.Errorf("got %q, want prefix v1.new.", fresh)
	}
	err = after.Open(seal.PurposeRefresh, sealed, &v)
	if !errors.Is(err, seal.ErrInvalid) {
		t.Errorf("retired key: got error %v, want %v", err, seal.ErrInvalid)
	}
}

func TestSingleKeyMovesToKeyFile(t *testing.T) {
	t.Parallel()
	old := newSecret(t)
	single, err := seal.SingleKey(old)
	if err != nil {
		t.Fatalf("SingleKey: %v", err)
	}
	sealed, err := single.Seal(seal.PurposeRefresh, "token", time.Time{})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !strings.HasPrefix(sealed, "v1."+seal.SingleKeyID+".") {
		t.Errorf("got %q, want prefix v1.%s.", sealed, seal.SingleKeyID)
	}
	moved := newKeyring(t, keyFile("new", newSecret(t), seal.SingleKeyID, old))
	var v string
	err = moved.Open(seal.PurposeRefresh, sealed, &v)
	if err != nil || v != "token" {
		t.Errorf("got %q, %v, want %q, nil", v, err, "token")
	}
}

func TestSingleKeyRejects(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		secret  string
		wantErr string
	}{
		{name: "short key", secret: base64.StdEncoding.EncodeToString([]byte("too short")), wantErr: "at least 32"},
		{name: "not base64", secret: "!!", wantErr: "isn't base64"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := seal.SingleKey(testCase.secret)
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Errorf("got error %v, want it to mention %q", err, testCase.wantErr)
			}
		})
	}
}

func TestParseKeyringRejects(t *testing.T) {
	t.Parallel()
	good := newSecret(t)
	short := base64.StdEncoding.EncodeToString([]byte("too short"))
	testCases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "no primary", raw: fmt.Sprintf("keys:\n  - id: a\n    secret: %s\n", good), wantErr: "no primary key"},
		{name: "two primaries", raw: keyFile("a", good) + fmt.Sprintf("  - id: b\n    secret: %s\n    primary: true\n", good), wantErr: "more than one primary"},
		{name: "short key", raw: keyFile("a", short), wantErr: "at least 32"},
		{name: "duplicate id", raw: keyFile("a", good, "a", good), wantErr: "duplicate key id"},
		{name: "dot in id", raw: keyFile("a.b", good), wantErr: "no dots"},
		{name: "not yaml", raw: "keys: [", wantErr: "key file:"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := seal.ParseKeyring([]byte(testCase.raw))
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Errorf("got error %v, want it to mention %q", err, testCase.wantErr)
			}
		})
	}
}
