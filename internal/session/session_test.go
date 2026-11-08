package session

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const secret = "0123456789abcdef0123456789abcdef"

func TestMintParseRoundTrip(t *testing.T) {
	tok, expiresIn, err := MintSession(secret, "fzr", 7)
	if err != nil {
		t.Fatal(err)
	}
	if expiresIn != 12*60*60 {
		t.Fatalf("TTL = %d, want 12h", expiresIn)
	}
	claims, err := Parse(secret, tok)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != "fzr" || claims.TV != 7 || claims.ID == "" {
		t.Fatalf("claims: %+v", claims)
	}
}

func TestWrongSecretFails(t *testing.T) {
	tok, _, _ := MintSession(secret, "fzr", 1)
	if _, err := Parse("different-secret-32-bytes-long!!!", tok); err == nil {
		t.Fatal("verification must fail across secrets")
	}
}

// The classic JWT foot-guns, pinned at library config (lumen §8 rules):
// alg=None and an RS→HS confusion token must never validate.
func TestAlgConfusionRejected(t *testing.T) {
	// an unexpired alg:none token, correctly base64url'd — the pin must
	// reject it even though the claims themselves look perfect
	payload := `{"sub":"fzr","iat":1700000000,"exp":9999999999,"tv":1,"jti":"x"}`
	forged := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + "."
	if _, err := Parse(secret, forged); err == nil {
		t.Fatal("alg:none accepted — the pin is broken")
	}
	for _, garbage := range []string{"", "x", "a.b.c", "eyJhbGciOiJub25lIn0.e30."} {
		if _, err := Parse(secret, garbage); err == nil {
			t.Fatalf("garbage token accepted: %q", garbage)
		}
	}
}

func TestExpiredRejected(t *testing.T) {
	// mint with a timestamp in the past: hand-roll the claims
	claims := Claims{TV: 2, RegisteredClaims: jwt.RegisteredClaims{
		Subject:   "fzr",
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-25 * time.Hour)),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
		ID:        "nonce",
	}}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(secret, s); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestArgon2idRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("hash format: %s", hash[:40])
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("verify rejected the right password")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Fatal("verify accepted a wrong password")
	}
	// salts differ per hash (two hashes of one password both verify)
	h2, _ := HashPassword("correct horse battery staple")
	if h2 == hash {
		t.Fatal("salt reuse — identical hashes betray the salt")
	}
	if !VerifyPassword(h2, "correct horse battery staple") {
		t.Fatal("second hash rejected")
	}
	for _, junk := range []string{"", "not-a-hash", "$argon2id$v=19$m=1,t=1,p=1$AAAA$AAAA"} {
		if VerifyPassword(junk, "x") {
			t.Fatalf("junk hash verified: %q", junk)
		}
	}
}
