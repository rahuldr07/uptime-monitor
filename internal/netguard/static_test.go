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
var strayClient = regexp.MustCompile(`http\.Client\{|net\.Dialer\{|var \w+ (net\.Dialer|http\.(Client|Transport))|http\.Transport\{|http\.DefaultClient|http\.DefaultTransport|http\.(Get|Post|Head|PostForm)\(|smtp\.SendMail\(|net\.Dial\w*\(|tls\.Dial\(|tls\.DialWithDialer\(|smtp\.Dial\(|new\((http\.(Client|Transport)|net\.Dialer)\)`)

// testHook matches the test-only provider constructor, which accepts any client.
const testHook = "oauth.NewForTest("

// TestNoStrayClients fails if any production file outside internal/netguard
// builds its own client, transport or dialer, which would bypass the guard.
func TestNoStrayClients(t *testing.T) {
	root := filepath.Join("..", "..")
	skipDir := filepath.Join(root, "internal", "netguard")
	oauthtestDir := filepath.Join(root, "internal", "oauth", "oauthtest")
	var hits, scanned []string
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
		rel, _ := filepath.Rel(root, path)
		scanned = append(scanned, filepath.ToSlash(rel))
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		inOauthtest := strings.HasPrefix(filepath.Clean(path), oauthtestDir+string(filepath.Separator))
		for i, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strayClient.MatchString(line) || (!inOauthtest && strings.Contains(line, testHook)) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", filepath.ToSlash(rel), i+1, strings.TrimSpace(line)))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"main.go", "internal/web/helpers.go"} {
		found := false
		for _, f := range scanned {
			if f == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("scan did not reach %s (scanned %d files); wrong root or empty walk", want, len(scanned))
		}
	}
	if len(hits) > 0 {
		t.Errorf("clients and dialers must be built through netguard:\n%s", strings.Join(hits, "\n"))
	}
}
