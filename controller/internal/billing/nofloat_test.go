package billing

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Money is integers (D-64): no file of the billing package may use floating
// point. A mutation check for "float arithmetic" that no amount test could
// reliably catch.
func TestNoFloatingPoint(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\bfloat(32|64)\b|strconv\.ParseFloat|math\.(Round|Ceil|Floor)\b`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if loc := re.FindIndex(b); loc != nil {
			t.Errorf("%s uses floating point: %q", f, b[loc[0]:loc[1]])
		}
	}
}
