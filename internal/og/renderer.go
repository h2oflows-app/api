// Package og renders OpenGraph share-card PNGs for reach, report, and gauge pages.
//
// Cards are 1200x630 (the standard OG aspect). Text composition only in v0 —
// no map thumbnails. Fonts are embedded so the binary is portable.
package og

import (
	"bytes"
	"embed"
	"fmt"
	"image/color"

	"github.com/fogleman/gg"
	"github.com/golang/freetype/truetype"
	"golang.org/x/image/font"
)

//go:embed fonts/inter-400-latin.ttf fonts/inter-700-latin.ttf
var fontFS embed.FS

const (
	Width  = 1200
	Height = 630
)

var (
	regularFont *truetype.Font
	boldFont    *truetype.Font
)

func init() {
	regBytes, err := fontFS.ReadFile("fonts/inter-400-latin.ttf")
	if err != nil {
		panic(fmt.Sprintf("og: read regular font: %v", err))
	}
	bldBytes, err := fontFS.ReadFile("fonts/inter-700-latin.ttf")
	if err != nil {
		panic(fmt.Sprintf("og: read bold font: %v", err))
	}
	regularFont, err = truetype.Parse(regBytes)
	if err != nil {
		panic(fmt.Sprintf("og: parse regular font: %v", err))
	}
	boldFont, err = truetype.Parse(bldBytes)
	if err != nil {
		panic(fmt.Sprintf("og: parse bold font: %v", err))
	}
}

func face(f *truetype.Font, size float64) font.Face {
	return truetype.NewFace(f, &truetype.Options{Size: size, DPI: 72, Hinting: font.HintingNone})
}

// Color palette — matches dashboard dark theme.
var (
	bgColor       = color.RGBA{0x0c, 0x16, 0x22, 0xff} // navy
	fgColor       = color.RGBA{0xff, 0xff, 0xff, 0xff}
	mutedColor    = color.RGBA{0x94, 0xa3, 0xb8, 0xff} // slate-400
	accentColor   = color.RGBA{0x3b, 0x82, 0xf6, 0xff} // blue-500 (brand primary)
	lowColor      = color.RGBA{0xef, 0x44, 0x44, 0xff} // red-500
	runningColor  = color.RGBA{0x22, 0xc5, 0x5e, 0xff} // green-500
	highColor     = color.RGBA{0x3b, 0x82, 0xf6, 0xff} // blue-500
	defaultBadge  = color.RGBA{0x6b, 0x72, 0x80, 0xff} // gray
)

func bandColor(status string) color.RGBA {
	switch status {
	case "low":
		return lowColor
	case "running":
		return runningColor
	case "high":
		return highColor
	default:
		return defaultBadge
	}
}

// ReachData is the input to RenderReach.
type ReachData struct {
	Name        string
	RiverName   string
	Region      string
	ClassLabel  string  // "III+", "IV", etc.
	LengthMi    float64 // 0 if unknown
	CurrentCfs  float64
	HasCfs      bool
	FlowStatus  string // "low" / "running" / "high" / "" (unknown)
	Slug        string
}

