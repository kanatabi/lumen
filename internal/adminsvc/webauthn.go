package adminsvc

// webauthn.go - the PASSKEY login method (the auth-method seam, C1
// round 2026-11-25). This is the first implementation of the normalized
// shape the estate agreed on: every login method is begin -> finish ->
// the ONE session mint; what varies is how the user proves who they are
// and what the browser consumes. When oauth:github lands later it adds
// a sibling file implementing the same shape - NOT a second mint, NOT
// new session semantics, NOT a route sprawl.
//
// Discoverable credentials (passkeys, the owner's Bitwarden flow): the
// login begin carries NO username - the authenticator offers the
// credentials it holds for this rp.id, and the assertion's userHandle
// IS the users.id surrogate.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/kanatabi/lumen/internal/auth"
	"github.com/kanatabi/lumen/internal/session"
	"github.com/kanatabi/lumen/internal/store"
)

// Sentinels handlers map onto statuses.
var (
	ErrPasskeyState     = errors.New("passkey challenge invalid or expired") // 400: the client restarts the flow
	ErrPasskeyNotFound  = errors.New("passkey not registered")               // folds into the 401 voice
	ErrPasskeyExists    = errors.New("passkey already registered")           // 409: the credential id is unique
	ErrPasskeyLabel     = errors.New("passkey label too long")               // 400: the label is user copy
	ErrPasskeyForbidden = errors.New("passkeys not enabled")                 // the feature is off
)

// EnableWebAuthn turns the passkey method ON (cmd wiring reads the
// config; both values empty = off, the methods endpoint omits it, the
// login UI degrades gracefully). origins is the comma-separated list of
// browser origins the ceremony accepts — ONE registrable-domain rp-id
// serves every environment's mellon (console + staging-console), the
// role-split design: identity shared, content per env.
func (s *Service) EnableWebAuthn(rpID, origins string) error {
	if rpID == "" {
		return fmt.Errorf("webauthn: empty rp id")
	}
	if origins == "" {
		origins = "https://" + rpID
	}
	list := make([]string, 0, 4)
	for _, o := range strings.Split(origins, ",") {
		if trimmed := strings.TrimSpace(o); trimmed != "" {
			list = append(list, trimmed)
		}
	}
	if len(list) == 0 {
		return fmt.Errorf("webauthn: empty origins")
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPOrigins:     list,
		RPDisplayName: "gofar; console",
	})
	if err != nil {
		return err
	}
	s.wa = wa
	return nil
}

// WebAuthnEnabled answers the methods endpoint (the portal's login UI
// is data-driven: a button per enabled method).
func (s *Service) WebAuthnEnabled() bool { return s.wa != nil }

// waUser adapts the store's auth row + credentials into the library's
// User interface. WebAuthnID = users.id BYTES (the surrogate key was
// pre-declared to be exactly this: the credential's user handle).
type waUser struct {
	id    []byte
	name  string
	creds []webauthn.Credential
}

func (u waUser) WebAuthnID() []byte                         { return u.id }
func (u waUser) WebAuthnName() string                       { return u.name }
func (u waUser) WebAuthnDisplayName() string                { return u.name }
func (u waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }
func (u waUser) WebAuthnIcon() string                       { return "" }

// toLib converts the store's raw rows into the library's credential
// records (the storage stays protocol-shallow on purpose).
func toLib(rows []store.WebAuthnCredential) []webauthn.Credential {
	out := make([]webauthn.Credential, 0, len(rows))
	for _, r := range rows {
		id, err := base64.RawURLEncoding.DecodeString(r.ID)
		if err != nil {
			continue
		}
		transport := make([]protocol.AuthenticatorTransport, 0, len(r.Transports))
		for _, t := range r.Transports {
			transport = append(transport, protocol.AuthenticatorTransport(t))
		}
		out = append(out, webauthn.Credential{
			ID:              id,
			PublicKey:       r.PublicKey,
			AttestationType: "none",
			Transport:       transport,
			Authenticator:   webauthn.Authenticator{SignCount: r.SignCount},
		})
	}
	return out
}

