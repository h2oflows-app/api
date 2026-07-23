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

func TestBandAndColorForCFS(t *testing.T) {
	thresholds := []Threshold{
		{Value: 300, Label: "Running", Color: "p3"},
		{Value: 800, Label: "High", Color: "p4"},
	}

	if band, color := BandAndColorForCFS(100, "Too Low", "p0", thresholds); band != "Too Low" || color != "p0" {
		t.Errorf("below lowest threshold: got (%q,%q), want (Too Low,p0)", band, color)
	}
	if band, color := BandAndColorForCFS(300, "Too Low", "p0", thresholds); band != "Running" || color != "p3" {
		t.Errorf("exact match lowest: got (%q,%q), want (Running,p3)", band, color)
	}
	if band, color := BandAndColorForCFS(5000, "Too Low", "p0", thresholds); band != "High" || color != "p4" {
		t.Errorf("above highest: got (%q,%q), want (High,p4)", band, color)
	}
	if band, color := BandAndColorForCFS(500, "Base", "p0", nil); band != "Base" || color != "p0" {
		t.Errorf("no thresholds: got (%q,%q), want (Base,p0)", band, color)
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
