package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
)

// control_meta is a small key/value table in control.db for node-local
// janitor/consumer state that is NOT node truth and therefore is never appended
// to the changelog. It mirrors the per-project meta table shape.
//
// Today it holds the per-domain "populated" sentinels; later it also holds the
// changelog acked watermark (v2.6.0).
const (
	// MetaFirewallPopulated / MetaVolumesPopulated latch true the first time the
	// controller submits that domain's desired-state DOWN. Until the latch is
	// set, a DOWN reader must NOT converge kernel/schedule state to "empty" from
	// an unpopulated control.db (an unpopulated table is indistinguishable from
	// "controller sent zero" and would otherwise close all ports / drop every
	// backup schedule). This is the guard behind the coordinated DOWN cutovers.
	MetaFirewallPopulated = "firewall_populated"
	MetaVolumesPopulated  = "volumes_populated"

	// MetaChangelogAcked is the controller's acknowledged changelog watermark:
	// the highest seq the single per-node consumer has durably projected. The
	// prune janitor deletes changelog rows at or below it (subject to an age
	// floor). Advanced only forward (monotonic) so a stale/rewound ack can't
	// resurrect the cursor.
	MetaChangelogAcked = "changelog_acked_seq"

	// MetaInstanceID is a random 128-bit hex id minted once per control.db (by
	// Open, if absent). It changes only when control.db is recreated, which lets
	// the controller tell a rebuilt node from a restarted one.
	MetaInstanceID = "instance_id"
)

// GetMeta returns the value for key. found=false on a miss (not an error).
func (s *Store) GetMeta(ctx context.Context, key string) (value string, found bool, err error) {
	row := s.control.QueryRowContext(ctx, `SELECT value FROM control_meta WHERE key = ?`, key)
	switch err := row.Scan(&value); {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("store: get control_meta %q: %w", key, err)
	default:
		return value, true, nil
	}
}

// SetMeta upserts key=value. This is consumer/janitor state, not node truth, so
// it is a plain autocommit write and never appends a changelog row.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	if _, err := s.control.ExecContext(ctx, `
		INSERT INTO control_meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value); err != nil {
		return fmt.Errorf("store: set control_meta %q: %w", key, err)
	}
	return nil
}

// IsPopulated reports whether the given domain's populated sentinel is latched.
// domain is one of MetaFirewallPopulated / MetaVolumesPopulated.
func (s *Store) IsPopulated(ctx context.Context, domain string) (bool, error) {
	v, found, err := s.GetMeta(ctx, domain)
	if err != nil {
		return false, err
	}
	return found && v == "1", nil
}

// setMetaTx upserts a control_meta row inside an existing transaction, so a
// populated sentinel latches atomically with the desired-state write that earns
// it. Not changelogged (see the package comment).
func setMetaTx(ctx context.Context, tx *sql.Tx, key, value string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO control_meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value); err != nil {
		return fmt.Errorf("store: set control_meta %q: %w", key, err)
	}
	return nil
}

// SetChangelogAcked advances the controller's acked changelog watermark. The
// update is monotonic: a seq at or below the stored value is silently ignored (no
// error), so a duplicate or reordered ack from the controller can never rewind the
// prune watermark and drop rows the controller still needs. Not changelogged
// (consumer state).
func (s *Store) SetChangelogAcked(ctx context.Context, seq int64) error {
	if seq < 0 {
		return errors.New("store: SetChangelogAcked requires seq >= 0")
	}
	_, err := s.control.ExecContext(ctx, `
		INSERT INTO control_meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
		WHERE CAST(excluded.value AS INTEGER) > CAST(control_meta.value AS INTEGER)
	`, MetaChangelogAcked, seq)
	if err != nil {
		return fmt.Errorf("store: set changelog acked: %w", err)
	}
	return nil
}

// GetChangelogAcked returns the acked watermark (0 if never set).
func (s *Store) GetChangelogAcked(ctx context.Context) (int64, error) {
	v, found, err := s.GetMeta(ctx, MetaChangelogAcked)
	if err != nil {
		return 0, err
	}
	if !found || v == "" {
		return 0, nil
	}
	seq, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("store: parse changelog acked %q: %w", v, err)
	}
	return seq, nil
}

// ensureInstanceID mints the control.db instance id if absent. An existing id is
// never replaced.
func ensureInstanceID(db *sql.DB) error {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("store: mint instance id: %w", err)
	}
	if _, err := db.Exec(`
		INSERT INTO control_meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO NOTHING
	`, MetaInstanceID, hex.EncodeToString(b[:])); err != nil {
		return fmt.Errorf("store: mint instance id: %w", err)
	}
	return nil
}

// InstanceID returns the control.db instance id ("" if never minted).
func (s *Store) InstanceID(ctx context.Context) (string, error) {
	v, _, err := s.GetMeta(ctx, MetaInstanceID)
	return v, err
}