// PasskeyLoginBegin is the LOGIN door's begin: a discoverable challenge
// (no username - the authenticator knows). The state token carries the
// session data signed; nothing persists.
func (s *Service) PasskeyLoginBegin(ctx context.Context) (string, protocol.CredentialAssertion, error) {
	if s.wa == nil {
		return "", protocol.CredentialAssertion{}, ErrPasskeyForbidden
	}
	options, sessionData, err := s.wa.BeginDiscoverableLogin()
	if err != nil {
		return "", protocol.CredentialAssertion{}, fmt.Errorf("passkey begin: %w", err)
	}
	state, err := auth.SignState(s.stateSecret(), "passkey-login", "passkey", sessionData)
	if err != nil {
		return "", protocol.CredentialAssertion{}, err
	}
	return state, *options, nil
}

// PasskeyLoginFinish is the normalized POST-verify path: verify the
// assertion, resolve the user THROUGH the credential, then the SAME
// throttle/disabled/mint treatment the password login gives.
func (s *Service) PasskeyLoginFinish(ctx context.Context, stateToken string, body []byte, ip string) (string, int, Principal, error) {
	if s.wa == nil {
		return "", 0, Principal{}, ErrPasskeyForbidden
	}
	// the ip throttle keys BEFORE any verification: passkeys are a
	// login door like any other - the same flood discipline applies
	ipKey := "ip:" + ip
	if ok, wait := s.limiter.Allow(ipKey); !ok {
		return "", 0, Principal{}, &ThrottledError{RetryAfter: wait}
	}
	var sessionData webauthn.SessionData
	if err := auth.VerifyState(s.stateSecret(), "passkey-login", "passkey", stateToken, &sessionData); err != nil {
		return "", 0, Principal{}, fmt.Errorf("%w: %v", ErrPasskeyState, err)
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(body))
	if err != nil {
		return "", 0, Principal{}, fmt.Errorf("%w: %v", ErrInvalidCredentials, err)
	}
	// the discoverable handler: the assertion names a credential; the
	// user comes WITH it. Unknown credential = the ONE denial voice.
	credential, err := s.wa.ValidateDiscoverableLogin(
		func(rawID, userHandle []byte) (webauthn.User, error) {
			row, err := s.store.WebAuthnCredentialByID(ctx, base64.RawURLEncoding.EncodeToString(rawID))
			if err != nil {
				return nil, ErrPasskeyNotFound
			}
			// the row's user MUST own the assertion's userHandle (the
			// 16-byte users.id) - a mismatched pair is a forged
			// assertion, not a login
			uidStr, err := uuidString(userHandle)
			if err != nil || row.UserID != uidStr {
				return nil, ErrPasskeyNotFound
			}
			authRow, ok, err := s.store.AuthForID(ctx, uidStr)
			if err != nil || !ok {
				return nil, ErrPasskeyNotFound
			}
			uid, err := uuidBytes(authRow.ID)
			if err != nil {
				return nil, ErrPasskeyNotFound
			}
			creds, err := s.store.WebAuthnCredentialsFor(ctx, row.UserID)
			if err != nil {
				return nil, ErrPasskeyNotFound
			}
			return waUser{id: uid, name: authRow.Username, creds: toLib(creds)}, nil
		},
		sessionData, parsed,
	)
	if err != nil {
		s.limiter.Fail(ipKey)
		return "", 0, Principal{}, fmt.Errorf("%w: validate: %v", ErrInvalidCredentials, err) // ONE indistinguishable message
	}
	// resolve the user row the assertion resolved to
	row, err := s.store.WebAuthnCredentialByID(ctx, base64.RawURLEncoding.EncodeToString(credential.ID))
	if err != nil {
		return "", 0, Principal{}, ErrInvalidCredentials
	}
	authRow, ok, err := s.store.AuthForID(ctx, row.UserID)
	if err != nil || !ok {
		return "", 0, Principal{}, ErrInvalidCredentials
	}
	if authRow.Disabled {
		return "", 0, Principal{}, ErrDisabled // the same distinct 403 as the password door
	}
	// the CLONE tripwire: the library sets CloneWarning when the assertion's
	// counter did not advance (0 -> 0 never warns, so counter-less passkeys
	// are unaffected). A cloned authenticator must not mint — refuse, and
	// leave the audit line; the stored counter is untouched.
	if credential.Authenticator.CloneWarning {
		slog.Warn("session.deny.passkey.clone",
			"user", authRow.Username, "credential", row.ID, "ip", ip,
			"stored", row.SignCount, "asserted", credential.Authenticator.SignCount)
		return "", 0, Principal{}, ErrInvalidCredentials
	}
	// the replay tripwire: the counter moved, persist it
	if err := s.store.WebAuthnTouchSignCount(ctx, row.ID, credential.Authenticator.SignCount); err != nil {
		return "", 0, Principal{}, err
	}
	token, expiresIn, err := mintFor(s.jwtSecret, authRow)
	if err != nil {
		return "", 0, Principal{}, err
	}
	s.limiter.Success(ipKey)
	return token, expiresIn, Principal{
		Username: authRow.Username, ID: authRow.ID, Scopes: authRow.Scopes,
		MustChange: authRow.MustChange, Disabled: authRow.Disabled,
	}, nil
}

