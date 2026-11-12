package web

import (
	"os"
	"strings"
	"testing"
)

// The served licence text must contain the vendored Lucide licence verbatim.
func TestLicensesTxtHasLucideLicense(t *testing.T) {
	src, err := os.ReadFile("../../third_party/lucide/LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	served, err := os.ReadFile("static/licenses.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(served), strings.TrimSpace(string(src))) {
		t.Fatal("static/licenses.txt is out of sync with third_party/lucide/LICENSE")
	}
}
