package ui

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
)

// TestReedSolomonKnownAnswer: the error correction of the classic
// "HELLO WORLD" 1-M example (thonky.com's QR tutorial).
func TestReedSolomonKnownAnswer(t *testing.T) {
	data := []byte{32, 91, 11, 120, 209, 114, 220, 77, 67, 64, 236, 17, 236, 17, 236, 17}
	want := []byte{196, 35, 39, 119, 235, 215, 231, 226, 93, 23}
	if got := rsRemainder(data, rsDivisor(10)); !bytes.Equal(got, want) {
		t.Fatalf("ecc = %v, want %v", got, want)
	}
}

// TestQRFormatAndVersionBits: the format and version information match
// the tables of ISO/IEC 18004 (level M, masks 0-7; versions 7-10).
func TestQRFormatAndVersionBits(t *testing.T) {
	format := []string{
		"101010000010010", "101000100100101", "101111001111100", "101101101001011",
		"100010111111001", "100000011001110", "100111110010111", "100101010100000",
	}
	for m, want := range format {
		got := qrFormatBits(m)
		var s strings.Builder
		for i := 14; i >= 0; i-- {
			s.WriteByte('0' + byte(got>>i&1))
		}
		if s.String() != want {
			t.Errorf("mask %d: format %s, want %s", m, s.String(), want)
		}
	}
	for ver, want := range map[int]int{7: 0x07C94, 8: 0x085BC, 9: 0x09A99, 10: 0x0A4D3} {
		q := newQR(ver)
		got := 0
		for i := 0; i < 18; i++ {
			a, b := q.size-11+i%3, i/3
			if q.modules[b][a] != q.modules[a][b] {
				t.Fatalf("version %d: the two version blocks differ", ver)
			}
			if q.modules[b][a] {
				got |= 1 << i
			}
		}
		if got != want {
			t.Errorf("version %d: bits %#x, want %#x", ver, got, want)
		}
	}
	// Capacities (data codewords at level M).
	for ver, want := range map[int]int{1: 16, 2: 28, 3: 44, 4: 64, 5: 86, 6: 108, 7: 124, 8: 154, 9: 182, 10: 216} {
		if got := qrDataCodewords(ver); got != want {
			t.Errorf("version %d: %d data codewords, want %d", ver, got, want)
		}
	}
}

