package web

import (
	"os"
	"strings"
	"testing"
)

// The served licence text must contain the project and the vendored Lucide licences verbatim.
func TestLicensesTxtHasLucideLicense(t *testing.T) {
	served, err := os.ReadFile("static/licenses.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../../LICENSE", "../../third_party/lucide/LICENSE"} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(served), strings.TrimSpace(string(src))) {
			t.Fatalf("static/licenses.txt is out of sync with %s", path)
		}
	}
}
