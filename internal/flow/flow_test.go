package flow

import "testing"

func TestBandLabelForCFS(t *testing.T) {
	thresholds := []Threshold{
		{Value: 300, Label: "Running"},
		{Value: 800, Label: "High"},
		{Value: 2000, Label: "Very High"},
	}

	cases := []struct {
		name string
		cfs  float64
		want string
	}{
		{"below lowest threshold falls back to base label", 100, "Too Low"},
		{"exact match on lowest threshold", 300, "Running"},
		{"between thresholds keeps lower one", 799, "Running"},
		{"exact match on middle threshold", 800, "High"},
		{"exact match on highest threshold", 2000, "Very High"},
		{"above highest threshold keeps highest", 5000, "Very High"},
		{"zero with zero-value threshold not lower than base", 0, "Too Low"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BandLabelForCFS(tc.cfs, "Too Low", thresholds)
			if got == nil {
				t.Fatalf("got nil, want %q", tc.want)
			}
			if *got != tc.want {
				t.Errorf("BandLabelForCFS(%v) = %q, want %q", tc.cfs, *got, tc.want)
			}
		})
	}
}

func TestBandLabelForCFS_NoThresholds(t *testing.T) {
	got := BandLabelForCFS(500, "Base", nil)
	if got == nil || *got != "Base" {
		t.Fatalf("got %v, want Base", got)
	}
}

func TestClampReportBand(t *testing.T) {
	low := "low"
	high := "High" // not in AllowedReportBands (wrong case, and not a threshold-family word)

	if got := ClampReportBand(nil); got != nil {
		t.Errorf("ClampReportBand(nil) = %v, want nil", got)
	}
	if got := ClampReportBand(&low); got == nil || *got != "low" {
		t.Errorf("ClampReportBand(%q) = %v, want unchanged", low, got)
	}
	if got := ClampReportBand(&high); got != nil {
		t.Errorf("ClampReportBand(%q) = %v, want nil", high, got)
	}
}
