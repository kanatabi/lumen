package api_test

// webauthn_integration_test.go — the passkey flow against REAL Postgres
// (the LUMEN_INTEGRATION gate): a FAKE AUTHENTICATOR — pure Go, an ECDSA
// P-256 key, hand-built authenticator data, CBOR via the library's own
// encoder — drives register -> login -> MINT end to end, plus the
// denials the audit demanded: a replayed assertion (clone tripwire),
// tampered state, forged handle, disabled user, duplicate registration,
// the label cap, scoped delete, and the unconfigured plane's 404.
// The library owns the protocol crypto; this test proves OUR wiring.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
)

// fakeAuthenticator is a real WebAuthn credential: a private key the
// server never sees, a credential id, and the counter the replay
// tripwire watches.
type fakeAuthenticator struct {
	key    *ecdsa.PrivateKey
	credID []byte
	rpID   string
	origin string
	userID []byte // the user handle the assertions carry
	count  uint32
}

func newFakeAuth(t *testing.T, rpID, origin string, userID []byte) *fakeAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credID := make([]byte, 32)
	if _, err := rand.Read(credID); err != nil {
		t.Fatal(err)
	}
	return &fakeAuthenticator{key: key, credID: credID, rpID: rpID, origin: origin, userID: userID}
}

func (f *fakeAuthenticator) clientData(ty, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": ty, "challenge": challenge, "origin": f.origin, "crossOrigin": false,
	})
	return b
}

func (f *fakeAuthenticator) rpIDHash() []byte {
	h := sha256.Sum256([]byte(f.rpID))
	return h[:]
}

// coseKey is the public key in COSE form (EC2, P-256, alg ES256 = -7).
func (f *fakeAuthenticator) coseKey() []byte {
	pub := make([]byte, 32)
	copy(pub[32-len(f.key.PublicKey.X.Bytes()):], f.key.PublicKey.X.Bytes())
	pri := make([]byte, 32)
	copy(pri[32-len(f.key.PublicKey.Y.Bytes()):], f.key.PublicKey.Y.Bytes())
	b, err := webauthncbor.Marshal(map[int]any{
		1: 2, 3: -7, -1: 1, -2: pub, -3: pri,
	})
	if err != nil {
		panic(err)
	}
	return b
}

func newAuthData(rpHash []byte, flags byte, count uint32, withCred bool, credID, cose []byte) []byte {
	buf := bytes.NewBuffer(nil)
	buf.Write(rpHash)
	buf.WriteByte(flags)
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], count)
	buf.Write(c[:])
	if withCred {
		buf.Write(make([]byte, 16)) // aaguid: none
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(credID)))
		buf.Write(l[:])
		buf.Write(credID)
		buf.Write(cose)
	}
	return buf.Bytes()
}

// registerResponse builds the browser's creation JSON (fmt "none" —
// self-attestation; the rp verifies the key, not the vendor).
func (f *fakeAuthenticator) registerResponse(challenge string) string {
	auth := newAuthData(f.rpIDHash(), 0x41, f.count, true, f.credID, f.coseKey())
	attObj, err := webauthncbor.Marshal(map[string]any{
		"fmt": "none", "attStmt": map[string]any{}, "authData": auth,
	})
	if err != nil {
		panic(err)
	}
	cdj := f.clientData("webauthn.create", challenge)
	id := base64.RawURLEncoding.EncodeToString(f.credID)
	b, _ := json.Marshal(map[string]any{
		"id": id, "rawId": id, "type": "public-key",
		"response": map[string]any{
			"attestationObject": base64.RawURLEncoding.EncodeToString(attObj),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(cdj),
		},
	})
	return string(b)
}

// assertion builds the browser's login JSON and advances the counter.
func (f *fakeAuthenticator) assertion(challenge string) string {
	f.count++
	auth := newAuthData(f.rpIDHash(), 0x05, f.count, false, nil, nil)
	cdj := f.clientData("webauthn.get", challenge)
	cdHash := sha256.Sum256(cdj)
	sigInput := append(append([]byte{}, auth...), cdHash[:]...)
	digest := sha256.Sum256(sigInput)
	sig, err := ecdsa.SignASN1(rand.Reader, f.key, digest[:])
	if err != nil {
		panic(err)
	}
	id := base64.RawURLEncoding.EncodeToString(f.credID)
	b, _ := json.Marshal(map[string]any{
		"id": id, "rawId": id, "type": "public-key",
		"response": map[string]any{
			"authenticatorData": base64.RawURLEncoding.EncodeToString(auth),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(cdj),
			"signature":         base64.RawURLEncoding.EncodeToString(sig),
			"userHandle":        base64.RawURLEncoding.EncodeToString(f.userID),
		},
	})
	return string(b)
}