// PasskeyRegisterBegin is the SETUP door's begin (authenticated - the
// credential binds to the SESSION's user). Existing credentials ride
// the exclude list so a second passkey never shadows the first.
func (s *Service) PasskeyRegisterBegin(ctx context.Context, p Principal) (string, protocol.CredentialCreation, error) {
	if s.wa == nil {
		return "", protocol.CredentialCreation{}, ErrPasskeyForbidden
	}
	uid, err := uuidBytes(p.ID)
	if err != nil {
		return "", protocol.CredentialCreation{}, err
	}
	creds, err := s.store.WebAuthnCredentialsFor(ctx, p.ID)
	if err != nil {
		return "", protocol.CredentialCreation{}, err
	}
	user := waUser{id: uid, name: p.Username, creds: toLib(creds)}
	exclusions := make([]protocol.CredentialDescriptor, 0, len(user.creds))
	for _, c := range user.creds {
		exclusions = append(exclusions, protocol.CredentialDescriptor{
			Type:         protocol.PublicKeyCredentialType,
			CredentialID: protocol.URLEncodedBase64(c.ID),
			Transport:    c.Transport,
		})
	}
	options, sessionData, err := s.wa.BeginRegistration(user,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred, // discoverable: the usernameless login
			UserVerification: protocol.VerificationPreferred,
		}),
		webauthn.WithExclusions(exclusions),
	)
	if err != nil {
		return "", protocol.CredentialCreation{}, fmt.Errorf("passkey register begin: %w", err)
	}
	state, err := auth.SignState(s.stateSecret(), "passkey-register", "passkey", sessionData)
	if err != nil {
		return "", protocol.CredentialCreation{}, err
	}
	return state, *options, nil
}