// RenderReach produces a PNG share card for a reach.
func RenderReach(d ReachData) ([]byte, error) {
	dc := gg.NewContext(Width, Height)
	dc.SetColor(bgColor)
	dc.Clear()

	// Top bar — H2OFlows wordmark
	dc.SetColor(accentColor)
	dc.DrawCircle(80, 70, 18)
	dc.Fill()
	dc.SetColor(fgColor)
	dc.SetFontFace(face(boldFont, 28))
	dc.DrawString("H2OFlows", 110, 80)

	// Class chip top-right — width sized to label
	if d.ClassLabel != "" {
		label := "Class " + d.ClassLabel
		dc.SetFontFace(face(boldFont, 28))
		labelW, _ := dc.MeasureString(label)
		chipW := labelW + 50
		chipX := float64(Width-50) - chipW
		dc.SetColor(accentColor)
		dc.DrawRoundedRectangle(chipX, 50, chipW, 52, 12)
		dc.Fill()
		dc.SetColor(fgColor)
		dc.SetFontFace(face(boldFont, 28))
		dc.DrawStringAnchored(label, chipX+chipW/2, 78, 0.5, 0.5)
	}

	// River name (small uppercase eyebrow) — sits above the title with clear gap
	if d.RiverName != "" {
		dc.SetColor(mutedColor)
		dc.SetFontFace(face(boldFont, 22))
		dc.DrawString(strUpper(d.RiverName), 80, 180)
	}

	// Reach name — large, possibly wrapped. y is the baseline of line 1.
	dc.SetColor(fgColor)
	dc.SetFontFace(face(boldFont, 64))
	wrapAndDraw(dc, d.Name, 80, 260, float64(Width-160), 76)

	// Region + length line
	parts := []string{}
	if d.Region != "" {
		parts = append(parts, d.Region)
	}
	if d.LengthMi > 0 {
		parts = append(parts, fmt.Sprintf("%.1f mi", d.LengthMi))
	}
	if len(parts) > 0 {
		dc.SetColor(mutedColor)
		dc.SetFontFace(face(regularFont, 32))
		dc.DrawString(joinDot(parts), 80, 470)
	}

	// Bottom-left: current CFS with band color
	if d.HasCfs {
		bc := bandColor(d.FlowStatus)
		dc.SetColor(bc)
		dc.DrawRoundedRectangle(80, 520, 380, 70, 14)
		dc.Fill()
		dc.SetColor(fgColor)
		dc.SetFontFace(face(boldFont, 38))
		dc.DrawString(fmt.Sprintf("%s cfs", formatThousands(d.CurrentCfs)), 100, 568)
		dc.SetFontFace(face(boldFont, 22))
		dc.DrawStringAnchored(strUpper(displayFlowStatus(d.FlowStatus)), 440, 555, 1.0, 0.5)
	}

	// Bottom-right: URL footer
	dc.SetColor(mutedColor)
	dc.SetFontFace(face(regularFont, 22))
	dc.DrawStringAnchored("h2oflows.app/reaches/"+d.Slug, float64(Width-80), 565, 1.0, 0.5)

	var buf bytes.Buffer
	if err := dc.EncodePNG(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ReportData is the input to RenderReport.
type ReportData struct {
	Title      string
	ReachName  string
	Handle     string  // without leading @
	ReportDate string  // "2026-05-18"
	FlowCfs    float64
	HasCfs     bool
	FlowBand   string // "low" / "running" / "high"
	Paddled    bool
	ID         string
}

// RenderReport produces a PNG share card for a report.
func RenderReport(d ReportData) ([]byte, error) {
	dc := gg.NewContext(Width, Height)
	dc.SetColor(bgColor)
	dc.Clear()

	// Top bar
	dc.SetColor(accentColor)
	dc.DrawCircle(80, 70, 18)
	dc.Fill()
	dc.SetColor(fgColor)
	dc.SetFontFace(face(boldFont, 28))
	dc.DrawString("H2OFlows", 110, 80)

	// "REPORT" badge top-right
	dc.SetColor(mutedColor)
	dc.SetFontFace(face(boldFont, 22))
	dc.DrawStringAnchored("REPORT", float64(Width-80), 80, 1.0, 0.5)

	// Reach name (eyebrow) — sits above the title with clear gap
	if d.ReachName != "" {
		dc.SetColor(accentColor)
		dc.SetFontFace(face(boldFont, 24))
		dc.DrawString(strUpper(d.ReachName), 80, 180)
	}

	// Title (large)
	dc.SetColor(fgColor)
	dc.SetFontFace(face(boldFont, 60))
	wrapAndDraw(dc, d.Title, 80, 260, float64(Width-160), 72)

	// Footer block — date + handle + CFS
	dc.SetColor(mutedColor)
	dc.SetFontFace(face(regularFont, 28))
	footerParts := []string{}
	if d.ReportDate != "" {
		footerParts = append(footerParts, formatReportDate(d.ReportDate))
	}
	if d.Handle != "" {
		footerParts = append(footerParts, "@"+d.Handle)
	}
	if d.Paddled {
		footerParts = append(footerParts, "paddled")
	}
	dc.DrawString(joinDot(footerParts), 80, 510)

	if d.HasCfs {
		bc := bandColor(d.FlowBand)
		dc.SetColor(bc)
		dc.DrawRoundedRectangle(80, 540, 380, 60, 12)
		dc.Fill()
		dc.SetColor(fgColor)
		dc.SetFontFace(face(boldFont, 32))
		dc.DrawString(fmt.Sprintf("%s cfs", formatThousands(d.FlowCfs)), 100, 582)
		if d.FlowBand != "" {
			dc.SetFontFace(face(boldFont, 20))
			dc.DrawStringAnchored(strUpper(d.FlowBand), 440, 570, 1.0, 0.5)
		}
	}

	// URL footer right
	dc.SetColor(mutedColor)
	dc.SetFontFace(face(regularFont, 20))
	dc.DrawStringAnchored("h2oflows.app/reports/"+d.ID, float64(Width-80), 575, 1.0, 0.5)

	var buf bytes.Buffer
	if err := dc.EncodePNG(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// GaugeData is the input to RenderGauge.
type GaugeData struct {
	Name       string
	Source     string  // "USGS", "DWR", etc.
	CurrentCfs float64
	HasCfs     bool
	FlowStatus string
	ID         string
}

// RenderGauge produces a PNG share card for a gauge.
func RenderGauge(d GaugeData) ([]byte, error) {
	dc := gg.NewContext(Width, Height)
	dc.SetColor(bgColor)
	dc.Clear()

	// Top bar
	dc.SetColor(accentColor)
	dc.DrawCircle(80, 70, 18)
	dc.Fill()
	dc.SetColor(fgColor)
	dc.SetFontFace(face(boldFont, 28))
	dc.DrawString("H2OFlows", 110, 80)

	// Source chip
	if d.Source != "" {
		dc.SetColor(mutedColor)
		dc.SetFontFace(face(boldFont, 22))
		dc.DrawStringAnchored(strUpper(d.Source), float64(Width-80), 80, 1.0, 0.5)
	}

	// Eyebrow
	dc.SetColor(accentColor)
	dc.SetFontFace(face(boldFont, 24))
	dc.DrawString("GAUGE", 80, 180)

	// Name (large)
	dc.SetColor(fgColor)
	dc.SetFontFace(face(boldFont, 56))
	wrapAndDraw(dc, d.Name, 80, 260, float64(Width-160), 70)

	// Big CFS centerpiece — number + "cfs" stacked horizontally with clear gap
	if d.HasCfs {
		bc := bandColor(d.FlowStatus)
		dc.SetColor(bc)
		dc.SetFontFace(face(boldFont, 120))
		numStr := formatThousands(d.CurrentCfs)
		dc.DrawString(numStr, 80, 540)
		numW, _ := dc.MeasureString(numStr)
		dc.SetFontFace(face(regularFont, 36))
		dc.SetColor(mutedColor)
		dc.DrawString("cfs", 100+numW, 540)
		dc.SetColor(bc)
		dc.SetFontFace(face(boldFont, 28))
		dc.DrawString(strUpper(displayFlowStatus(d.FlowStatus)), 80, 590)
	}

	// URL footer
	dc.SetColor(mutedColor)
	dc.SetFontFace(face(regularFont, 20))
	dc.DrawStringAnchored("h2oflows.app/gauges/"+d.ID, float64(Width-80), 575, 1.0, 0.5)

	var buf bytes.Buffer
	if err := dc.EncodePNG(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---- helpers ----------------------------------------------------------------

func strUpper(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			r = r - 'a' + 'A'
		}
		out = append(out, r)
	}
	return string(out)
}

func joinDot(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "  ·  " + p
	}
	return out
}

func formatThousands(n float64) string {
	if n < 0 {
		return "-" + formatThousands(-n)
	}
	i := int64(n + 0.5)
	s := fmt.Sprintf("%d", i)
	if len(s) <= 3 {
		return s
	}
	rev := []byte(s)
	out := make([]byte, 0, len(s)+len(s)/3)
	for i, c := range rev {
		if i > 0 && (len(rev)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, byte(c))
	}
	return string(out)
}

// formatReportDate accepts "yyyy-mm-dd" and returns "Mon DD, YYYY".
func formatReportDate(d string) string {
	if len(d) != 10 {
		return d
	}
	yr := d[0:4]
	mo := d[5:7]
	day := d[8:10]
	months := []string{"", "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	moNum := 0
	for i := 0; i < len(mo); i++ {
		moNum = moNum*10 + int(mo[i]-'0')
	}
	if moNum < 1 || moNum > 12 {
		return d
	}
	dayNum := 0
	for i := 0; i < len(day); i++ {
		dayNum = dayNum*10 + int(day[i]-'0')
	}
	return fmt.Sprintf("%s %d, %s", months[moNum], dayNum, yr)
}

func displayFlowStatus(s string) string {
	switch s {
	case "low":
		return "Low"
	case "running":
		return "Running"
	case "high":
		return "High"
	default:
		return "Unknown"
	}
}

// wrapAndDraw lays out s into lines that fit `maxWidth`, drawing each at y
// stepping by `lineHeight`. Drops lines past 2 (truncation with ellipsis).
func wrapAndDraw(dc *gg.Context, s string, x, y, maxWidth, lineHeight float64) {
	words := splitSpaces(s)
	if len(words) == 0 {
		return
	}
	lines := []string{}
	current := ""
	for _, w := range words {
		probe := current
		if probe != "" {
			probe += " "
		}
		probe += w
		pw, _ := dc.MeasureString(probe)
		if pw > maxWidth && current != "" {
			lines = append(lines, current)
			current = w
		} else {
			current = probe
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	if len(lines) > 2 {
		lines = lines[:2]
		lines[1] = truncateToWidth(dc, lines[1]+"…", maxWidth)
	}
	for i, ln := range lines {
		dc.DrawString(ln, x, y+float64(i)*lineHeight)
	}
}

func splitSpaces(s string) []string {
	out := []string{}
	cur := ""
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func truncateToWidth(dc *gg.Context, s string, maxWidth float64) string {
	w, _ := dc.MeasureString(s)
	if w <= maxWidth {
		return s
	}
	for len(s) > 1 {
		s = s[:len(s)-1]
		probe := s + "…"
		pw, _ := dc.MeasureString(probe)
		if pw <= maxWidth {
			return probe
		}
	}
	return s
}