// qrDecode reads a symbol back (format, unmasking, zigzag order,
// de-interleaving, a Reed-Solomon syndrome check per block, byte mode).
func qrDecode(t *testing.T, q *qrCode) []byte {
	t.Helper()
	n := q.size
	// Finder patterns in three corners.
	for _, c := range [][2]int{{3, 3}, {n - 4, 3}, {3, n - 4}} {
		for dy := -3; dy <= 3; dy++ {
			for dx := -3; dx <= 3; dx++ {
				d := max(abs(dx), abs(dy))
				if q.modules[c[1]+dy][c[0]+dx] != (d != 2) {
					t.Fatalf("finder pattern at %v broken", c)
				}
			}
		}
	}
	// Both copies of the format information name level M and the mask.
	var f1, f2 int
	pos1 := [][2]int{{8, 0}, {8, 1}, {8, 2}, {8, 3}, {8, 4}, {8, 5}, {8, 7}, {8, 8}, {7, 8}, {5, 8}, {4, 8}, {3, 8}, {2, 8}, {1, 8}, {0, 8}}
	for i, p := range pos1 {
		if q.modules[p[1]][p[0]] {
			f1 |= 1 << i
		}
	}
	for i := 0; i < 8; i++ {
		if q.modules[8][n-1-i] {
			f2 |= 1 << i
		}
	}
	for i := 8; i < 15; i++ {
		if q.modules[n-15+i][8] {
			f2 |= 1 << i
		}
	}
	if f1 != f2 {
		t.Fatalf("format copies differ: %b %b", f1, f2)
	}
	mask := -1
	for m := 0; m < 8; m++ {
		if qrFormatBits(m) == f1 {
			mask = m
		}
	}
	if mask < 0 {
		t.Fatalf("format bits %b are not level M", f1)
	}
	if !q.modules[n-8][8] {
		t.Fatal("no dark module")
	}
	// Unmask and read the codewords.
	ref := newQR(q.version)
	var bits []bool
	for right := n - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < n; vert++ {
			for j := 0; j < 2; j++ {
				x, y := right-j, vert
				if (right+1)&2 == 0 {
					y = n - 1 - vert
				}
				if ref.function[y][x] {
					continue
				}
				bits = append(bits, q.modules[y][x] != qrMasked(mask, x, y))
			}
		}
	}
	raw := qrRawDataModules(q.version) / 8
	cw := make([]byte, raw)
	for i := 0; i < raw*8; i++ {
		if bits[i] {
			cw[i>>3] |= 1 << (7 - i&7)
		}
	}
	numBlocks, eccLen := qrNumBlocks[q.version], qrECCPerBlock[q.version]
	numShort := numBlocks - raw%numBlocks
	shortLen := raw / numBlocks
	blocks := make([][]byte, numBlocks)
	k := 0
	for i := 0; i <= shortLen; i++ {
		for j := range blocks {
			if i == shortLen-eccLen && j < numShort {
				continue
			}
			blocks[j] = append(blocks[j], cw[k])
			k++
		}
	}
	var data []byte
	for j, b := range blocks {
		for i := 0; i < eccLen; i++ { // S_i = c(alpha^i) must be 0
			x := byte(1)
			for e := 0; e < i; e++ {
				x = gfMul(x, 2)
			}
			var s byte
			for _, c := range b {
				s = gfMul(s, x) ^ c
			}
			if s != 0 {
				t.Fatalf("block %d: syndrome %d is %d", j, i, s)
			}
		}
		data = append(data, b[:len(b)-eccLen]...)
	}
	// Byte mode segment.
	r := 0
	read := func(n int) int {
		v := 0
		for i := 0; i < n; i++ {
			v = v<<1 | int(data[r>>3]>>(7-r&7)&1)
			r++
		}
		return v
	}
	if m := read(4); m != 0b0100 {
		t.Fatalf("mode %04b, want byte mode", m)
	}
	cb := 8
	if q.version >= 10 {
		cb = 16
	}
	out := make([]byte, read(cb))
	for i := range out {
		out[i] = byte(read(8))
	}
	return out
}

func TestQRRoundTrip(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	uri := "otpauth://totp/Mailroom:owner%40example.org?secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP&issuer=Mailroom&algorithm=SHA1&digits=6&period=30"
	cases := [][]byte{[]byte("A"), []byte(uri), []byte(strings.Repeat("x", 213))}
	for _, n := range []int{14, 26, 42, 62, 84, 106, 122, 152, 180, 200} {
		b := make([]byte, n)
		rnd.Read(b)
		cases = append(cases, b)
	}
	versions := map[int]bool{}
	for _, in := range cases {
		q, err := encodeQR(in)
		if err != nil {
			t.Fatalf("%d bytes: %v", len(in), err)
		}
		versions[q.version] = true
		if got := qrDecode(t, q); !bytes.Equal(got, in) {
			t.Fatalf("version %d: decoded %q, want %q", q.version, got, in)
		}
	}
	if len(versions) < 8 {
		t.Fatalf("only versions %v tested", versions)
	}
	if _, err := encodeQR(make([]byte, 214)); err != errQRTooLong {
		t.Fatalf("214 bytes: %v", err)
	}
	svg, err := qrSVG(uri, `Key "QR"`)
	if err != nil {
		t.Fatal(err)
	}
	s := string(svg)
	if !strings.HasPrefix(s, `<svg class="qr"`) || !strings.Contains(s, `aria-label="Key &#34;QR&#34;"`) || strings.Contains(s, "otpauth") {
		t.Fatalf("svg = %.300s", s)
	}
}
