package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

// APIKey authenticates a request via the X-API-Key header (form "hf_live_...").
// The SHA-256 hex of the key is matched against user_api_access.api_key_hash;
// on success the owning account is attached to the request context as the user.
// Rejects (401) when the header is missing or the key is unknown. Used by the
// #155 public run-upload endpoints.
func APIKey(db *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-API-Key")
			if key == "" {
				writeJSONError(w, http.StatusUnauthorized, "missing API key")
				return
			}
			sum := sha256.Sum256([]byte(key))
			hash := hex.EncodeToString(sum[:])
			var ownerID string
			if err := db.QueryRow(r.Context(),
				`SELECT owner_id FROM user_api_access WHERE api_key_hash = $1`, hash,
			).Scan(&ownerID); err != nil {
				writeJSONError(w, http.StatusUnauthorized, "invalid API key")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), ownerID, "", "")))
		})
	}
}
