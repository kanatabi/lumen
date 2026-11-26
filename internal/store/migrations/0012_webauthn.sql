-- 0012_webauthn.sql — the passkey credentials (the auth-method seam,
-- C1 round 2026-11-25): a user's WebAuthn credentials live HERE, admin-
-- plane lineage (the users table's home; the cms subset never sees this
-- file). One row per registered authenticator: the credential id is the
-- primary key (base64url, as the browser addresses it), the public key
-- is the only half we hold (the private key lives in the authenticator
-- — Bitwarden/iCloud/a key), and the sign counter is the replay
-- tripwire (cloned-credential detection is the library's job). A user
-- may hold MANY (phone + Bitwarden + a key someday). No provider column
-- — this table is passkey-shaped on purpose; OAuth identities get their
-- own shape when they land (the seam is the flow, not the storage).
--
-- users.id is the FK (the RBAC spine's surrogate key — provider tables
-- were pre-declared to key on it).
CREATE TABLE webauthn_credentials (
	id TEXT PRIMARY KEY,          -- base64url credential id (browser-addressed)
	user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	public_key BYTEA NOT NULL,    -- COSE key (EC2/Ed25519 per attestation)
	sign_count BIGINT NOT NULL DEFAULT 0,
	transports TEXT[] NOT NULL DEFAULT '{}',  -- usb/nfc/ble/internal/hybrid
	label TEXT NOT NULL,          -- the user's own name for the device
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	last_used_at TIMESTAMPTZ
);

CREATE INDEX webauthn_credentials_user_idx ON webauthn_credentials(user_id);
