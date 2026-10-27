// Package hostagent implements the device side of the host agent protocol: a
// small program on the target host that receives text and files over the USB
// serial port and pastes them where the cursor is. The wire format is shared
// with the browser, which sends blob frames through a data channel that the
// device relays to the port.
package hostagent

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
)

const (
	magic      = "JKVM"
	Version    = 1
	headerSize = 16
	crcSize    = 4

	// MaxPayload bounds a single frame. Blob chunks stay well below it so a
	// frame always fits one data channel message.
	MaxPayload = 64 * 1024

	// SeqDeviceBit marks sequence numbers of frames the device originates, so
	// their acknowledgements never collide with frames relayed for the browser.
	SeqDeviceBit = uint32(1) << 31
)

// Type identifies a frame.
type Type uint8

const (
	TypeHello      Type = 0x01
	TypeAck        Type = 0x02
	TypeNack       Type = 0x03
	TypePing       Type = 0x04
	TypePong       Type = 0x05
	TypeBye        Type = 0x06
	TypeText       Type = 0x10
	TypeBlobBegin  Type = 0x20
	TypeBlobData   Type = 0x21
	TypeBlobEnd    Type = 0x22
	TypeBlobCancel Type = 0x23
	TypeStatus     Type = 0x30
)

func (t Type) String() string {
	switch t {
	case TypeHello:
		return "hello"
	case TypeAck:
		return "ack"
	case TypeNack:
		return "nack"
	case TypePing:
		return "ping"
	case TypePong:
		return "pong"
	case TypeBye:
		return "bye"
	case TypeText:
		return "text"
	case TypeBlobBegin:
		return "blob_begin"
	case TypeBlobData:
		return "blob_data"
	case TypeBlobEnd:
		return "blob_end"
	case TypeBlobCancel:
		return "blob_cancel"
	case TypeStatus:
		return "status"
	}
	return "unknown"
}

// FlagTextClipboard asks the agent to put the text on the clipboard and press
// Ctrl+V instead of typing it with Unicode key events.
const FlagTextClipboard uint8 = 0x01

// Frame is one protocol message.
type Frame struct {
	Type    Type
	Flags   uint8
	Seq     uint32
	Payload []byte
}

var (
	errShort      = errors.New("hostagent: incomplete frame")
	ErrBadMagic   = errors.New("hostagent: bad magic")
	ErrBadVersion = errors.New("hostagent: unsupported frame version")
	ErrBadCRC     = errors.New("hostagent: bad crc")
	ErrTooLarge   = errors.New("hostagent: payload too large")
)

// Encode serializes the frame: magic, version, type, flags, reserved, seq,
// length, payload and a CRC-32 over everything before it. Integers are
// little-endian.
func (f Frame) Encode() []byte {
	b := make([]byte, headerSize+len(f.Payload)+crcSize)
	copy(b, magic)
	b[4] = Version
	b[5] = byte(f.Type)
	b[6] = f.Flags
	binary.LittleEndian.PutUint32(b[8:], f.Seq)
	binary.LittleEndian.PutUint32(b[12:], uint32(len(f.Payload)))
	copy(b[headerSize:], f.Payload)
	end := headerSize + len(f.Payload)
	binary.LittleEndian.PutUint32(b[end:], crc32.ChecksumIEEE(b[:end]))
	return b
}

// Decode parses the frame at the start of b and returns how many bytes it
// occupies. The payload is copied, so b may be reused.
func Decode(b []byte) (Frame, int, error) {
	var f Frame
	if len(b) < headerSize {
		return f, 0, errShort
	}
	if string(b[:4]) != magic {
		return f, 0, ErrBadMagic
	}
	if b[4] != Version {
		return f, 0, ErrBadVersion
	}
	length := binary.LittleEndian.Uint32(b[12:16])
	if length > MaxPayload {
		return f, 0, ErrTooLarge
	}
	n := headerSize + int(length) + crcSize
	if len(b) < n {
		return f, 0, errShort
	}
	if crc32.ChecksumIEEE(b[:n-crcSize]) != binary.LittleEndian.Uint32(b[n-crcSize:n]) {
		return f, 0, ErrBadCRC
	}
	f.Type = Type(b[5])
	f.Flags = b[6]
	f.Seq = binary.LittleEndian.Uint32(b[8:12])
	f.Payload = append([]byte(nil), b[headerSize:n-crcSize]...)
	return f, n, nil
}

// DecodeExact parses b as exactly one frame with nothing after it.
func DecodeExact(b []byte) (Frame, error) {
	f, n, err := Decode(b)
	if err != nil {
		return f, err
	}
	if n != len(b) {
		return f, errors.New("hostagent: trailing bytes after frame")
	}
	return f, nil
}

// Decoder reassembles frames from a byte stream. Bytes that are not part of a
// valid frame are skipped, so a decoder can watch console traffic for the
// agent's first frame.
type Decoder struct {
	buf []byte
}

// Feed appends stream bytes.
func (d *Decoder) Feed(b []byte) {
	d.buf = append(d.buf, b...)
}

// Reset drops buffered bytes.
func (d *Decoder) Reset() {
	d.buf = d.buf[:0]
}

// Next returns the next complete frame, if any.
func (d *Decoder) Next() (Frame, bool) {
	for {
		i := bytes.Index(d.buf, []byte(magic))
		if i < 0 {
			// Keep a possible partial magic at the end.
			if keep := len(magic) - 1; len(d.buf) > keep {
				d.buf = append(d.buf[:0], d.buf[len(d.buf)-keep:]...)
			}
			return Frame{}, false
		}
		if i > 0 {
			d.buf = append(d.buf[:0], d.buf[i:]...)
		}
		f, n, err := Decode(d.buf)
		switch {
		case err == nil:
			d.buf = append(d.buf[:0], d.buf[n:]...)
			return f, true
		case errors.Is(err, errShort):
			return Frame{}, false
		default:
			// Not a frame after all; resume the search after this magic.
			d.buf = append(d.buf[:0], d.buf[1:]...)
		}
	}
}

// AckPayload builds the payload of an ACK or NACK: the acknowledged sequence
// number followed by an optional note.
func AckPayload(seq uint32, note string) []byte {
	b := make([]byte, 4+len(note))
	binary.LittleEndian.PutUint32(b, seq)
	copy(b[4:], note)
	return b
}

// ParseAck splits an ACK or NACK payload.
func ParseAck(payload []byte) (seq uint32, note string, ok bool) {
	if len(payload) < 4 {
		return 0, "", false
	}
	return binary.LittleEndian.Uint32(payload), string(payload[4:]), true
}
