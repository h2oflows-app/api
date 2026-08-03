// Package handlers — run_elevation.go computes per-run elevation + gradient
// (mig 000150: user_reaches.put_in_elevation_ft / take_out_elevation_ft /
// gradient_fpm), replacing the put_in_lng "west=upstream" sort heuristic
// (web#386) with a direction-agnostic basis. Per-run rather than reusing
// gauges.elevation_ft: multiple runs commonly share one gauge (a shared
// elevation would tie them, giving no ordering), and a gauge can sit miles
// from the actual put-in.
//
// Shared by every path that writes put_in/take_out or a centerline: Create,
// Import, Update (user_reaches.go), createOneUploadRun, UploadUpdate
// (run_upload.go). forkRunTx is the one exception — it copies the source
// run's already-resolved values directly (same coordinates, nothing moved),
// no EPQS call needed.
package handlers

import (
	"context"
	"time"

	"github.com/h2oflow/h2oflow/apps/api/internal/elevation"
	"github.com/jackc/pgx/v5/pgxpool"
)

// metersPerMile matches the conversion constant already used for the
// geometry-clustering ST_DWithin radius elsewhere in this package (1 mile,
// e.g. MapCommunity's geo_clusters CTE and cluster.go's nearbyRuns).
const metersPerMile = 1609.34

// elevationLookupTimeout bounds each USGS EPQS call — tighter than
// internal/elevation's own 15s http.Client timeout, so a slow point-query
// response can't hold a create/update request open as long as that client
// would otherwise allow. Put-in and take-out are queried concurrently
// (queryEndpointElevations), so this is the worst-case added time for BOTH,
// not each.
const elevationLookupTimeout = 8 * time.Second

// startElevationLookup kicks off put-in/take-out elevation resolution in the
// background and returns a function that blocks until both are ready.
//
// Call it as early as the handler safely can — right after put_in/take_out
// are known — and call the returned function only once the values are
// actually needed (immediately before the INSERT/UPDATE that writes them).
// Every call site in this package already does other network-bound work
// between "endpoints known" and "row written" (state/river resolution via
// nldi.StateAt/BasinAt, or buildUploadCenterline's own up-to-30s NLDI fetch)
// — starting the EPQS calls first lets them run ALONGSIDE that work instead
// of stacking on top of it, so the up-to-elevationLookupTimeout cost is
// usually fully hidden. The result channel is buffered (size 1), so a
// handler that returns early on an error path without ever calling the
// returned function does not leak the goroutine — the send always succeeds.
func startElevationLookup(ctx context.Context, putInLng, putInLat, takeOutLng, takeOutLat float64) func() (putInFt, takeOutFt *float64) {
	type result struct{ putFt, takeFt *float64 }
	ch := make(chan result, 1)
	go func() {
		putFt, takeFt := queryEndpointElevations(ctx, putInLng, putInLat, takeOutLng, takeOutLat)
		ch <- result{putFt, takeFt}
	}()
	return func() (putInFt, takeOutFt *float64) {
		res := <-ch
		return res.putFt, res.takeFt
	}
}

// queryEndpointElevations resolves put-in and take-out elevation (feet)
// concurrently via USGS EPQS (internal/elevation). Fails soft PER POINT: a
// timeout, network error, or unparseable response on either point yields a
// nil result for THAT point only. This never returns an error — an EPQS
// hiccup must never fail or block the surrounding create/update request.
func queryEndpointElevations(ctx context.Context, putInLng, putInLat, takeOutLng, takeOutLat float64) (putInFt, takeOutFt *float64) {
	type point struct {
		lng, lat float64
		ch       chan *float64
	}
	points := []point{
		{putInLng, putInLat, make(chan *float64, 1)},
		{takeOutLng, takeOutLat, make(chan *float64, 1)},
	}
	for _, p := range points {
		go func(p point) {
			cctx, cancel := context.WithTimeout(ctx, elevationLookupTimeout)
			defer cancel()
			v, err := elevation.QueryElevation(cctx, p.lng, p.lat)
			if err != nil {
				p.ch <- nil
				return
			}
			p.ch <- &v
		}(p)
	}
	return <-points[0].ch, <-points[1].ch
}

// centerlineMiles returns the length of a GeoJSON LineString in miles, via
// PostGIS ST_Length on a geography cast (metres natively), converted with
// metersPerMile. Evaluated directly against the supplied GeoJSON string —
// not a re-read of the stored row — so callers can use it on a centerline
// that's about to be written but isn't committed yet.
//
// ok=false for an empty string, a non-positive length (degenerate/invalid
// geometry — this is also the gradient divide-by-zero guard, since
// gradientFPM refuses to compute unless milesOK), or a query error. Never an
// error return: this only ever feeds a best-effort gradient calculation.
func centerlineMiles(ctx context.Context, db *pgxpool.Pool, geojson string) (miles float64, ok bool) {
	if geojson == "" {
		return 0, false
	}
	err := db.QueryRow(ctx,
		`SELECT ST_Length(ST_GeomFromGeoJSON($1)::geography) / $2`,
		geojson, metersPerMile,
	).Scan(&miles)
	if err != nil || miles <= 0 {
		return 0, false
	}
	return miles, true
}

// gradientFPM computes average drop in feet per mile from put-in/take-out
// elevation and centerline length: (putInFt - takeOutFt) / miles.
//
// ok=false (callers must leave gradient_fpm NULL) unless both elevations are
// present AND milesOK — river_miles<=0 is already excluded by
// centerlineMiles's ok=false, so the division below can never divide by
// zero. A negative result (take-out higher than put-in — bad data, or a
// reversed centerline) is returned as-is, not clamped to zero: a visibly
// wrong number a human can spot and fix beats one silently hidden.
func gradientFPM(putInFt, takeOutFt *float64, miles float64, milesOK bool) (fpm float64, ok bool) {
	if putInFt == nil || takeOutFt == nil || !milesOK {
		return 0, false
	}
	return (*putInFt - *takeOutFt) / miles, true
}
