// Package auth — the login-method seam (the C1 round, 2026-11-25).
// Every login/link method (password today, passkey now, oauth later)
// normalizes to begin -> finish -> the ONE session mint; what varies is
// how the user proves who they are and what the browser consumes. This
// package holds the method-agnostic half: the signed begin-state token
// (stateless challenge carriage — replica-safe, replay-proof) and the
// method registry shape.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrStateInvalid = errors.New("auth state invalid")
	ErrStateExpired = errors.New("auth state expired")
)

// stateTTL bounds a begin->finish round trip: minutes, not sessions. A
// stale challenge is a dead challenge — the client restarts the flow.
const stateTTL = 3 * time.Minute

// statePayload is the HMAC'd body. Purpose isolates the flows (a login
// state can never finish a link and vice versa); Method names the
// method that minted it; Nonce makes every token unique.
type statePayload struct {
	Purpose   string `json:"p"`
	Method    string `json:"m"`
	Nonce     string `json:"n"`
	ExpiresAt int64  `json:"e"`
	Body      any    `json:"b,omitempty"`
}

// SignState serializes payload (method-shaped: a webauthn.SessionData,
// an oauth state bundle, ...), HMACs it with the session secret, and
// returns the opaque token the client carries back to finish. The
// challenge itself is public knowledge — the signature is the
// TAMPERING guard, not secrecy.
func SignState(secret []byte, purpose, method string, payload any) (string, error) {
	body := statePayload{
		Purpose:   purpose,
		Method:    method,
		Nonce:     mustNonce(),
		ExpiresAt: time.Now().Add(stateTTL).Unix(),
		Body:      payload,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("state marshal: %w", err)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(raw)
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(raw) + "." +
		base64.RawURLEncoding.EncodeToString(sig), nil
}

// VerifyState parses + authenticates + freshness-checks a state token
// into out (the same method-shaped struct SignState received). Purpose
// and method must match what the caller expects — a login state can
// never finish a link, a passkey state can never finish an oauth flow.
func VerifyState(secret []byte, purpose, method, token string, out any) error {
	raw, sig, ok := strings2(token)
	if !ok {
		return fmt.Errorf("%w: shape", ErrStateInvalid)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(raw)
	if !hmac.Equal(mac.Sum(nil), sig) {
		return fmt.Errorf("%w: signature", ErrStateInvalid)
	}
	var body statePayload
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("%w: json", ErrStateInvalid)
	}
	if body.Purpose != purpose || body.Method != method {
		return fmt.Errorf("%w: purpose/method", ErrStateInvalid)
	}
	if time.Now().Unix() > body.ExpiresAt {
		return fmt.Errorf("%w: expired", ErrStateExpired)
	}
	repacked, err := json.Marshal(body.Body)
	if err != nil {
		return fmt.Errorf("%w: repack", ErrStateInvalid)
	}
	if err := json.Unmarshal(repacked, out); err != nil {
		return fmt.Errorf("%w: body shape", ErrStateInvalid)
	}
	return nil
}

func strings2(token string) ([]byte, []byte, bool) {
	parts := make([][]byte, 0, 2)
	start := 0
	for i := 0; i <= len(token); i++ {
		if i == len(token) || token[i] == '.' {
			part, err := base64.RawURLEncoding.DecodeString(token[start:i])
			if err != nil {
				return nil, nil, false
			}
			parts = append(parts, part)
			start = i + 1
		}
	}
	if len(parts) != 2 {
		return nil, nil, false
	}
	return parts[0], parts[1], true
}

func mustNonce() string {
	n := make([]byte, 12)
	if _, err := rand.Read(n); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(n)
}
