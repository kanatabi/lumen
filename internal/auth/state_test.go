package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStateRoundtrip(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	type body struct {
		Challenge string `json:"c"`
	}
	token, err := SignState(secret, "passkey-login", "passkey", body{Challenge: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	var out body
	if err := VerifyState(secret, "passkey-login", "passkey", token, &out); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if out.Challenge != "abc" {
		t.Fatalf("challenge = %q", out.Challenge)
	}
}

func TestStateTamper(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	type body struct {
		Challenge string `json:"c"`
	}
	token, _ := SignState(secret, "passkey-login", "passkey", body{Challenge: "abc"})
	// flip the payload's last character
	parts := strings.Split(token, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[0])
	payload[len(payload)-1] ^= 0xFF
	tampered := base64.RawURLEncoding.EncodeToString(payload) + "." + parts[1]
	var out body
	if err := VerifyState(secret, "passkey-login", "passkey", tampered, &out); err == nil {
		t.Fatal("tampered token verified")
	}
}

func TestStatePurposeIsolation(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	type body struct {
		Challenge string `json:"c"`
	}
	token, _ := SignState(secret, "passkey-login", "passkey", body{Challenge: "abc"})
	var out body
	if err := VerifyState(secret, "passkey-link", "passkey", token, &out); err == nil {
		t.Fatal("a login state finished a link")
	}
}

func TestStateExpiry(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	token, _ := SignState(secret, "passkey-login", "passkey", struct{}{})
	// the TTL is minutes — an expired token is the same verification shape;
	// verify via a crafted far-past expiry by direct payload surgery
	parts := strings.Split(token, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[0])
	sig := parts[1]
	// re-sign with an expired body
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	body["e"] = time.Now().Add(-time.Hour).Unix()
	raw, _ := json.Marshal(body)
	mac := hmac.New(sha256.New, secret)
	mac.Write(raw)
	forged := base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	var out struct{}
	if err := VerifyState(secret, "passkey-login", "passkey", forged, &out); err == nil {
		t.Fatal("expired token verified")
	}
	_ = sig
}
