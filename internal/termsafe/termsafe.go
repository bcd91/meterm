// Package termsafe keeps what a remote host sends from doing more to a terminal than
// showing text. Terminals act on escape sequences wherever they find them, so a line
// in a log can set the clipboard (OSC 52), change the title, move the cursor over
// what's already there, or answer a query with keystrokes.
//
// Writer passes text, tabs and newlines and SGR (colour) sequences, and drops every
// other escape sequence and control character, including the 8-bit forms: C1 bytes,
// raw or UTF-8 encoded. It holds back nothing but an incomplete sequence, so a followed
// log still shows each line as it arrives.
package termsafe

import (
	"bytes"
	"io"
	"unicode/utf8"
)

const (
	esc = 0x1b
	bel = 0x07
	can = 0x18
	sub = 0x1a
	// maxCSI is more than any real colour sequence; longer ones aren't kept.
	maxCSI = 64
)

type state int

const (
	ground      state = iota
	escape            // after ESC
	escapeInter       // in ESC with intermediates, up to its final byte
	csi               // in a control sequence
	text              // in a string: OSC, DCS, SOS, PM or APC
	textEscape        // after ESC in a string
)

// Writer filters what's written to it on to an underlying writer. Sequences can be
// split across writes. It isn't safe for concurrent use.
type Writer struct {
	w       io.Writer
	st      state
	seq     []byte // the current control sequence's parameters
	tooLong bool
	pending []byte // an incomplete UTF-8 sequence
	out     bytes.Buffer
}

// New returns a Writer that passes what's safe on to w.
func New(w io.Writer) *Writer { return &Writer{w: w} }

func (f *Writer) Write(p []byte) (int, error) {
	f.out.Reset()
	for _, b := range p {
		f.feed(b)
	}
	if f.out.Len() > 0 {
		if _, err := f.w.Write(f.out.Bytes()); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush forgets anything incomplete, at the end of the stream.
func (f *Writer) Flush() {
	f.pending, f.seq, f.tooLong, f.st = nil, nil, false, ground
}

// String filters a whole string.
func String(s string) string {
	var b bytes.Buffer
	_, _ = New(&b).Write([]byte(s))
	return b.String()
}

func (f *Writer) feed(b byte) {
	switch f.st {
	case ground:
		f.feedGround(b)
	case escape:
		f.feedEscape(b)
	case escapeInter:
		switch {
		case b == esc:
			f.st = escape
		case b >= 0x20 && b <= 0x2f:
		default:
			f.st = ground
			if b == '\n' {
				f.out.WriteByte(b)
			}
		}
	case csi:
		f.feedCSI(b)
	case text:
		// A line break ends it too, which no terminal does: otherwise one stray
		// byte in a log would hide everything after it from whoever's reading.
		switch b {
		case esc:
			f.st = textEscape
		case bel, can, sub, 0x9c:
			f.st = ground
		case '\n':
			f.st = ground
			f.out.WriteByte(b)
		}
	case textEscape:
		switch b {
		case '\\':
			f.st = ground
		default:
			f.st = escape
			f.feedEscape(b)
		}
	}
}

func (f *Writer) feedGround(b byte) {
	if len(f.pending) > 0 {
		f.pending = append(f.pending, b)
		if !utf8.FullRune(f.pending) {
			return
		}
		seq := f.pending
		f.pending = nil
		r, size := utf8.DecodeRune(seq)
		if r == utf8.RuneError && size <= 1 {
			// Not UTF-8, so this byte stands alone, and what followed it is looked
			// at afresh: it may be the start of something.
			f.out.WriteByte(seq[0])
			for _, c := range seq[1:] {
				f.feed(c)
			}
			return
		}
		if r >= 0x80 && r <= 0x9f {
			f.c1(byte(r))
			return
		}
		f.out.Write(seq)
		return
	}
	switch {
	case b == '\n' || b == '\t':
		f.out.WriteByte(b)
	case b == esc:
		f.st = escape
	case b < 0x20 || b == 0x7f:
	case b < 0x7f:
		f.out.WriteByte(b)
	case b >= 0x80 && b <= 0x9f:
		f.c1(b)
	case b < 0xc2:
		f.out.WriteByte(b) // not UTF-8, but a terminal only shows it
	default:
		f.pending = append(f.pending[:0], b)
	}
}

// c1 acts on a C1 control, the 8-bit form of ESC and a letter.
func (f *Writer) c1(b byte) {
	switch b {
	case 0x9b:
		f.startCSI()
	case 0x90, 0x98, 0x9d, 0x9e, 0x9f:
		f.st = text
	}
}

func (f *Writer) feedEscape(b byte) {
	switch {
	case b == '[':
		f.startCSI()
	case b == ']' || b == 'P' || b == 'X' || b == '^' || b == '_':
		f.st = text
	case b == esc:
	case b == '\n':
		f.st = ground
		f.out.WriteByte(b)
	case b >= 0x20 && b <= 0x2f:
		f.st = escapeInter
	default:
		f.st = ground
	}
}

func (f *Writer) startCSI() {
	f.st, f.seq, f.tooLong = csi, f.seq[:0], false
}

func (f *Writer) feedCSI(b byte) {
	switch {
	case b == esc:
		f.st = escape
	case b == can || b == sub:
		f.st = ground
	case b == '\n':
		f.st = ground
		f.out.WriteByte(b)
	case b < 0x20 || b == 0x7f:
	case b >= 0x40 && b <= 0x7e:
		f.st = ground
		if b == 'm' && !f.tooLong && colourParams(f.seq) {
			f.out.WriteString("\x1b[")
			f.out.Write(f.seq)
			f.out.WriteByte('m')
		}
	case b >= 0x80:
		f.st = ground // nothing in a control sequence is this; see it as text
		f.feedGround(b)
	default:
		if len(f.seq) >= maxCSI {
			f.tooLong = true
			return
		}
		f.seq = append(f.seq, b)
	}
}

func colourParams(seq []byte) bool {
	for _, b := range seq {
		if (b < '0' || b > '9') && b != ';' && b != ':' {
			return false
		}
	}
	return true
}
