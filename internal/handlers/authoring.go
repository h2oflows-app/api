// Package handlers — authoring.go holds the shared special-user authoring
// model introduced in #314: which owners a caller may create/edit content
// under, API key issuance for special/public-API accounts, and a thin
// per-hour rate limit on run writes.
package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// editableOwnerIDs returns the set of owner_ids the caller may author or edit
// content under: their own account, plus the owner_id of any special-user
// account whose handle matches one of the caller's app roles (loaded from
// user_roles by the LoadAppRoles middleware). There is NO data_admin/
// site_admin fallback here — special-account editing requires an exact
// role==handle grant. This intentionally drops the old sentinel-only
// isDataAdmin shortcut (#314).
//
// Best-effort: a query failure just returns [callerID] (caller can still edit
// their own content), consistent with this codebase's "_ = " error-swallowing
// idiom on non-critical lookups.
func editableOwnerIDs(ctx context.Context, db *pgxpool.Pool, callerID string) []string {
	ids := []string{callerID}
	roles := auth.AppRolesFromContext(ctx)
	if len(roles) == 0 {
		return ids
	}
	rows, err := db.Query(ctx,
		`SELECT owner_id FROM user_profiles WHERE is_special AND handle = ANY($1::text[])`, roles)
	if err != nil {
		return ids
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// isValidRoleName reports whether role is grantable via AssignRole / the
// admin roles-members endpoints: a legacy system role, or the handle of an
// existing special user.
func isValidRoleName(ctx context.Context, db *pgxpool.Pool, role string) bool {
	if role == "site_admin" {
		return true
	}
	var exists bool
	_ = db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM user_profiles WHERE is_special AND handle = $1)`, role,
	).Scan(&exists)
	return exists
}

// generateAPIKey mints a new key in the form "hf_live_" + 32 hex chars. It
// returns the full key (shown to the caller exactly once), its SHA-256 hex
// digest (stored in api_key_hash), and its last 4 characters (stored in
// api_key_last4 for display).
func generateAPIKey() (full, hash, last4 string, err error) {
	buf := make([]byte, 16) // 16 bytes -> 32 hex chars
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", err
	}
	suffix := hex.EncodeToString(buf)
	full = "hf_live_" + suffix
	sum := sha256.Sum256([]byte(full))
	hash = hex.EncodeToString(sum[:])
	last4 = suffix[len(suffix)-4:]
	return full, hash, last4, nil
}

// standardRateLimitDefaults are applied to a first-time user_api_access row
// for a regular (non-special) account.
const (
	defaultRunsPerHour       = 100
	defaultMaxBatch          = 10
	defaultRequestsPerMinute = 30
	defaultConcurrentJobs    = 1
)

// specialRateLimitDefaults are applied to a first-time user_api_access row
// for a special (is_special) account.
const (
	specialRunsPerHour       = 500
	specialMaxBatch          = 50
	specialRequestsPerMinute = 60
	specialConcurrentJobs    = 1
)

// RateLimitError is returned by recordRunWrite when the author owner has hit
// their runs_per_hour quota. Handlers should respond 429.
type RateLimitError struct {
	RunsPerHour int
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limit exceeded: %d runs/hour", e.RunsPerHour)
}

// recordRunWrite lazily upserts a user_api_access row for ownerID (standard
// defaults 100/10/30/1; special users get 500/50/60/1), rolls the hourly
// usage window forward when it has elapsed, and increments usage_hour_count.
// Returns a *RateLimitError when the increment would exceed runs_per_hour.
//
// Call at the top of run-write handlers (Create, Update, SetRapids,
// SetAccessPoints, SetFlowRanges), passing the resolved AUTHOR owner_id — not
// necessarily the caller — so bulk-as-special-account writes count against
// the special account's quota, not the individual editor's.
//
// Best-effort on infra failure: a DB error never blocks a write (returns nil)
// — the rate limit is a thin courtesy gate, not a security boundary.
func recordRunWrite(ctx context.Context, db *pgxpool.Pool, ownerID string) error {
	var isSpecial bool
	_ = db.QueryRow(ctx, `SELECT is_special FROM user_profiles WHERE owner_id = $1`, ownerID).Scan(&isSpecial)

	runsPerHour, maxBatch, reqPerMin, concurrent := defaultRunsPerHour, defaultMaxBatch, defaultRequestsPerMinute, defaultConcurrentJobs
	if isSpecial {
		runsPerHour, maxBatch, reqPerMin, concurrent = specialRunsPerHour, specialMaxBatch, specialRequestsPerMinute, specialConcurrentJobs
	}

	_, err := db.Exec(ctx, `
		INSERT INTO user_api_access
			(owner_id, runs_per_hour, max_batch, requests_per_minute, concurrent_jobs, usage_hour_start, usage_hour_count)
		VALUES ($1, $2, $3, $4, $5, NOW(), 0)
		ON CONFLICT (owner_id) DO NOTHING
	`, ownerID, runsPerHour, maxBatch, reqPerMin, concurrent)
	if err != nil {
		return nil
	}

	var limit, usageCount int
	var usageStart *time.Time
	if err := db.QueryRow(ctx, `
		SELECT runs_per_hour, usage_hour_start, usage_hour_count
		FROM user_api_access WHERE owner_id = $1
	`, ownerID).Scan(&limit, &usageStart, &usageCount); err != nil {
		return nil
	}

	if usageStart == nil || time.Since(*usageStart) >= time.Hour {
		_, _ = db.Exec(ctx, `
			UPDATE user_api_access SET usage_hour_start = NOW(), usage_hour_count = 1 WHERE owner_id = $1
		`, ownerID)
		return nil
	}

	if usageCount >= limit {
		return &RateLimitError{RunsPerHour: limit}
	}

	_, _ = db.Exec(ctx, `UPDATE user_api_access SET usage_hour_count = usage_hour_count + 1 WHERE owner_id = $1`, ownerID)
	return nil
}
