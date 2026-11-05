package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// SensitiveColumnDecision documents the security decision for a database column
// that carries or touches authentication/secret material.
type SensitiveColumnDecision struct {
	Table       string
	Column      string
	Mechanism   string
	Searchable  bool
	Explanation string
}

// SensitiveColumnInventory is the authoritative inventory of every database
// column in SoroBeacon that holds or touches operator secrets or credentials,
// along with how it is protected at rest.
var SensitiveColumnInventory = []SensitiveColumnDecision{
	{
		Table:       "channels",
		Column:      "config",
		Mechanism:   "aead-aes-gcm",
		Searchable:  false,
		Explanation: "Holds webhook URLs, Slack/Telegram/Discord bot tokens, PagerDuty keys, and SMTP passwords; encrypted at rest via ConfigCipher (AES-GCM envelope).",
	},
	{
		Table:       "channel_signing_keys",
		Column:      "secret",
		Mechanism:   "aead-aes-gcm",
		Searchable:  false,
		Explanation: "Holds recoverable HMAC-SHA256 webhook signing secrets; encrypted at rest via ConfigCipher with explicit enc:v1: envelope prefix while key_id stays plaintext for indexed lookup.",
	},
	{
		Table:       "api_tokens",
		Column:      "token_hash",
		Mechanism:   "sha256-digest",
		Searchable:  true,
		Explanation: "Holds the irreversible SHA-256 hex digest of a 256-bit random bearer token; indexed for constant-time authentication lookup.",
	},
	{
		Table:       "api_tokens",
		Column:      "prefix",
		Mechanism:   "plaintext-display-prefix",
		Searchable:  true,
		Explanation: "Holds only the short display prefix of the token so operators can identify tokens in the UI without storing the secret.",
	},
	{
		Table:       "delivery_attempts",
		Column:      "response_snippet",
		Mechanism:   "redacted-plaintext",
		Searchable:  false,
		Explanation: "Truncated remote HTTP/SMTP response body; channel secrets are scrubbed before persistence.",
	},
}

