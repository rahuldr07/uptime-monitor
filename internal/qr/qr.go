// Package qr is a small QR code encoder: byte mode, error correction level M,
// versions 1 to 10 and one fixed mask pattern (any valid mask scans), rendered
// as SVG. It exists because the standard library has none; it serves the
// two-factor setup page.
package qr

import (
	"errors"
	"fmt"
	"strings"
)

// ErrTooLong is returned by Encode when data does not fit version 10.
var ErrTooLong = errors.New("qr: data too long")

const (
	maxVersion = 10
	// maskPattern is the one mask used; any valid mask scans.
	maskPattern = 0
)

// blockSpec is the level M block structure of one version: EC codewords per
// block and the two groups of data blocks (count, data codewords per block).
type blockSpec struct {
	ecPerBlock     int
	n1, data1      int
	n2, data2      int
	alignPositions []int
}

// specs is indexed by version-1 (ISO/IEC 18004, tables 9 and E.1).
var specs = [maxVersion]blockSpec{
	{10, 1, 16, 0, 0, nil},
	{16, 1, 28, 0, 0, []int{6, 18}},
	{26, 1, 44, 0, 0, []int{6, 22}},
	{18, 2, 32, 0, 0, []int{6, 26}},
	{24, 2, 43, 0, 0, []int{6, 30}},
	{16, 4, 27, 0, 0, []int{6, 34}},
	{18, 4, 31, 0, 0, []int{6, 22, 38}},
	{22, 2, 38, 2, 39, []int{6, 24, 42}},
	{22, 3, 36, 2, 37, []int{6, 26, 46}},
	{26, 4, 43, 1, 44, []int{6, 28, 50}},
}

func (b blockSpec) dataCodewords() int { return b.n1*b.data1 + b.n2*b.data2 }

// capacity is the number of bytes a version holds in byte mode at level M.
func capacity(version int) int {
	countBits := 8
	if version >= 10 {
		countBits = 16
	}
	return (specs[version-1].dataCodewords()*8 - 4 - countBits) / 8
}

// Code is an encoded QR symbol.
type Code struct {
	size int
	dark [][]bool
}

// Size is the number of modules along one side, without the quiet zone.
func (c *Code) Size() int { return c.size }

// Dark reports whether the module in column x, row y is dark. Coordinates
// outside the symbol are light.
func (c *Code) Dark(x, y int) bool {
	if x < 0 || y < 0 || x >= c.size || y >= c.size {
		return false
	}
	return c.dark[y][x]
}

// Encode encodes data in byte mode at level M in the smallest version that
// holds it, or returns ErrTooLong above version 10.
func Encode(data string) (*Code, error) {
	m, _, err := build(data)
	if err != nil {
		return nil, err
	}
	return &Code{size: m.size, dark: m.dark}, nil
}

// matrix is the module grid under construction. fn marks function modules
// (finders, timing, alignment, format, version, dark module); placed counts
// how often the data placement wrote each module.
type matrix struct {
	size   int
	dark   [][]bool
	fn     [][]bool
	placed [][]uint8
}

func newMatrix(size int) *matrix {
	m := &matrix{size: size}
	for range size {
		m.dark = append(m.dark, make([]bool, size))
		m.fn = append(m.fn, make([]bool, size))
		m.placed = append(m.placed, make([]uint8, size))
	}
	return m
}

func (m *matrix) setFn(x, y int, dark bool) {
	m.dark[y][x], m.fn[y][x] = dark, true
}

// build encodes data and returns the finished matrix and its version.
func build(data string) (*matrix, int, error) {
	version := 0
	for v := 1; v <= maxVersion; v++ {
		if len(data) <= capacity(v) {
			version = v
			break
		}
	}
	if version == 0 {
		return nil, 0, ErrTooLong
	}
	spec := specs[version-1]
	codewords := interleave(spec, dataCodewords(data, version, spec.dataCodewords()))
	m := newMatrix(version*4 + 17)
	m.drawFunctionPatterns(version, spec)
	m.placeData(codewords)
	m.applyMask(maskPattern)
	return m, version, nil
}

