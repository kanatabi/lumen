-- 0013_webauthn_flags.sql — the credential's WebAuthn FLAGS (the passkey
-- audit's live catch, 2026-11-27). The library stores the flags the
-- registration ceremony reported and VALIDATES the BackupEligible flag on
-- every login:
--
--   if credential.Flags.BackupEligible != authData.Flags.HasBackupEligible() {
--       return "BackupEligible flag inconsistency detected during login validation"
--   }
--
-- A synced passkey (Bitwarden, iCloud, a phone's keychain) is BE=1; a
-- device-bound one is BE=0. A store that keeps only the public key
-- reconstructs BE=0 for everyone, so every synced passkey registers fine
-- and then 401s forever. BE must NEVER change (the library's own words);
-- BS (backed up) may change and is refreshed on each successful login.
--
-- Rows written before this migration carry the defaults and cannot log in;
-- deleting and re-registering them is the honest repair (we cannot know
-- their BE from the row).
ALTER TABLE webauthn_credentials
	ADD COLUMN IF NOT EXISTS backup_eligible BOOLEAN NOT NULL DEFAULT false,
	ADD COLUMN IF NOT EXISTS backup_state    BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN webauthn_credentials.backup_eligible IS
	'the WebAuthn BE flag recorded at registration — invariant; the login validator compares it';
COMMENT ON COLUMN webauthn_credentials.backup_state IS
	'the WebAuthn BS flag — refresh on login (a backup may happen later); BE=0 + BS=1 is illegal';