// beginRegister/login helpers pull the challenge + state out of the
// begin responses (the options ride the lib's publicKey envelope).
type beginOut struct {
	State   string `json:"state"`
	Options struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	} `json:"options"`
}

func unwrap(t *testing.T, res []byte) beginOut {
	t.Helper()
	var out beginOut
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatalf("begin unwrap: %v", err)
	}
	if out.State == "" || out.Options.PublicKey.Challenge == "" {
		t.Fatalf("begin response missing state/challenge: %s", res)
	}
	return out
}

func TestWebAuthnFlow(t *testing.T) {
	h := newAdminHarness(t)
	const rpID, origin = "console.gofar.id.test", "https://console.gofar.id.test"
	if err := h.adminSvc.EnableWebAuthn(rpID, origin); err != nil {
		t.Fatal(err)
	}
	h.rebuild()

	// the methods endpoint advertises BOTH (data-driven login UI)
	code, res := h.do("GET", "/admin/session/auth/methods", "", "")
	if code != 200 || !bytes.Contains(res, []byte("passkey")) {
		t.Fatalf("methods: %d %s", code, res)
	}

	// the fake's userHandle = the user's surrogate id (16 bytes of the UUID)
	var uidStr string
	if err := h.pool.QueryRow(context.Background(), `SELECT id::text FROM users WHERE username='fzr-test'`).Scan(&uidStr); err != nil {
		t.Fatal(err)
	}
	uidStr = strings.ReplaceAll(uidStr, "-", "")
	uidBytes := make([]byte, 16)
	for i := 0; i < 16; i++ {
		if _, err := fmt.Sscanf(uidStr[i*2:i*2+2], "%02x", &uidBytes[i]); err != nil {
			t.Fatal(err)
		}
	}
	fake := newFakeAuth(t, rpID, origin, uidBytes)

	// REGISTER (authenticated): begin -> the fake creates -> finish -> 201
	code, res = h.admin("POST", "/admin/session/webauthn/register/begin", "")
	if code != 200 {
		t.Fatalf("register begin: %d %s", code, res)
	}
	begin := unwrap(t, res)
	code, res = h.admin("POST", "/admin/session/webauthn/register/finish",
		`{"label":"Bitwarden","state":"`+begin.State+`","response":`+fake.registerResponse(begin.Options.PublicKey.Challenge)+`}`)
	if code != 201 {
		t.Fatalf("register finish: %d %s", code, res)
	}
	if !bytes.Contains(res, []byte("Bitwarden")) {
		t.Fatalf("roster row missing: %s", res)
	}

	// the roster lists it
	if code, res = h.admin("GET", "/admin/session/webauthn", ""); code != 200 || !bytes.Contains(res, []byte("Bitwarden")) {
		t.Fatalf("roster: %d %s", code, res)
	}

	// LOGIN (public): begin -> the fake asserts -> finish -> THE MINT
	code, res = h.do("POST", "/admin/session/webauthn/login/begin", "", "")
	if code != 200 {
		t.Fatalf("login begin: %d %s", code, res)
	}
	begin = unwrap(t, res)
	loginBody := `{"state":"` + begin.State + `","response":` + fake.assertion(begin.Options.PublicKey.Challenge) + `}`
	code, res = h.do("POST", "/admin/session/webauthn/login/finish", loginBody, "")
	if code != 200 {
		t.Fatalf("login finish: %d %s", code, res)
	}
	var granted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(res, &granted); err != nil || granted.Token == "" {
		t.Fatalf("no token minted: %s", res)
	}
	// the minted session is REAL: it introspects
	if code, res = h.do("GET", "/admin/session", "", granted.Token); code != 200 || !bytes.Contains(res, []byte("fzr-test")) {
		t.Fatalf("whoami with the passkey-minted token: %d %s", code, res)
	}

	// REPLAY: the SAME assertion again. The library's counter check flags
	// the clone (0 -> 0 never warns, N -> N does) and the mint is refused —
	// a stolen assertion is not a second session.
	code, res = h.do("POST", "/admin/session/webauthn/login/finish", loginBody, "")
	if code != 401 {
		t.Fatalf("replayed assertion: %d %s", code, res)
	}

	// LABEL: user copy is capped (64 runes) before the row is written
	code, res = h.admin("POST", "/admin/session/webauthn/register/begin", "")
	if code != 200 {
		t.Fatalf("label begin: %d %s", code, res)
	}
	begin = unwrap(t, res)
	backup := newFakeAuth(t, rpID, origin, uidBytes)
	code, res = h.admin("POST", "/admin/session/webauthn/register/finish",
		`{"label":"`+strings.Repeat("x", 65)+`","state":"`+begin.State+`","response":`+backup.registerResponse(begin.Options.PublicKey.Challenge)+`}`)
	if code != 400 {
		t.Fatalf("label cap: %d %s", code, res)
	}

	// a SECOND authenticator registers cleanly (multiple passkeys per user)
	code, res = h.admin("POST", "/admin/session/webauthn/register/begin", "")
	begin = unwrap(t, res)
	code, res = h.admin("POST", "/admin/session/webauthn/register/finish",
		`{"label":"Backup key","state":"`+begin.State+`","response":`+backup.registerResponse(begin.Options.PublicKey.Challenge)+`}`)
	if code != 201 {
		t.Fatalf("second register: %d %s", code, res)
	}

	// DUPLICATE: the exclude list is a hint — the INSERT is the guard (409,
	// never a 500)
	code, res = h.admin("POST", "/admin/session/webauthn/register/begin", "")
	begin = unwrap(t, res)
	code, res = h.admin("POST", "/admin/session/webauthn/register/finish",
		`{"label":"Backup key","state":"`+begin.State+`","response":`+backup.registerResponse(begin.Options.PublicKey.Challenge)+`}`)
	if code != 409 {
		t.Fatalf("duplicate register: %d %s", code, res)
	}

	// TAMPERED STATE: a flipped token is a 400, never a mint
	code, res = h.do("POST", "/admin/session/webauthn/login/begin", "", "")
	begin = unwrap(t, res)
	tampered := begin.State[:len(begin.State)-4] + "AAAA"
	code, res = h.do("POST", "/admin/session/webauthn/login/finish",
		`{"state":"`+tampered+`","response":`+fake.assertion(begin.Options.PublicKey.Challenge)+`}`, "")
	if code != 400 {
		t.Fatalf("tampered state: %d %s", code, res)
	}

	// FORGED USER HANDLE: a different handle than the row's owner is the
	// ONE denial voice (401, indistinguishable)
	forged := newFakeAuth(t, rpID, origin, nil)
	*forged = *fake // the same credential key... but a foreign handle
	forged.userID = make([]byte, 16)
	for i := range forged.userID {
		forged.userID[i] = 0xEE
	}
	code, res = h.do("POST", "/admin/session/webauthn/login/begin", "", "")
	begin = unwrap(t, res)
	code, res = h.do("POST", "/admin/session/webauthn/login/finish",
		`{"state":"`+begin.State+`","response":`+forged.assertion(begin.Options.PublicKey.Challenge)+`}`, "")
	if code != 401 {
		t.Fatalf("forged handle: %d %s", code, res)
	}

	// DELETE is scoped to the caller's own row and the roster reflects it
	backupID := base64.RawURLEncoding.EncodeToString(backup.credID)
	if code, res = h.admin("DELETE", "/admin/session/webauthn/"+backupID, ""); code != 204 {
		t.Fatalf("delete: %d %s", code, res)
	}
	if code, res = h.admin("GET", "/admin/session/webauthn", ""); code != 200 ||
		!bytes.Contains(res, []byte("Bitwarden")) || bytes.Contains(res, []byte("Backup key")) {
		t.Fatalf("roster after delete: %d %s", code, res)
	}

	// DISABLED: the passkey door respects the flag (the same distinct 403)
	if code, _ = h.admin("POST", "/admin/users/fzr-test/disable", ""); code != 200 {
		t.Fatalf("disable: %d", code)
	}
	t.Cleanup(func() { _, _ = h.admin("POST", "/admin/users/fzr-test/enable", "") })
	code, res = h.do("POST", "/admin/session/webauthn/login/begin", "", "")
	begin = unwrap(t, res)
	code, res = h.do("POST", "/admin/session/webauthn/login/finish",
		`{"state":"`+begin.State+`","response":`+fake.assertion(begin.Options.PublicKey.Challenge)+`}`, "")
	if code != 403 {
		t.Fatalf("disabled via passkey: %d %s", code, res)
	}
}

var _ = protocol.AuthenticatorTransport("internal") // the transports ride the roster

// TestWebAuthnDisabled proves the honest-absence shape: an admin plane with
// no relying-party identity serves NO passkey route (404 "no such method"),
// and the methods endpoint omits it — the portal's button disappears.
func TestWebAuthnDisabled(t *testing.T) {
	h := newAdminHarness(t) // deliberately NOT enabled
	if code, res := h.do("GET", "/admin/session/auth/methods", "", ""); code != 200 ||
		bytes.Contains(res, []byte("passkey")) {
		t.Fatalf("methods on a disabled plane: %d %s", code, res)
	}
	if code, res := h.do("POST", "/admin/session/webauthn/login/begin", "", ""); code != 404 ||
		!bytes.Contains(res, []byte("no such method")) {
		t.Fatalf("disabled begin: %d %s", code, res)
	}
	if code, res := h.admin("POST", "/admin/session/webauthn/register/begin", ""); code != 404 {
		t.Fatalf("disabled register begin: %d %s", code, res)
	}
}