// ReencryptSensitiveColumns re-encrypts up to batchSize plaintext or
// retired-key rows across channels.config and channel_signing_keys.secret under
// the store's current primary ConfigCipher key. It is resumable: each call
// processes rows with id > afterID and returns the number of rows rewritten and
// the highest row ID visited.
func (p *Postgres) ReencryptSensitiveColumns(ctx context.Context, afterID int64, batchSize int) (rewritten int, lastID int64, err error) {
	if p.cipher == nil {
		return 0, afterID, errors.New("cannot re-encrypt sensitive columns: no config cipher is configured")
	}
	if batchSize <= 0 {
		batchSize = 100
	}

	rows, err := p.pool.Query(ctx,
		`SELECT id, name, config FROM channels WHERE id > $1 ORDER BY id ASC LIMIT $2`,
		afterID, batchSize,
	)
	if err != nil {
		return 0, afterID, fmt.Errorf("scanning channels for re-encryption: %w", err)
	}
	type chanRow struct {
		id   int64
		name string
		raw  []byte
	}
	var pending []chanRow
	for rows.Next() {
		var r chanRow
		if err := rows.Scan(&r.id, &r.name, &r.raw); err != nil {
			rows.Close()
			return 0, afterID, err
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, afterID, err
	}

	lastID = afterID
	for _, r := range pending {
		if r.id > lastID {
			lastID = r.id
		}
		plaintext, err := configForRead(p.cipher, r.id, r.name, r.raw)
		if err != nil {
			return rewritten, lastID, err
		}
		sealed, err := configForWrite(p.cipher, r.id, r.name, plaintext)
		if err != nil {
			return rewritten, lastID, err
		}
		if _, err := p.pool.Exec(ctx, `UPDATE channels SET config = $1 WHERE id = $2`, sealed, r.id); err != nil {
			return rewritten, lastID, err
		}
		rewritten++
	}

	// Also re-encrypt any signing keys in the ID window.
	keyRows, err := p.pool.Query(ctx,
		`SELECT id, secret FROM channel_signing_keys WHERE id > $1 ORDER BY id ASC LIMIT $2`,
		afterID, batchSize,
	)
	if err != nil {
		return rewritten, lastID, fmt.Errorf("scanning signing keys for re-encryption: %w", err)
	}
	type keyRow struct {
		id     int64
		secret string
	}
	var keys []keyRow
	for keyRows.Next() {
		var kr keyRow
		if err := keyRows.Scan(&kr.id, &kr.secret); err != nil {
			keyRows.Close()
			return rewritten, lastID, err
		}
		keys = append(keys, kr)
	}
	keyRows.Close()
	if err := keyRows.Err(); err != nil {
		return rewritten, lastID, err
	}

	for _, kr := range keys {
		if kr.id > lastID {
			lastID = kr.id
		}
		pt, err := p.decryptSigningSecret(kr.secret)
		if err != nil {
			return rewritten, lastID, err
		}
		sealed, err := p.encryptSigningSecret(pt)
		if err != nil {
			return rewritten, lastID, err
		}
		if _, err := p.pool.Exec(ctx, `UPDATE channel_signing_keys SET secret = $1 WHERE id = $2`, sealed, kr.id); err != nil {
			return rewritten, lastID, err
		}
		rewritten++
	}

	return rewritten, lastID, nil
}

// ReencryptSensitiveColumns re-encrypts up to batchSize plaintext or
// retired-key rows across channels.config and channel_signing_keys.secret in
// the SQLite store.
func (s *SQLite) ReencryptSensitiveColumns(ctx context.Context, afterID int64, batchSize int) (rewritten int, lastID int64, err error) {
	if s.cipher == nil {
		return 0, afterID, errors.New("cannot re-encrypt sensitive columns: no config cipher is configured")
	}
	if batchSize <= 0 {
		batchSize = 100
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, config FROM channels WHERE id > ? ORDER BY id ASC LIMIT ?`,
		afterID, batchSize,
	)
	if err != nil {
		return 0, afterID, fmt.Errorf("scanning channels for re-encryption: %w", err)
	}
	type chanRow struct {
		id   int64
		name string
		raw  string
	}
	var pending []chanRow
	for rows.Next() {
		var r chanRow
		if err := rows.Scan(&r.id, &r.name, &r.raw); err != nil {
			_ = rows.Close()
			return 0, afterID, err
		}
		pending = append(pending, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, afterID, err
	}

	lastID = afterID
	for _, r := range pending {
		if r.id > lastID {
			lastID = r.id
		}
		plaintext, err := configForRead(s.cipher, r.id, r.name, []byte(r.raw))
		if err != nil {
			return rewritten, lastID, err
		}
		sealed, err := configForWrite(s.cipher, r.id, r.name, plaintext)
		if err != nil {
			return rewritten, lastID, err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE channels SET config = ? WHERE id = ?`, string(sealed), r.id); err != nil {
			return rewritten, lastID, err
		}
		rewritten++
	}

	keyRows, err := s.db.QueryContext(ctx,
		`SELECT id, secret FROM channel_signing_keys WHERE id > ? ORDER BY id ASC LIMIT ?`,
		afterID, batchSize,
	)
	if err != nil {
		return rewritten, lastID, fmt.Errorf("scanning signing keys for re-encryption: %w", err)
	}
	type keyRow struct {
		id     int64
		secret string
	}
	var keys []keyRow
	for keyRows.Next() {
		var kr keyRow
		if err := keyRows.Scan(&kr.id, &kr.secret); err != nil {
			_ = keyRows.Close()
			return rewritten, lastID, err
		}
		keys = append(keys, kr)
	}
	_ = keyRows.Close()
	if err := keyRows.Err(); err != nil {
		return rewritten, lastID, err
	}

	for _, kr := range keys {
		if kr.id > lastID {
			lastID = kr.id
		}
		pt, err := s.decryptSigningSecret(kr.secret)
		if err != nil {
			return rewritten, lastID, err
		}
		sealed, err := s.encryptSigningSecret(pt)
		if err != nil {
			return rewritten, lastID, err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE channel_signing_keys SET secret = ? WHERE id = ?`, sealed, kr.id); err != nil {
			return rewritten, lastID, err
		}
		rewritten++
	}

	return rewritten, lastID, nil
}

// VerifyEncryptionKey checks that the configured cipher can decrypt existing
// encrypted rows at startup, failing fast if the wrong key was supplied.
func (p *Postgres) VerifyEncryptionKey(ctx context.Context) error {
	if p.cipher == nil {
		return nil
	}
	rows, err := p.pool.Query(ctx, `SELECT id, name, config FROM channels LIMIT 10`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		var raw []byte
		if err := rows.Scan(&id, &name, &raw); err != nil {
			return err
		}
		if _, err := configForRead(p.cipher, id, name, raw); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	keyRows, err := p.pool.Query(ctx, `SELECT secret FROM channel_signing_keys LIMIT 10`)
	if err != nil {
		return err
	}
	defer keyRows.Close()
	for keyRows.Next() {
		var stored string
		if err := keyRows.Scan(&stored); err != nil {
			return err
		}
		if _, err := p.decryptSigningSecret(stored); err != nil {
			return err
		}
	}
	return keyRows.Err()
}

// VerifyEncryptionKey checks that the configured cipher can decrypt existing
// encrypted rows in the SQLite store at startup.
func (s *SQLite) VerifyEncryptionKey(ctx context.Context) error {
	if s.cipher == nil {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, config FROM channels LIMIT 10`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var name, raw string
		if err := rows.Scan(&id, &name, &raw); err != nil {
			return err
		}
		if _, err := configForRead(s.cipher, id, name, json.RawMessage(strings.TrimSpace(raw))); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	keyRows, err := s.db.QueryContext(ctx, `SELECT secret FROM channel_signing_keys LIMIT 10`)
	if err != nil {
		return err
	}
	defer func() { _ = keyRows.Close() }()
	for keyRows.Next() {
		var stored string
		if err := keyRows.Scan(&stored); err != nil {
			return err
		}
		if _, err := s.decryptSigningSecret(stored); err != nil {
			return err
		}
	}
	return keyRows.Err()
}
