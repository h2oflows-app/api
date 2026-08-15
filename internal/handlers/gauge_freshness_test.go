package handlers

import (
	"testing"
	"time"
)

func ptrInt32(v int32) *int32 { return &v }

func TestReadingFreshness(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { ts := now.Add(-d); return &ts }

	mins := func(m int) *int32 { return ptrInt32(int32(m) * 60) }

	cases := []struct {
		name      string
		last      *time.Time
		interval  *int32
		wantStale bool
		wantAge   *int64
	}{
		// A gauge that has never reported has nothing to be stale.
		{"no reading at all", nil, mins(15), false, nil},

		// 15-minute cadence: 3x is 45m, which is exactly the floor.
		{"fast gauge, fresh", ago(5 * time.Minute), mins(15), false, i64(300)},
		{"fast gauge, at the floor", ago(45 * time.Minute), mins(15), false, i64(2700)},
		{"fast gauge, past the floor", ago(46 * time.Minute), mins(15), true, i64(2760)},

		// Slow gauge: the floor must not fire early on it. 84m cadence -> 252m.
		{"slow gauge, 3h old is fine", ago(3 * time.Hour), mins(84), false, i64(10800)},
		{"slow gauge, past 3x cadence", ago(5 * time.Hour), mins(84), true, i64(18000)},

		// The floor protects a very fast gauge from a brief gap.
		{"1-minute cadence, 20m gap", ago(20 * time.Minute), mins(1), false, i64(1200)},
		{"1-minute cadence, 2h gap", ago(2 * time.Hour), mins(1), true, i64(7200)},

		// Unknown cadence falls back to the 15m default, so still the floor.
		{"no interval, fresh", ago(30 * time.Minute), nil, false, i64(1800)},
		{"no interval, stale", ago(90 * time.Minute), nil, true, i64(5400)},
		{"zero interval treated as unknown", ago(90 * time.Minute), ptrInt32(0), true, i64(5400)},

		// The case this whole thing exists for: staging's gauges, 8+ days old,
		// every poll "succeeding".
		{"the staging case", ago(8*24*time.Hour + 20*time.Hour), mins(15), true, i64(763200)},

		// A source stamping slightly ahead must not read as negative.
		{"future timestamp", ago(-90 * time.Second), mins(15), false, i64(0)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			age, stale := readingFreshness(tc.last, tc.interval, now)

			if stale != tc.wantStale {
				t.Errorf("stale = %v, want %v", stale, tc.wantStale)
			}
			switch {
			case tc.wantAge == nil && age != nil:
				t.Errorf("age = %d, want nil", *age)
			case tc.wantAge != nil && age == nil:
				t.Errorf("age = nil, want %d", *tc.wantAge)
			case tc.wantAge != nil && *age != *tc.wantAge:
				t.Errorf("age = %d, want %d", *age, *tc.wantAge)
			}
		})
	}
}

// The point of a derived value: it keeps getting worse on its own, which is
// exactly what a stored flag cannot do once nothing is updating the row.
func TestReadingFreshnessGrowsWithoutAnyWrite(t *testing.T) {
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	last := base.Add(-30 * time.Minute)

	if _, stale := readingFreshness(&last, ptrInt32(900), base); stale {
		t.Fatal("30m on a 15m cadence should not be stale yet")
	}
	// Same row, same columns, nothing written — only the clock moved.
	if _, stale := readingFreshness(&last, ptrInt32(900), base.Add(30*time.Minute)); !stale {
		t.Fatal("60m on a 15m cadence must be stale with no write having occurred")
	}
}

func i64(v int64) *int64 { return &v }
