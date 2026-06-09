package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DiscoverHandler serves unified ranked search across curated + community runs.
type DiscoverHandler struct {
	db *pgxpool.Pool
}

func NewDiscoverHandler(db *pgxpool.Pool) *DiscoverHandler {
	return &DiscoverHandler{db: db}
}

type discoverRun struct {
	ID                  string    `json:"id"`
	Slug                string    `json:"slug"`
	Name                string    `json:"name"`
	Handle              string    `json:"handle"`
	IsOfficial          bool      `json:"is_official"`
	ClassMin            *float64  `json:"class_min"`
	ClassMax            *float64  `json:"class_max"`
	LengthMi            *float64  `json:"length_mi"`
	UpvoteCount         int64     `json:"upvote_count"`
	LastForkedAt        *time.Time `json:"last_forked_at"`
	GaugeName           *string   `json:"gauge_name"`
	PutInLng            *float64  `json:"put_in_lng"`
	PutInLat            *float64  `json:"put_in_lat"`
	OriginalAuthorHandle *string  `json:"original_author_handle"`
}

// ListRuns handles GET /api/v1/discover/runs.
// Params: q, min_class, max_class, has_gauge, handle, limit, offset.
// Returns curated + community runs interleaved, ranked per V15.
func (h *DiscoverHandler) ListRuns(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))

	var minClass *float64
	if v := r.URL.Query().Get("min_class"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			minClass = &f
		}
	}
	var maxClass *float64
	if v := r.URL.Query().Get("max_class"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			maxClass = &f
		}
	}

	hasGauge := r.URL.Query().Get("has_gauge") == "true"
	handle := strings.TrimSpace(r.URL.Query().Get("handle"))

	limit := 20
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 50 {
		limit = l
	}
	offset := 0
	if o, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && o >= 0 {
		offset = o
	}

	rows, err := h.db.Query(r.Context(), `
		SELECT
			id, slug, name, handle, is_official,
			class_min, class_max, length_mi,
			upvote_count, last_forked_at, gauge_name,
			put_in_lng, put_in_lat,
			original_author_handle,
			text_rank
		FROM (
			-- curated H2OFlows reaches
			SELECT
				r.id::text                          AS id,
				r.slug                              AS slug,
				r.name                              AS name,
				'h2oflows'                          AS handle,
				TRUE                                AS is_official,
				r.class_min                         AS class_min,
				r.class_max                         AS class_max,
				r.length_mi                         AS length_mi,
				0::bigint                           AS upvote_count,
				NULL::timestamptz                   AS last_forked_at,
				g.name                              AS gauge_name,
				ST_X(r.start_point::geometry)       AS put_in_lng,
				ST_Y(r.start_point::geometry)       AS put_in_lat,
				NULL::text                          AS original_author_handle,
				CASE
					WHEN LOWER(r.name) = LOWER($1)              THEN 2
					WHEN LOWER(r.name) LIKE LOWER($1) || '%'    THEN 1
					ELSE 0
				END                                 AS text_rank
			FROM reaches r
			LEFT JOIN gauges g ON g.id = r.primary_gauge_id
			WHERE ($1 = '' OR r.name ILIKE '%' || $1 || '%' OR r.river_name ILIKE '%' || $1 || '%')
			  AND ($2::float8 IS NULL OR r.class_max >= $2)
			  AND ($3::float8 IS NULL OR r.class_min <= $3)
			  AND (NOT $4::bool OR g.id IS NOT NULL)
			  AND ($5 = '')

			UNION ALL

			-- community user reaches
			SELECT
				ur.id::text                         AS id,
				ur.slug                             AS slug,
				ur.name                             AS name,
				COALESCE(up.handle, 'h2oflows')     AS handle,
				(up.owner_id IS NULL)               AS is_official,
				ur.class_min                        AS class_min,
				ur.class_max                        AS class_max,
				CASE
					WHEN ur.centerline IS NOT NULL
						THEN ROUND((ST_Length(ur.centerline) / 1609.34)::numeric, 2)::float8
					ELSE ROUND((ST_Distance(ur.put_in::geography, ur.take_out::geography) / 1609.34)::numeric, 2)::float8
				END                                 AS length_mi,
				COALESCE(
					(SELECT COUNT(*) FROM run_upvotes uv WHERE uv.user_reach_id = ur.id), 0
				)::bigint                           AS upvote_count,
				ur.original_forked_at               AS last_forked_at,
				COALESCE(g.name, cg.name)           AS gauge_name,
				ST_X(ur.put_in::geometry)           AS put_in_lng,
				ST_Y(ur.put_in::geometry)           AS put_in_lat,
				ur.original_author_handle           AS original_author_handle,
				CASE
					WHEN LOWER(ur.name) = LOWER($1)             THEN 2
					WHEN LOWER(ur.name) LIKE LOWER($1) || '%'   THEN 1
					ELSE 0
				END                                 AS text_rank
			FROM user_reaches ur
			LEFT JOIN gauges g ON g.id = ur.primary_gauge_id
			LEFT JOIN custom_gauges cg ON cg.id = ur.custom_gauge_id
			LEFT JOIN user_profiles up ON up.owner_id = ur.owner_id
			WHERE ur.visibility = 'public' AND ur.deleted_at IS NULL
			  AND ur.completeness_score >= 0.2
			  AND ($1 = '' OR ur.name ILIKE '%' || $1 || '%' OR ur.river_name ILIKE '%' || $1 || '%')
			  AND ($2::float8 IS NULL OR ur.class_max >= $2)
			  AND ($3::float8 IS NULL OR ur.class_min <= $3)
			  AND (NOT $4::bool OR (g.id IS NOT NULL OR cg.id IS NOT NULL))
			  AND ($5 = '' OR up.handle = $5)
		) combined
		ORDER BY upvote_count DESC, is_official DESC, text_rank DESC, last_forked_at DESC NULLS LAST
		LIMIT $6 OFFSET $7
	`, q, minClass, maxClass, hasGauge, handle, limit+1, offset)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	items := make([]discoverRun, 0)
	for rows.Next() {
		var run discoverRun
		var textRank int
		if err := rows.Scan(
			&run.ID, &run.Slug, &run.Name, &run.Handle, &run.IsOfficial,
			&run.ClassMin, &run.ClassMax, &run.LengthMi,
			&run.UpvoteCount, &run.LastForkedAt, &run.GaugeName,
			&run.PutInLng, &run.PutInLat,
			&run.OriginalAuthorHandle,
			&textRank,
		); err != nil {
			continue
		}
		_ = textRank
		items = append(items, run)
	}

	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"items":       items,
		"has_more":    hasMore,
		"next_offset": offset + len(items),
	})
}
