package ui

// A small QR code encoder (ISO/IEC 18004) for the two-factor setup page:
// the otpauth:// link as a QR code an authenticator app can scan. It is
// written here rather than vendored because the page needs only a little
// of the standard: byte mode, error correction level M, versions 1-10
// (up to 213 bytes, far more than an otpauth link). It follows the
// structure of Project Nayuki's reference implementation. The code is
// rendered as inline SVG, so no image request and no CSP change.

import (
	"errors"
	"fmt"
	"html/template"
	"strings"
)

// qrMaxVersion is the largest version the encoder supports.
const qrMaxVersion = 10

// qrECCPerBlock and qrNumBlocks are the error correction codewords per
// block and the number of blocks at level M, by version (index 0 unused).
var (
	qrECCPerBlock = [qrMaxVersion + 1]int{0, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26}
	qrNumBlocks   = [qrMaxVersion + 1]int{0, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5}
)

// errQRTooLong is returned for data that doesn't fit version 10-M.
var errQRTooLong = errors.New("qr: data too long")

// qrCode is an encoded symbol: modules[y][x], true = dark.
type qrCode struct {
	version  int
	size     int
	mask     int
	modules  [][]bool
	function [][]bool // function patterns (finder, timing, alignment, format, version)
}

// qrRawDataModules is the number of modules available for data and
// error correction in a version (everything but the function patterns).
func qrRawDataModules(ver int) int {
	n := (16*ver+128)*ver + 64
	if ver >= 2 {
		align := ver/7 + 2
		n -= (25*align-10)*align - 55
		if ver >= 7 {
			n -= 36
		}
	}
	return n
}

// qrDataCodewords is the number of data codewords of a version at level M.
func qrDataCodewords(ver int) int {
	return qrRawDataModules(ver)/8 - qrECCPerBlock[ver]*qrNumBlocks[ver]
}

// encodeQR encodes data in byte mode at level M, in the smallest version
// that fits, with the mask of the lowest penalty.
func encodeQR(data []byte) (*qrCode, error) {
	ver := 0
	for v := 1; v <= qrMaxVersion; v++ {
		countBits := 8
		if v >= 10 {
			countBits = 16
		}
		if 4+countBits+8*len(data) <= qrDataCodewords(v)*8 {
			ver = v
			break
		}
	}
	if ver == 0 {
		return nil, errQRTooLong
	}
	// Bit stream: mode 0100 (byte), character count, data, terminator,
	// padding to whole bytes, then the pad codewords 0xEC 0x11.
	var bits []bool
	put := func(v, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, v>>i&1 == 1)
		}
	}
	put(0b0100, 4)
	if ver >= 10 {
		put(len(data), 16)
	} else {
		put(len(data), 8)
	}
	for _, b := range data {
		put(int(b), 8)
	}
	capBits := qrDataCodewords(ver) * 8
	put(0, min(4, capBits-len(bits)))
	put(0, (8-len(bits)%8)%8)
	for pad := 0xEC; len(bits) < capBits; pad ^= 0xEC ^ 0x11 {
		put(pad, 8)
	}
	codewords := make([]byte, len(bits)/8)
	for i, b := range bits {
		if b {
			codewords[i>>3] |= 1 << (7 - i&7)
		}
	}

	q := newQR(ver)
	q.drawCodewords(qrInterleave(ver, codewords))
	best, bestPenalty := 0, -1
	for m := 0; m < 8; m++ {
		q.applyMask(m)
		q.drawFormatBits(m)
		if p := q.penalty(); bestPenalty < 0 || p < bestPenalty {
			best, bestPenalty = m, p
		}
		q.applyMask(m) // XOR again undoes it
	}
	q.mask = best
	q.applyMask(best)
	q.drawFormatBits(best)
	return q, nil
}

