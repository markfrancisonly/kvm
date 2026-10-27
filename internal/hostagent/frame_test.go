package hostagent

import (
	"bytes"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	in := Frame{Type: TypeText, Flags: FlagTextClipboard, Seq: SeqDeviceBit | 7, Payload: []byte("héllo\nworld")}
	b := in.Encode()
	out, n, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(b) {
		t.Fatalf("consumed %d of %d bytes", n, len(b))
	}
	if out.Type != in.Type || out.Flags != in.Flags || out.Seq != in.Seq || !bytes.Equal(out.Payload, in.Payload) {
		t.Fatalf("got %+v, want %+v", out, in)
	}
	if _, err := DecodeExact(append(b, 'x')); err == nil {
		t.Fatal("trailing bytes were accepted")
	}
}

func TestDecodeRejectsBadFrames(t *testing.T) {
	good := Frame{Type: TypePing, Seq: 1}.Encode()

	bad := append([]byte(nil), good...)
	bad[len(bad)-1] ^= 0xff
	if _, _, err := Decode(bad); err != ErrBadCRC {
		t.Fatalf("corrupted crc: got %v", err)
	}

	bad = append([]byte(nil), good...)
	bad[4] = 9
	if _, _, err := Decode(bad); err != ErrBadVersion {
		t.Fatalf("bad version: got %v", err)
	}

	bad = append([]byte(nil), good...)
	bad[12], bad[13], bad[14], bad[15] = 0xff, 0xff, 0xff, 0xff
	if _, _, err := Decode(bad); err != ErrTooLarge {
		t.Fatalf("huge length: got %v", err)
	}

	if _, _, err := Decode(good[:headerSize-1]); err != errShort {
		t.Fatalf("short header: got %v", err)
	}
	if _, _, err := Decode([]byte("not a frame at all")); err != ErrBadMagic {
		t.Fatalf("bad magic: got %v", err)
	}
}

func TestDecoderResyncsAroundGarbage(t *testing.T) {
	first := Frame{Type: TypeHello, Seq: 1, Payload: []byte(`{"v":1}`)}
	second := Frame{Type: TypePing, Seq: 2}
	corrupt := Frame{Type: TypePong, Seq: 3}.Encode()
	corrupt[len(corrupt)-2] ^= 0x55

	var stream []byte
	stream = append(stream, []byte("console noise JK")...) // partial magic in the noise
	stream = append(stream, first.Encode()...)
	stream = append(stream, corrupt...)
	stream = append(stream, []byte("JKVMjunk")...)
	stream = append(stream, second.Encode()...)

	var d Decoder
	var got []Frame
	// Feed one byte at a time to exercise partial frames.
	for _, c := range stream {
		d.Feed([]byte{c})
		for {
			f, ok := d.Next()
			if !ok {
				break
			}
			got = append(got, f)
		}
	}
	if len(got) != 2 || got[0].Type != TypeHello || got[0].Seq != 1 || got[1].Type != TypePing || got[1].Seq != 2 {
		t.Fatalf("decoded %+v", got)
	}
}

func TestAckPayload(t *testing.T) {
	seq, note, ok := ParseAck(AckPayload(SeqDeviceBit|42, "elevated"))
	if !ok || seq != SeqDeviceBit|42 || note != "elevated" {
		t.Fatalf("got %d %q %v", seq, note, ok)
	}
	if _, _, ok := ParseAck([]byte{1, 2}); ok {
		t.Fatal("short payload was accepted")
	}
}
