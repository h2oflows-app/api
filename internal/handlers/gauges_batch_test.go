package handlers

import (
	"reflect"
	"testing"
)

// A malformed id used to take the whole request down with it: every id is cast
// in one $1::uuid[], so one bad entry aborted the query and the caller's entire
// dashboard failed to hydrate. These are the shapes seen in the wild — an id
// that is empty because the client interpolated a missing value, the literal
// "undefined", and a `custom:<uuid>` gauge key from the gauges view.
func TestSplitBatchItemsDropsNonUUIDs(t *testing.T) {
	const (
		a = "86d31b3c-addd-4890-93f7-f393442669ec"
		b = "0f2c3f0b-1f4a-4c1e-9c5a-2b7d8e6f1a34"
	)

	tests := []struct {
		name      string
		items     []string
		wantIDs   []string
		wantSlugs []string
	}{
		{
			name:      "plain and contextual ids survive",
			items:     []string{a, b + ":south-platte-river-bailey"},
			wantIDs:   []string{a, b},
			wantSlugs: []string{"", "south-platte-river-bailey"},
		},
		{
			name:      "uppercase is still a uuid",
			items:     []string{"86D31B3C-ADDD-4890-93F7-F393442669EC"},
			wantIDs:   []string{"86D31B3C-ADDD-4890-93F7-F393442669EC"},
			wantSlugs: []string{""},
		},
		{
			name:      "empty id with a reach slug is dropped, not passed as ''",
			items:     []string{":south-platte-river-bailey", a},
			wantIDs:   []string{a},
			wantSlugs: []string{""},
		},
		{
			name:      "literal undefined is dropped",
			items:     []string{"undefined", "undefined:some-run", a},
			wantIDs:   []string{a},
			wantSlugs: []string{""},
		},
		{
			name:      "custom-gauge key is dropped",
			items:     []string{"custom:" + a, b},
			wantIDs:   []string{b},
			wantSlugs: []string{""},
		},
		{
			name:      "every id bad leaves empty slices, not a bad query",
			items:     []string{"nope", "also-nope"},
			wantIDs:   []string{},
			wantSlugs: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids, slugs := splitBatchItems(tt.items)
			if !reflect.DeepEqual(ids, tt.wantIDs) {
				t.Errorf("ids = %v, want %v", ids, tt.wantIDs)
			}
			if !reflect.DeepEqual(slugs, tt.wantSlugs) {
				t.Errorf("slugs = %v, want %v", slugs, tt.wantSlugs)
			}
			if len(ids) != len(slugs) {
				t.Errorf("parallel arrays diverged: %d ids vs %d slugs", len(ids), len(slugs))
			}
		})
	}
}

func TestSplitBatchItemsCapsAt200(t *testing.T) {
	items := make([]string, 250)
	for i := range items {
		items[i] = "86d31b3c-addd-4890-93f7-f393442669ec"
	}
	ids, slugs := splitBatchItems(items)
	if len(ids) != 200 || len(slugs) != 200 {
		t.Fatalf("cap not applied: %d ids, %d slugs", len(ids), len(slugs))
	}
}

func TestIsUUID(t *testing.T) {
	valid := []string{
		"86d31b3c-addd-4890-93f7-f393442669ec",
		"86D31B3C-ADDD-4890-93F7-F393442669EC",
	}
	invalid := []string{
		"", "undefined", "custom", "86d31b3c-addd-4890-93f7-f393442669e", // short
		"86d31b3c-addd-4890-93f7-f393442669ecc",                       // long
		"86d31b3c addd 4890 93f7 f393442669ec",                        // wrong separators
		"86d31b3cxaddd-4890-93f7-f393442669ec",                        // separator in wrong slot
		"g6d31b3c-addd-4890-93f7-f393442669ec",                        // non-hex
	}
	for _, s := range valid {
		if !isUUID(s) {
			t.Errorf("isUUID(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if isUUID(s) {
			t.Errorf("isUUID(%q) = true, want false", s)
		}
	}
}
