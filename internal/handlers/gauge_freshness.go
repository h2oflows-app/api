package handlers

import "time"

// Reading-age staleness (api#208).
//
// poll_health answers "can we reach the source". It is derived entirely from
// consecutive_poll_failures, so a source that keeps returning HTTP 200 with a
// frozen value has zero failures and reads healthy forever. That is a different
// fact about a different subject: the source is up, it just isn't producing.
//
// DERIVED, NOT STORED, on purpose. A stored health flag goes stale on its own —
// staging's poll_health has been asserting "healthy" about gauges that stopped
// reporting on 2026-08-04, because nothing is running there to update it, and
// the same is true in prod for any gauge dropped from polled_gauge_ids or
// missed during a poller outage. A value recomputed from timestamps at request
// time cannot lie that way: if nothing has updated the gauge, the age simply
// grows.
//
// Only meaningful because api#207 made last_reading_at the READING's timestamp
// rather than the poll time. Before that this computation had no real input.

const (
	// Missed intervals before a gauge counts as stale. Three is late enough to
	// ride out one skipped publish plus a retry, early enough to matter.
	stalenessIntervalMultiple = 3

	// Floor, so a fast-publishing gauge isn't called stale over a brief gap —
	// a 15-minute cadence would otherwise trip at 45 minutes exactly.
	stalenessFloor = 45 * time.Minute

	// Cadence assumed when a gauge has no detected interval yet. Every gauge
	// with readings currently has one (updateDetectedInterval needs 6 readings),
	// so this covers the first few polls of a brand-new gauge.
	stalenessDefaultInterval = 15 * time.Minute
)

// readingFreshness reports how old a gauge's newest reading is, and whether
// that age is stale RELATIVE TO THAT GAUGE'S OWN CADENCE.
//
// Cadence-relative rather than a fixed threshold because observed intervals
// span 15-84 minutes: one fixed number would cry stale on a slow gauge and stay
// quiet on a fast one that had died.
//
// A gauge with no reading at all reports a nil age and stale=false. There is
// nothing to be stale — it is a gauge that has never produced, which the UI
// already shows by having no value to display. Callers wanting to tell the two
// apart have the nil age.
func readingFreshness(lastReadingAt *time.Time, detectedIntervalSeconds *int32, now time.Time) (*int64, bool) {
	if lastReadingAt == nil {
		return nil, false
	}

	age := now.Sub(*lastReadingAt)
	if age < 0 {
		// Source stamped slightly ahead, or clock skew. Not stale.
		age = 0
	}
	ageSeconds := int64(age / time.Second)

	interval := stalenessDefaultInterval
	if detectedIntervalSeconds != nil && *detectedIntervalSeconds > 0 {
		interval = time.Duration(*detectedIntervalSeconds) * time.Second
	}

	threshold := time.Duration(stalenessIntervalMultiple) * interval
	if threshold < stalenessFloor {
		threshold = stalenessFloor
	}

	return &ageSeconds, age > threshold
}
