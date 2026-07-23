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
	if got := ClampReportBand(nil); got != nil {
		t.Errorf("ClampReportBand(nil) = %v, want nil", got)
	}
	// case-insensitive fold onto the reports CHECK set
	for in, want := range map[string]string{"low": "low", "Running": "running", "High": "high"} {
		in := in
		if got := ClampReportBand(&in); got == nil || *got != want {
			t.Errorf("ClampReportBand(%q) = %v, want %q", in, got, want)
		}
	}
	// labels outside the CHECK set clamp to nil
	for _, in := range []string{"Too Low", "Very High", "Custom Label"} {
		in := in
		if got := ClampReportBand(&in); got != nil {
			t.Errorf("ClampReportBand(%q) = %v, want nil", in, got)
		}
	}
}
