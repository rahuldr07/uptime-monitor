//go:build zbar

package qr

// Opt-in check that a real decoder reads what Encode produces. It renders
// strings across versions 1 to 10 to SVG, rasterises the path in Python and
// decodes the bitmap with libzbar (via ctypes, nothing is installed). Run:
//
//	go test -tags zbar -run TestDecodesWithZbar -v ./internal/qr
//
// It is skipped when python3 or libzbar.so.0 is missing.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const zbarScript = `
import ctypes, glob, re, sys
try:
    z = ctypes.CDLL("libzbar.so.0")
except OSError:
    print("SKIP libzbar not found"); sys.exit(3)
V, L, U = ctypes.c_void_p, ctypes.c_ulong, ctypes.c_uint
z.zbar_image_scanner_create.restype = V
z.zbar_image_create.restype = V
z.zbar_image_first_symbol.restype = V
z.zbar_image_first_symbol.argtypes = [V]
z.zbar_symbol_get_data.restype = ctypes.c_char_p
z.zbar_symbol_get_data.argtypes = [V]
z.zbar_scan_image.argtypes = [V, V]
z.zbar_image_set_format.argtypes = [V, L]
z.zbar_image_set_size.argtypes = [V, U, U]
z.zbar_image_set_data.argtypes = [V, V, L, V]
def render(svg, px=6):
    n = int(re.search(r'viewBox="0 0 (\d+)', svg).group(1))
    d = re.search(r' d="([^"]*)"', svg).group(1)
    g = [[255] * n for _ in range(n)]
    for x, y, w in re.findall(r'M(\d+) (\d+)h(\d+)v1h-\d+z', d):
        for i in range(int(w)):
            g[int(y)][int(x) + i] = 0
    buf = bytearray()
    for row in g:
        r = bytearray()
        for v in row:
            r += bytes([v]) * px
        for _ in range(px):
            buf += r
    return n * px, bytes(buf)
bad = 0
for f in sorted(glob.glob(sys.argv[1] + "/*.svg")):
    want = open(f[:-4] + ".txt", "rb").read()
    w, buf = render(open(f).read())
    sc, img = z.zbar_image_scanner_create(), z.zbar_image_create()
    z.zbar_image_set_format(img, int.from_bytes(b"Y800", "little"))
    z.zbar_image_set_size(img, w, w)
    keep = ctypes.create_string_buffer(buf, len(buf))
    z.zbar_image_set_data(img, keep, len(buf), None)
    z.zbar_scan_image(sc, img)
    sym = z.zbar_image_first_symbol(img)
    got = z.zbar_symbol_get_data(sym) if sym else None
    ok = got == want
    bad += not ok
    print(("ok   " if ok else "FAIL ") + f.split("/")[-1], len(want), "bytes", "" if ok else repr(got))
sys.exit(1 if bad else 0)
`

func TestDecodesWithZbar(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	long := "otpauth://totp/Uptime%20%28status.example.com%29:someone%40example.com?secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP&issuer=Uptime%20%28status.example.com%29&algorithm=SHA1&digits=6&period=30"
	dir := t.TempDir()
	var inputs []string
	for _, n := range []int{1, 14, 15, 26, 27, 42, 43, 62, 63, 84, 85, 106, 107, 122, 123, 152, 153, 180, 181, 213} {
		inputs = append(inputs, strings.Repeat("Ab3-", 60)[:n])
	}
	inputs = append(inputs, long[:120], long)
	for i, s := range inputs {
		c, err := Encode(s)
		if err != nil {
			t.Fatal(err)
		}
		base := filepath.Join(dir, fmt.Sprintf("%02d", i))
		if err := os.WriteFile(base+".svg", []byte(c.SVG(1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(base+".txt", []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command("python3", "-I", "-c", zbarScript, dir).CombinedOutput()
	t.Logf("%s", out)
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 3 {
		t.Skip("libzbar.so.0 not available")
	}
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
}
