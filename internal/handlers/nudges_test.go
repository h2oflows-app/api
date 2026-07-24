package handlers

import (
	"testing"
	"time"
)

func d(s string) time.Time {
	t, err := parseDate(s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestCurrentStreakDays(t *testing.T) {
	today := d("2026-07-23")

	cases := []struct {
		name  string
		dates []time.Time // descending, as scanCurrentStreakDays' caller returns
		want  int
	}{
		{"empty -> 0", nil, 0},
		{
			"anchored on today, 3-day run",
			[]time.Time{d("2026-07-23"), d("2026-07-22"), d("2026-07-21")},
			3,
		},
		{
			"anchored on yesterday (hasn't paddled today yet)",
			[]time.Time{d("2026-07-22"), d("2026-07-21")},
			2,
		},
		{
			"most recent is 2+ days old -> broken streak, 0",
			[]time.Time{d("2026-07-20"), d("2026-07-19"), d("2026-07-18")},
			0,
		},
		{
			"gap stops the count at the gap, not the array end",
			[]time.Time{d("2026-07-23"), d("2026-07-22"), d("2026-07-20"), d("2026-07-19")},
			2, // 23,22 consecutive; 20 breaks it (gap on the 21st)
		},
		{
			"single day today",
			[]time.Time{d("2026-07-23")},
			1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := currentStreakDays(tc.dates, today)
			if got != tc.want {
				t.Errorf("currentStreakDays() = %d, want %d", got, tc.want)
			}
		})
	}
}