// newQR returns an empty symbol of version ver with its function patterns.
func newQR(ver int) *qrCode {
	size := ver*4 + 17
	q := &qrCode{version: ver, size: size, modules: make([][]bool, size), function: make([][]bool, size)}
	for i := range q.modules {
		q.modules[i] = make([]bool, size)
		q.function[i] = make([]bool, size)
	}
	for i := 0; i < size; i++ {
		q.setFunction(6, i, i%2 == 0)
		q.setFunction(i, 6, i%2 == 0)
	}
	q.drawFinder(3, 3)
	q.drawFinder(size-4, 3)
	q.drawFinder(3, size-4)
	pos := qrAlignmentPositions(ver)
	for i := range pos {
		for j := range pos {
			if (i == 0 && j == 0) || (i == 0 && j == len(pos)-1) || (i == len(pos)-1 && j == 0) {
				continue // the finder patterns are there
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					q.setFunction(pos[i]+dx, pos[j]+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	q.drawFormatBits(0) // reserves the format areas; redrawn per mask
	if ver >= 7 {
		rem := ver
		for i := 0; i < 12; i++ {
			rem = rem<<1 ^ (rem>>11)*0x1F25
		}
		bits := ver<<12 | rem
		for i := 0; i < 18; i++ {
			dark := bits>>i&1 == 1
			a, b := size-11+i%3, i/3
			q.setFunction(a, b, dark)
			q.setFunction(b, a, dark)
		}
	}
	return q
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func (q *qrCode) setFunction(x, y int, dark bool) {
	q.modules[y][x] = dark
	q.function[y][x] = true
}

// drawFinder draws a finder pattern with its separator around (x, y).
func (q *qrCode) drawFinder(x, y int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			xx, yy := x+dx, y+dy
			if xx >= 0 && xx < q.size && yy >= 0 && yy < q.size {
				d := max(abs(dx), abs(dy))
				q.setFunction(xx, yy, d != 2 && d != 4)
			}
		}
	}
}

// qrAlignmentPositions are the centre coordinates of the alignment
// patterns of a version.
func qrAlignmentPositions(ver int) []int {
	if ver == 1 {
		return nil
	}
	n := ver/7 + 2
	step := (ver*8 + n*3 + 5) / (n*4 - 4) * 2
	size := ver*4 + 17
	out := make([]int, n)
	out[0] = 6
	for i := 1; i < n; i++ {
		out[n-i] = size - 7 - (i-1)*step
	}
	return out
}

// qrFormatBits is the 15-bit format information for level M and mask.
func qrFormatBits(mask int) int {
	data := 0b00<<3 | mask // level M is 00
	rem := data
	for i := 0; i < 10; i++ {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	return (data<<10 | rem) ^ 0x5412
}

// drawFormatBits draws both copies of the format information.
func (q *qrCode) drawFormatBits(mask int) {
	bits := qrFormatBits(mask)
	bit := func(i int) bool { return bits>>i&1 == 1 }
	for i := 0; i <= 5; i++ {
		q.setFunction(8, i, bit(i))
	}
	q.setFunction(8, 7, bit(6))
	q.setFunction(8, 8, bit(7))
	q.setFunction(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		q.setFunction(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		q.setFunction(q.size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		q.setFunction(8, q.size-15+i, bit(i))
	}
	q.setFunction(8, q.size-8, true) // the dark module
}

// qrInterleave splits the data codewords into blocks, adds each block's
// Reed-Solomon codewords and interleaves them.
func qrInterleave(ver int, data []byte) []byte {
	numBlocks, eccLen := qrNumBlocks[ver], qrECCPerBlock[ver]
	raw := qrRawDataModules(ver) / 8
	numShort := numBlocks - raw%numBlocks
	shortLen := raw / numBlocks
	div := rsDivisor(eccLen)
	blocks := make([][]byte, numBlocks)
	k := 0
	for i := range blocks {
		n := shortLen - eccLen
		if i >= numShort {
			n++
		}
		dat := append([]byte(nil), data[k:k+n]...)
		k += n
		ecc := rsRemainder(dat, div)
		if i < numShort {
			dat = append(dat, 0)
		}
		blocks[i] = append(dat, ecc...)
	}
	out := make([]byte, 0, raw)
	for i := range blocks[0] {
		for j, b := range blocks {
			if i != shortLen-eccLen || j >= numShort {
				out = append(out, b[i])
			}
		}
	}
	return out
}

// gfMul multiplies in GF(2^8) modulo x^8 + x^4 + x^3 + x^2 + 1.
func gfMul(x, y byte) byte {
	var z int
	for i := 7; i >= 0; i-- {
		z = z<<1 ^ (z>>7)*0x11D
		z ^= int(y>>i&1) * int(x)
	}
	return byte(z)
}

// rsDivisor is the Reed-Solomon generator polynomial of degree n (its
// coefficients from the highest power down, the leading 1 left out).
func rsDivisor(n int) []byte {
	d := make([]byte, n)
	d[n-1] = 1
	root := byte(1)
	for i := 0; i < n; i++ {
		for j := range d {
			d[j] = gfMul(d[j], root)
			if j+1 < n {
				d[j] ^= d[j+1]
			}
		}
		root = gfMul(root, 2)
	}
	return d
}

// rsRemainder is the Reed-Solomon error correction of data.
func rsRemainder(data, div []byte) []byte {
	out := make([]byte, len(div))
	for _, b := range data {
		f := b ^ out[0]
		copy(out, out[1:])
		out[len(out)-1] = 0
		for i, c := range div {
			out[i] ^= gfMul(c, f)
		}
	}
	return out
}

// drawCodewords places the codewords in the zigzag order.
func (q *qrCode) drawCodewords(data []byte) {
	i := 0
	for right := q.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5 // skip the vertical timing pattern
		}
		for vert := 0; vert < q.size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = q.size - 1 - vert // upward
				}
				if !q.function[y][x] && i < len(data)*8 {
					q.modules[y][x] = data[i>>3]>>(7-i&7)&1 == 1
					i++
				}
			}
		}
	}
}

// qrMasked reports whether mask m inverts the module at (x, y).
func qrMasked(m, x, y int) bool {
	switch m {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (x/3+y/2)%2 == 0
	case 5:
		return x*y%2+x*y%3 == 0
	case 6:
		return (x*y%2+x*y%3)%2 == 0
	default:
		return ((x+y)%2+x*y%3)%2 == 0
	}
}

// applyMask XORs mask m over the data modules.
func (q *qrCode) applyMask(m int) {
	for y := 0; y < q.size; y++ {
		for x := 0; x < q.size; x++ {
			if !q.function[y][x] && qrMasked(m, x, y) {
				q.modules[y][x] = !q.modules[y][x]
			}
		}
	}
}

// penalty is the standard mask penalty score (lower is easier to scan).
func (q *qrCode) penalty() int {
	n := q.size
	at := func(x, y int, vertical bool) bool {
		if vertical {
			return q.modules[x][y]
		}
		return q.modules[y][x]
	}
	p := 0
	finder := [][]bool{
		{true, false, true, true, true, false, true, false, false, false, false},
		{false, false, false, false, true, false, true, true, true, false, true},
	}
	for _, vertical := range []bool{false, true} {
		for y := 0; y < n; y++ {
			run := 0
			for x := 0; x < n; x++ {
				// Rule 1: runs of five or more of the same colour.
				if x > 0 && at(x, y, vertical) == at(x-1, y, vertical) {
					run++
				} else {
					run = 1
				}
				if run == 5 {
					p += 3
				} else if run > 5 {
					p++
				}
				// Rule 3: finder-like patterns 1:1:3:1:1 next to four light modules.
				for _, pat := range finder {
					if x+len(pat) > n {
						continue
					}
					match := true
					for k, dark := range pat {
						if at(x+k, y, vertical) != dark {
							match = false
							break
						}
					}
					if match {
						p += 40
					}
				}
			}
		}
	}
	// Rule 2: 2x2 blocks of one colour.
	dark := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if q.modules[y][x] {
				dark++
			}
			if x+1 < n && y+1 < n {
				c := q.modules[y][x]
				if c == q.modules[y][x+1] && c == q.modules[y+1][x] && c == q.modules[y+1][x+1] {
					p += 3
				}
			}
		}
	}
	// Rule 4: the share of dark modules away from 50 %.
	total := n * n
	k := (abs(dark*20-total*10) + total - 1) / total
	return p + (k-1)*10
}

// qrSVG renders data as a QR code in inline SVG (4 modules of quiet
// zone, dark modules as one path). The markup contains no data, only
// coordinates, so it is safe to embed.
func qrSVG(data, title string) (template.HTML, error) {
	q, err := encodeQR([]byte(data))
	if err != nil {
		return "", err
	}
	const quiet = 4
	n := q.size + 2*quiet
	var path strings.Builder
	for y := 0; y < q.size; y++ {
		for x := 0; x < q.size; x++ {
			if q.modules[y][x] {
				fmt.Fprintf(&path, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	return template.HTML(fmt.Sprintf(
		`<svg class="qr" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" role="img" aria-label="%s" shape-rendering="crispEdges">`+
			`<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="%s"/></svg>`,
		n, n, template.HTMLEscapeString(title), n, n, path.String())), nil
}