type bitBuffer struct {
	bytes []byte
	n     int // bits used
}

func (b *bitBuffer) add(v uint32, count int) {
	for i := count - 1; i >= 0; i-- {
		if b.n%8 == 0 {
			b.bytes = append(b.bytes, 0)
		}
		if v>>uint(i)&1 == 1 {
			b.bytes[b.n/8] |= 0x80 >> uint(b.n%8)
		}
		b.n++
	}
}

// dataCodewords builds the padded data codewords: mode, count, bytes,
// terminator and the alternating EC/11 pad bytes.
func dataCodewords(data string, version, total int) []byte {
	var b bitBuffer
	b.add(0b0100, 4)
	if version >= 10 {
		b.add(uint32(len(data)), 16)
	} else {
		b.add(uint32(len(data)), 8)
	}
	for i := 0; i < len(data); i++ {
		b.add(uint32(data[i]), 8)
	}
	b.add(0, min(4, total*8-b.n))
	b.add(0, (8-b.n%8)%8)
	for pad := byte(0xEC); len(b.bytes) < total; pad ^= 0xEC ^ 0x11 {
		b.bytes = append(b.bytes, pad)
	}
	return b.bytes
}

// interleave splits data into blocks, appends each block's EC codewords and
// returns the final codeword sequence.
func interleave(spec blockSpec, data []byte) []byte {
	var blocks, ecs [][]byte
	pos := 0
	for g, n := range []int{spec.n1, spec.n2} {
		size := spec.data1
		if g == 1 {
			size = spec.data2
		}
		for range n {
			blk := data[pos : pos+size]
			pos += size
			blocks = append(blocks, blk)
			ecs = append(ecs, reedSolomon(blk, spec.ecPerBlock))
		}
	}
	out := make([]byte, 0, spec.dataCodewords()+len(blocks)*spec.ecPerBlock)
	for i := 0; i < spec.data2 || i < spec.data1; i++ {
		for _, blk := range blocks {
			if i < len(blk) {
				out = append(out, blk[i])
			}
		}
	}
	for i := 0; i < spec.ecPerBlock; i++ {
		for _, ec := range ecs {
			out = append(out, ec[i])
		}
	}
	return out
}

var gfExp [512]byte
var gfLog [256]int

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		gfExp[i] = byte(x)
		gfLog[x] = i
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11D
		}
	}
	for i := 255; i < len(gfExp); i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[gfLog[a]+gfLog[b]]
}

// reedSolomon returns the n EC codewords of data: the remainder of
// data(x)*x^n divided by the generator (x-a^0)...(x-a^(n-1)) over GF(256).
func reedSolomon(data []byte, n int) []byte {
	gen := []byte{1} // highest degree first, leading 1 implicit below
	for i := 0; i < n; i++ {
		next := make([]byte, len(gen)+1)
		for j, c := range gen {
			next[j] ^= c
			next[j+1] ^= gfMul(c, gfExp[i])
		}
		gen = next
	}
	rem := make([]byte, n)
	for _, d := range data {
		f := d ^ rem[0]
		copy(rem, rem[1:])
		rem[n-1] = 0
		for j := 0; j < n; j++ {
			rem[j] ^= gfMul(gen[j+1], f)
		}
	}
	return rem
}

