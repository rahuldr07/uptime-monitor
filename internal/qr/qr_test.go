package qr

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestReedSolomon(t *testing.T) {
	// ISO/IEC 18004 Annex I, version 1-M.
	data := []byte{0x10, 0x20, 0x0C, 0x56, 0x61, 0x80, 0xEC, 0x11, 0xEC, 0x11, 0xEC, 0x11, 0xEC, 0x11, 0xEC, 0x11}
	want := []byte{0xA5, 0x24, 0xD4, 0xC1, 0xED, 0x36, 0xC7, 0x87, 0x2C, 0x55}
	got := reedSolomon(data, 10)
	if string(got) != string(want) {
		t.Fatalf("EC = % X, want % X", got, want)
	}
}

func TestFormatBits(t *testing.T) {
	if got := formatBits(0); got != 0b101010000010010 {
		t.Fatalf("format bits = %015b", got)
	}
}

func TestVersionBits(t *testing.T) {
	for v, want := range map[int]uint32{7: 0x07C94, 8: 0x085BC, 9: 0x09A99, 10: 0x0A4D3} {
		if got := versionBits(v); got != want {
			t.Errorf("version %d bits = %#x, want %#x", v, got, want)
		}
	}
}

func TestVersionChoice(t *testing.T) {
	for _, c := range []struct{ n, version int }{
		{0, 1}, {14, 1}, {15, 2}, {26, 2}, {27, 3}, {106, 6}, {107, 7}, {213, 10},
	} {
		m, v, err := build(strings.Repeat("a", c.n))
		if err != nil || v != c.version || m.size != 17+4*c.version {
			t.Errorf("len %d: version %d, size %d, err %v; want version %d", c.n, v, m.size, err, c.version)
		}
	}
	if _, err := Encode(strings.Repeat("a", 214)); !errors.Is(err, ErrTooLong) {
		t.Fatalf("214 bytes: %v, want ErrTooLong", err)
	}
	want := []int{14, 26, 42, 62, 84, 106, 122, 152, 180, 213}
	for v, w := range want {
		if got := capacity(v + 1); got != w {
			t.Errorf("capacity(%d) = %d, want %d", v+1, got, w)
		}
	}
}

func TestFinderAndTiming(t *testing.T) {
	for _, n := range []int{5, 30, 120, 213} {
		c, err := Encode(strings.Repeat("x", n))
		if err != nil {
			t.Fatal(err)
		}
		size := c.Size()
		version := (size - 17) / 4
		for _, o := range [][2]int{{0, 0}, {size - 7, 0}, {0, size - 7}} {
			for dy := 0; dy < 7; dy++ {
				for dx := 0; dx < 7; dx++ {
					edge := dx == 0 || dy == 0 || dx == 6 || dy == 6
					core := dx >= 2 && dx <= 4 && dy >= 2 && dy <= 4
					if got := c.Dark(o[0]+dx, o[1]+dy); got != (edge || core) {
						t.Fatalf("n=%d finder at %v (%d,%d) = %v", n, o, dx, dy, got)
					}
				}
			}
		}
		for i := 8; i < size-8; i++ {
			if c.Dark(i, 6) != (i%2 == 0) || c.Dark(6, i) != (i%2 == 0) {
				t.Fatalf("n=%d timing broken at %d", n, i)
			}
		}
		if !c.Dark(8, 4*version+9) {
			t.Fatalf("n=%d dark module missing", n)
		}
	}
}

func TestSVG(t *testing.T) {
	c, err := Encode("hello")
	if err != nil {
		t.Fatal(err)
	}
	svg := c.SVG(4)
	n := c.Size() + 8
	for _, want := range []string{
		fmt.Sprintf(`viewBox="0 0 %d %d"`, n, n), `shape-rendering="crispEdges"`, `role="img"`, `aria-label="QR code"`, `<path`,
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("SVG missing %s", want)
		}
	}
	if strings.Contains(svg, "style=") || strings.Count(svg, "<path") != 1 {
		t.Error("SVG has a style attribute or more than one path")
	}
}

// decodeFormat returns the 15 format bits recovered from a copy of the format
// information after removing the XOR mask, and whether the BCH check holds.
func decodeFormat(bits uint32) (level, mask int, ok bool) {
	bits ^= 0x5412
	data := bits >> 10
	rem := data
	for i := 0; i < 10; i++ {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	return int(data >> 3), int(data & 7), rem == bits&0x3FF
}

func TestSelfCheck(t *testing.T) {
	long := "otpauth://totp/Uptime%20%28status.example.com%29:someone%40example.com?secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP&issuer=Uptime%20%28status.example.com%29&algorithm=SHA1&digits=6&period=30"
	for _, s := range []string{
		"a", strings.Repeat("a", 14), strings.Repeat("b", 26), strings.Repeat("c", 42), strings.Repeat("d", 62),
		strings.Repeat("e", 84), strings.Repeat("f", 106), strings.Repeat("g", 122), strings.Repeat("h", 152),
		strings.Repeat("i", 180), strings.Repeat("j", 213), long[:120], long,
	} {
		m, version, err := build(s)
		if err != nil {
			t.Fatalf("len %d: %v", len(s), err)
		}
		size := m.size
		// Both copies of the format information decode to level M, mask 0.
		var c1, c2 uint32
		get := func(x, y int) uint32 {
			if m.dark[y][x] {
				return 1
			}
			return 0
		}
		for i := 0; i <= 5; i++ {
			c1 |= get(8, i) << uint(i)
		}
		c1 |= get(8, 7)<<6 | get(8, 8)<<7 | get(7, 8)<<8
		for i := 9; i < 15; i++ {
			c1 |= get(14-i, 8) << uint(i)
		}
		for i := 0; i < 8; i++ {
			c2 |= get(size-1-i, 8) << uint(i)
		}
		for i := 8; i < 15; i++ {
			c2 |= get(8, size-15+i) << uint(i)
		}
		for n, c := range []uint32{c1, c2} {
			level, mask, ok := decodeFormat(c)
			if !ok || level != 0 || mask != 0 {
				t.Errorf("len %d: format copy %d decodes to level %d mask %d ok=%v", len(s), n+1, level, mask, ok)
			}
		}
		// Every module is either a function module or was written exactly
		// once, and the count matches the standard's data module count.
		free := 0
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				switch {
				case m.fn[y][x] && m.placed[y][x] != 0:
					t.Fatalf("len %d: function module (%d,%d) overwritten", len(s), x, y)
				case !m.fn[y][x] && m.placed[y][x] != 1:
					t.Fatalf("len %d: module (%d,%d) written %d times", len(s), x, y, m.placed[y][x])
				case !m.fn[y][x]:
					free++
				}
			}
		}
		spec := specs[version-1]
		remainder := 0
		switch {
		case version >= 2 && version <= 6:
			remainder = 7
		}
		total := spec.dataCodewords() + (spec.n1+spec.n2)*spec.ecPerBlock
		if free != total*8+remainder {
			t.Errorf("len %d (v%d): %d free modules, want %d", len(s), version, free, total*8+remainder)
		}
		// Version information, both copies, for v7 and up.
		if version >= 7 {
			var a, b uint32
			for i := 0; i < 18; i++ {
				a |= get(size-11+i%3, i/3) << uint(i)
				b |= get(i/3, size-11+i%3) << uint(i)
			}
			if a != versionBits(version) || b != a {
				t.Errorf("len %d: version info %#x/%#x", len(s), a, b)
			}
		}
	}
}
