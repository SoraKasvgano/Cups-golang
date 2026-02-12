package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPPD_ImageableAreaConvertsRightTopToMargins(t *testing.T) {
	dir := t.TempDir()
	ppdPath := filepath.Join(dir, "test.ppd")

	// Use exact point values for A4 in hundredths of mm:
	// 21000 -> 595.275590... points, 29700 -> 841.889763... points.
	// Margins of 18pt convert exactly to 635 (1/100 mm).
	content := `*PPD-Adobe: "4.3"
*FormatVersion: "4.3"
*PageSize iso_a4_210x297mm/A4: "<</PageSize[595.2756 841.8898]>>setpagedevice"
*ImageableArea iso_a4_210x297mm: "18 18 577.2756 823.8898"
`
	if err := os.WriteFile(ppdPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp ppd: %v", err)
	}

	ppd, err := LoadPPD(ppdPath)
	if err != nil {
		t.Fatalf("LoadPPD: %v", err)
	}

	size, ok := ppd.PageSizes["iso_a4_210x297mm"]
	if !ok {
		t.Fatalf("expected PageSize entry")
	}
	const want = 635
	if size.Left != want || size.Bottom != want || size.Right != want || size.Top != want {
		t.Fatalf("expected margins %d, got left=%d bottom=%d right=%d top=%d", want, size.Left, size.Bottom, size.Right, size.Top)
	}
}