func (m *matrix) drawFunctionPatterns(version int, spec blockSpec) {
	size := m.size
	for i := 0; i < size; i++ {
		m.setFn(6, i, i%2 == 0) // vertical timing, column 6
		m.setFn(i, 6, i%2 == 0) // horizontal timing, row 6
	}
	for _, c := range [][2]int{{3, 3}, {size - 4, 3}, {3, size - 4}} {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y := c[0]+dx, c[1]+dy
				if x < 0 || y < 0 || x >= size || y >= size {
					continue
				}
				d := max(abs(dx), abs(dy))
				m.setFn(x, y, d != 2 && d != 4)
			}
		}
	}
	pos := spec.alignPositions
	for i, cy := range pos {
		for j, cx := range pos {
			last := len(pos) - 1
			if (i == 0 && j == 0) || (i == 0 && j == last) || (i == last && j == 0) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					m.setFn(cx+dx, cy+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	m.drawFormat(maskPattern) // reserves the format areas and the dark module
	if version >= 7 {
		m.drawVersion(version)
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// formatBits is the 15-bit format information for level M and the mask: the
// 5 data bits (level 00, mask), a BCH(15,5) remainder and the 0x5412 XOR.
func formatBits(mask int) uint32 {
	data := uint32(mask) // level M is 00
	rem := data
	for i := 0; i < 10; i++ {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	return (data<<10 | rem) ^ 0x5412
}

// versionBits is the 18-bit version information: 6 data bits and a BCH(18,6)
// remainder.
func versionBits(version int) uint32 {
	rem := uint32(version)
	for i := 0; i < 12; i++ {
		rem = rem<<1 ^ (rem>>11)*0x1F25
	}
	return uint32(version)<<12 | rem
}

func (m *matrix) drawFormat(mask int) {
	bits := formatBits(mask)
	bit := func(i int) bool { return bits>>uint(i)&1 == 1 }
	size := m.size
	for i := 0; i <= 5; i++ {
		m.setFn(8, i, bit(i))
	}
	m.setFn(8, 7, bit(6))
	m.setFn(8, 8, bit(7))
	m.setFn(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		m.setFn(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		m.setFn(size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		m.setFn(8, size-15+i, bit(i))
	}
	m.setFn(8, size-8, true) // the always-dark module
}

func (m *matrix) drawVersion(version int) {
	bits := versionBits(version)
	for i := 0; i < 18; i++ {
		dark := bits>>uint(i)&1 == 1
		a, b := m.size-11+i%3, i/3
		m.setFn(a, b, dark)
		m.setFn(b, a, dark)
	}
}

// placeData writes the codewords in the standard zig-zag over the modules
// that are not function modules; modules left after the last bit (the
// remainder bits) stay light.
func (m *matrix) placeData(codewords []byte) {
	i := 0
	for right := m.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < m.size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = m.size - 1 - vert
				}
				if m.fn[y][x] {
					continue
				}
				m.dark[y][x] = i < len(codewords)*8 && codewords[i>>3]>>uint(7-i&7)&1 == 1
				m.placed[y][x]++
				i++
			}
		}
	}
}

func (m *matrix) applyMask(mask int) {
	for y := 0; y < m.size; y++ {
		for x := 0; x < m.size; x++ {
			if !m.fn[y][x] && maskBit(mask, x, y) {
				m.dark[y][x] = !m.dark[y][x]
			}
		}
	}
}

func maskBit(mask, x, y int) bool {
	switch mask {
	case 0:
		return (x+y)%2 == 0
	}
	panic(fmt.Sprintf("qr: mask %d not implemented", mask))
}

// SVG renders the code with a 4-module quiet zone, each module scale pixels.
// The modules are one path of rectangles, merged along rows.
func (c *Code) SVG(scale int) string {
	if scale < 1 {
		scale = 1
	}
	n := c.size + 8
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" shape-rendering="crispEdges" role="img" aria-label="QR code">`,
		n, n, n*scale, n*scale)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n)
	for y := 0; y < c.size; y++ {
		for x := 0; x < c.size; {
			if !c.dark[y][x] {
				x++
				continue
			}
			start := x
			for x < c.size && c.dark[y][x] {
				x++
			}
			fmt.Fprintf(&b, "M%d %dh%dv1h-%dz", start+4, y+4, x-start, x-start)
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}
