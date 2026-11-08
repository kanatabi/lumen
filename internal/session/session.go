// Package session implements the JWT design (lumen §8, owner rulings
// 2026-11-08): HS256 PINNED at parse time — `alg` negotiation is the classic
// JWT foot-gun and this library config makes None/RS-confusion fail by
// construction, not by review. Claims {sub, iat, exp, tv, jti}; tv is the
// token-version revocation handle: verification is signature AND the live
// users.token_version row.
package session

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/argon2"
)

const TTLSeconds = 60 * 60 * 12 // 12h — owner ruling; re-login is the refresh story

type Claims struct {
	TV int `json:"tv"` // users.token_version at mint; must still match
	jwt.RegisteredClaims
}

// MintSession signs a session for (username, current token_version).
// The caller reads tv from the users row INSIDE the auth check it just
// passed — tokens assert facts, not hopes.
func MintSession(secret, username string, tv int) (string, int, error) {
	now := time.Now().UTC()
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", 0, err
	}
	claims := Claims{
		TV: tv,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   username,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(TTLSeconds * time.Second)),
			ID:        base64.RawURLEncoding.EncodeToString(nonce),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(secret))
	if err != nil {
		return "", 0, err
	}
	return s, TTLSeconds, nil
}

var ErrInvalidToken = errors.New("invalid token")

// Parse validates signature + expiry + algorithm. tv vs truth is the
// caller's (one indexed read — the point of the design).
func Parse(secret, tokenStr string) (*Claims, error) {
	claims := &Claims{}
	tok, err := jwt.ParseWithClaims(tokenStr, claims,
		func(t *jwt.Token) (any, error) {
			if t.Method != jwt.SigningMethodHS256 {
				return nil, fmt.Errorf("%w: alg %v", ErrInvalidToken, t.Header["alg"])
			}
			return []byte(secret), nil
		},
		jwt.WithValidMethods([]string{"HS256"}), // belt AND suspenders
		jwt.WithExpirationRequired(),
	)
	if err != nil || !tok.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

// ---- argon2id credentials (OWASP-parameterized; hash format is self-
// describing so parameters can evolve without breaking old rows) ----

const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // 64 MiB — the single-user budget luxury
	argonThreads = 2
	argonKeyLen  = 32
	saltLen      = 16
)

func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(sum)), nil
}

func VerifyPassword(hash, password string) bool {
	// format: $argon2id$v=19$m=,t=,p=$salt$hash → split gives
	// ["", "argon2id", "v=19", "m=,t=,p", salt, hash] — six fields
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	sum := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(want, sum) == 1
}
