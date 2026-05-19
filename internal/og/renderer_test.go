package og

import (
	"bytes"
	"image"
	_ "image/png"
	"os"
	"testing"
)

func TestRenderReachSmoke(t *testing.T) {
	d := ReachData{
		Name:       "Browns Canyon (Hecla Junction to Stone Bridge)",
		RiverName:  "Arkansas River",
		Region:     "CO",
		ClassLabel: "III+",
		LengthMi:   11.2,
		CurrentCfs: 850,
		HasCfs:     true,
		FlowStatus: "running",
		Slug:       "arkansas-river-browns-canyon",
	}
	png, err := RenderReach(d)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(png) < 1000 {
		t.Fatalf("unexpectedly small PNG: %d bytes", len(png))
	}
	img, _, err := image.Decode(bytes.NewReader(png))
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	if img.Bounds().Dx() != Width || img.Bounds().Dy() != Height {
		t.Fatalf("got %dx%d want %dx%d", img.Bounds().Dx(), img.Bounds().Dy(), Width, Height)
	}
	if os.Getenv("OG_DUMP") != "" {
		_ = os.WriteFile("/tmp/og-reach.png", png, 0644)
	}
}

func TestRenderReportSmoke(t *testing.T) {
	d := ReportData{
		Title:      "Solid spring run — clean lines",
		ReachName:  "Browns Canyon",
		Handle:     "river_dan",
		ReportDate: "2026-05-15",
		FlowCfs:    1200,
		HasCfs:     true,
		FlowBand:   "running",
		Paddled:    true,
		ID:         "abc-123",
	}
	png, err := RenderReport(d)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(png) < 1000 {
		t.Fatalf("small PNG: %d", len(png))
	}
	if os.Getenv("OG_DUMP") != "" {
		_ = os.WriteFile("/tmp/og-report.png", png, 0644)
	}
}

func TestRenderGaugeSmoke(t *testing.T) {
	d := GaugeData{
		Name:       "Arkansas River at Salida",
		Source:     "USGS",
		CurrentCfs: 850,
		HasCfs:     true,
		FlowStatus: "running",
		ID:         "07091200",
	}
	png, err := RenderGauge(d)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(png) < 1000 {
		t.Fatalf("small PNG: %d", len(png))
	}
	if os.Getenv("OG_DUMP") != "" {
		_ = os.WriteFile("/tmp/og-gauge.png", png, 0644)
	}
}