// PasskeyRegisterFinish verifies the attestation and stores the
// credential. Registering does NOT bump token_version: it changes what
// can AUTHENTICATE, not what the session is allowed to do.
func (s *Service) PasskeyRegisterFinish(ctx context.Context, p Principal, stateToken, label string, body []byte) (store.WebAuthnCredential, error) {
	if s.wa == nil {
		return store.WebAuthnCredential{}, ErrPasskeyForbidden
	}
	var sessionData webauthn.SessionData
	if err := auth.VerifyState(s.stateSecret(), "passkey-register", "passkey", stateToken, &sessionData); err != nil {
		return store.WebAuthnCredential{}, fmt.Errorf("%w: %v", ErrPasskeyState, err)
	}
	uid, err := uuidBytes(p.ID)
	if err != nil {
		return store.WebAuthnCredential{}, err
	}
	creds, err := s.store.WebAuthnCredentialsFor(ctx, p.ID)
	if err != nil {
		return store.WebAuthnCredential{}, err
	}
	user := waUser{id: uid, name: p.Username, creds: toLib(creds)}
	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(body))
	if err != nil {
		return store.WebAuthnCredential{}, fmt.Errorf("%w: %v", ErrInvalidCredentials, err)
	}
	credential, err := s.wa.CreateCredential(user, sessionData, parsed)
	if err != nil {
		return store.WebAuthnCredential{}, fmt.Errorf("%w: %v", ErrInvalidCredentials, err)
	}
	if label == "" {
		label = "passkey"
	}
	if utf8.RuneCountInString(label) > 64 {
		return store.WebAuthnCredential{}, ErrPasskeyLabel
	}
	transport := make([]string, 0, len(credential.Transport))
	for _, t := range credential.Transport {
		transport = append(transport, string(t))
	}
	row := store.WebAuthnCredential{
		ID:         base64.RawURLEncoding.EncodeToString(credential.ID),
		UserID:     p.ID,
		PublicKey:  credential.PublicKey,
		SignCount:  credential.Authenticator.SignCount,
		Transports: transport,
		Label:      label,
	}
	createdAt, err := s.store.WebAuthnCreateCredential(ctx, row)
	if err != nil {
		if errors.Is(err, store.ErrCredentialExists) {
			return store.WebAuthnCredential{}, ErrPasskeyExists
		}
		return store.WebAuthnCredential{}, err
	}
	row.CreatedAt = createdAt
	return row, nil
}

// Passkeys is the settings roster (the user's own rows only).
func (s *Service) Passkeys(ctx context.Context, username string) ([]store.WebAuthnCredential, error) {
	p, ok, err := s.store.AuthFor(ctx, username)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrInvalidCredentials
	}
	return s.store.WebAuthnCredentialsFor(ctx, p.ID)
}

// PasskeyDelete removes one of the user's own credentials. Never a
// lockout: every user holds a password by construction (accounts are
// admin-created), so unlinking needs no guard.
func (s *Service) PasskeyDelete(ctx context.Context, username, id string) (bool, error) {
	p, ok, err := s.store.AuthFor(ctx, username)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, ErrInvalidCredentials
	}
	return s.store.WebAuthnDelete(ctx, p.ID, id)
}

// stateSecret is the session secret doing double duty: the state token
// is a signed, TTL-bound challenge carriage with an isolated purpose -
// it can never become a session (the purpose check refuses it).
func (s *Service) stateSecret() []byte { return []byte(s.jwtSecret) }

// mintFor is the shared mint shape the normalized finish paths call
// (the password Login mints identically - one mint, one door).
func mintFor(secret string, a store.Auth) (string, int, error) {
	return session.MintSession(secret, a.Username, a.TokenVersion, a.Scopes)
}

// uuidString formats the 16-byte user handle back into the dashed
// UUID string (the surrogate key's storage shape).
func uuidString(handle []byte) (string, error) {
	if len(handle) != 16 {
		return "", fmt.Errorf("user handle shape")
	}
	h := hex.EncodeToString(handle)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// uuidBytes parses the surrogate id into its 16 bytes (the WebAuthn
// user handle). The adminsvc imports the uuid codec from the store's
// dependency tree.
func uuidBytes(id string) ([]byte, error) {
	// base64-free: hex/uuid parse via pgx's codec would import more; the
	// uuid shape is 36 chars with dashes
	if len(id) != 36 {
		return nil, fmt.Errorf("user id shape")
	}
	out := make([]byte, 16)
	hexIdx := 0
	trimmed := strings.ReplaceAll(id, "-", "")
	if len(trimmed) != 32 {
		return nil, fmt.Errorf("user id shape")
	}
	for i := 0; i < 16; i++ {
		hi := hexVal(trimmed[hexIdx])
		lo := hexVal(trimmed[hexIdx+1])
		if hi < 0 || lo < 0 {
			return nil, fmt.Errorf("user id shape")
		}
		out[i] = byte(hi<<4 | lo)
		hexIdx += 2
	}
	return out, nil
}

func hexVal(b byte) int {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0')
	case b >= 'a' && b <= 'f':
		return int(b-'a') + 10
	case b >= 'A' && b <= 'F':
		return int(b-'A') + 10
	}
	return -1
}
