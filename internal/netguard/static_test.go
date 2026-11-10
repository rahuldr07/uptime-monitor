package netguard

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// strayClient matches code that builds an HTTP client, transport or dialer, or
// reaches the network through a package-level shortcut, without netguard.
var strayClient = regexp.MustCompile(`http\.Client\{|net\.Dialer\{|var \w+ net\.Dialer|http\.Transport\{|http\.DefaultClient|http\.DefaultTransport|http\.(Get|Post|Head|PostForm)\(|smtp\.SendMail\(|net\.Dial(Timeout)?\(|tls\.Dial\(`)

// TestNoStrayClients fails if any production file outside internal/netguard
// builds its own client, transport or dialer, which would bypass the guard.
func TestNoStrayClients(t *testing.T) {
	root := filepath.Join("..", "..")
	skipDir := filepath.Join(root, "internal", "netguard")
	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch {
			case d.Name() == ".git" || d.Name() == ".superpowers":
				return filepath.SkipDir
			case filepath.Clean(path) == filepath.Clean(skipDir):
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strayClient.MatchString(line) {
				rel, _ := filepath.Rel(root, path)
				hits = append(hits, fmt.Sprintf("%s:%d: %s", filepath.ToSlash(rel), i+1, strings.TrimSpace(line)))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Errorf("clients and dialers must be built through netguard:\n%s", strings.Join(hits, "\n"))
	}
}
