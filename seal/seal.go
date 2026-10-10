// Package seal turns values into opaque, tamper-proof strings that only speki can open. Speki hands these
// out instead of storing anything: client registrations, OAuth state, codes and tokens.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Purpose separates sealed values by use: each purpose has its own key, so a value sealed for one can't
// be opened as another.
type Purpose string

// The purposes speki seals values for.
const (
	PurposeClient      Purpose = "client"
	PurposeConsent     Purpose = "consent"
	PurposeTransaction Purpose = "transaction"
	PurposeCode        Purpose = "code"
	PurposeAccess      Purpose = "access"
	PurposeRefresh     Purpose = "refresh"
	PurposeCache       Purpose = "cache" // values parked in a shared store
)

// ErrInvalid is returned by Open for a value that isn't a sealed value of that purpose from this keyring.
var ErrInvalid = errors.New("invalid sealed value")

// ErrExpired is returned by Open for a sealed value past its expiry.
var ErrExpired = errors.New("sealed value expired")

// clockSkew is how far past its expiry a value is still accepted, for instances whose clocks drift.
const clockSkew = time.Minute

const version = "v1"

// Keyring holds the keys speki seals with. New values use the primary key; any key opens values sealed
// with it.
type Keyring struct {
	primary string
	keys    map[string][]byte
	now     func() time.Time
}

type keyFile struct {
	Keys []struct {
		ID      string `yaml:"id"`
		Secret  string `yaml:"secret"` // base64 of at least 32 random bytes
		Primary bool   `yaml:"primary"`
	} `yaml:"keys"`
}

// ParseKeyring reads a key file. It needs exactly one primary key, keys of at least 32 bytes, and unique
// IDs.
func ParseKeyring(raw []byte) (*Keyring, error) {
	var f keyFile
	err := yaml.Unmarshal(raw, &f)
	if err != nil {
		return nil, fmt.Errorf("key file: %w", err)
	}
	k := &Keyring{keys: make(map[string][]byte), now: time.Now}
	for _, entry := range f.Keys {
		if entry.ID == "" || strings.Contains(entry.ID, ".") {
			return nil, fmt.Errorf("key file: key id %q must be set and contain no dots", entry.ID)
		}
		if _, dup := k.keys[entry.ID]; dup {
			return nil, fmt.Errorf("key file: duplicate key id %q", entry.ID)
		}
		secret, err := base64.StdEncoding.DecodeString(entry.Secret)
		if err != nil {
			return nil, fmt.Errorf("key file: key %q: secret isn't base64", entry.ID)
		}
		if len(secret) < 32 {
			return nil, fmt.Errorf("key file: key %q is %d bytes, at least 32 needed", entry.ID, len(secret))
		}
		k.keys[entry.ID] = secret
		if entry.Primary {
			if k.primary != "" {
				return nil, errors.New("key file: more than one primary key")
			}
			k.primary = entry.ID
		}
	}
	if k.primary == "" {
		return nil, errors.New("key file: no primary key")
	}
	return k, nil
}

type envelope struct {
	Expires int64           `json:"exp,omitempty"` // Unix seconds; 0 never expires
	Value   json.RawMessage `json:"v"`
}

// Seal encrypts v for purpose p. A zero expires means the value never expires.
func (k *Keyring) Seal(p Purpose, v any, expires time.Time) (string, error) {
	value, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("seal: %w", err)
	}
	env := envelope{Value: value}
	if !expires.IsZero() {
		env.Expires = expires.Unix()
	}
	plain, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("seal: %w", err)
	}
	aead, err := k.aead(p, k.primary)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce) // crypto/rand.Read never fails
	sealed := aead.Seal(nonce, nonce, plain, additionalData(p, k.primary))
	return version + "." + k.primary + "." + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Open decrypts a value sealed for purpose p into v.
func (k *Keyring) Open(p Purpose, s string, v any) error {
	ver, rest, ok := strings.Cut(s, ".")
	if !ok || ver != version {
		return ErrInvalid
	}
	kid, data, ok := strings.Cut(rest, ".")
	if !ok {
		return ErrInvalid
	}
	if _, known := k.keys[kid]; !known {
		return ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(data)
	if err != nil {
		return ErrInvalid
	}
	aead, err := k.aead(p, kid)
	if err != nil {
		return err
	}
	if len(raw) < aead.NonceSize() {
		return ErrInvalid
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], additionalData(p, kid))
	if err != nil {
		return ErrInvalid
	}
	var env envelope
	err = json.Unmarshal(plain, &env)
	if err != nil {
		return ErrInvalid
	}
	if env.Expires != 0 && k.now().After(time.Unix(env.Expires, 0).Add(clockSkew)) {
		return ErrExpired
	}
	err = json.Unmarshal(env.Value, v)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}

// aead returns the cipher for purpose p with key kid: AES-256-GCM with a key derived for that purpose.
func (k *Keyring) aead(p Purpose, kid string) (cipher.AEAD, error) {
	derived, err := hkdf.Key(sha256.New, k.keys[kid], nil, "speki seal "+string(p), 32)
	if err != nil {
		return nil, fmt.Errorf("derive key: %w", err)
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func additionalData(p Purpose, kid string) []byte {
	return []byte(version + "|" + string(p) + "|" + kid)
}
