package audio

import "testing"

// Two consecutive 20 ms narrowband SILK packets from an Opus encoder.
var opusTestPackets = [][]byte{
	{
		0x08, 0x83, 0x3f, 0xa5, 0xbf, 0xd8, 0xe7, 0xe0, 0x83, 0xcc, 0x38, 0xc4, 0x2a, 0x9c, 0x3e, 0xe1,
		0x93, 0xa4, 0x2b, 0xcd, 0xca, 0x5c, 0x79, 0xac, 0x6e, 0xaa, 0x61, 0x23, 0xec, 0xdf, 0x5e, 0xcf,
		0x53, 0x78,
	},
	{
		0x08, 0x84, 0x05, 0xb1, 0xf6, 0xaf, 0xce, 0x3d, 0x5d, 0xb2, 0x57, 0x78, 0xaf, 0x58, 0x2b, 0x82,
		0x63, 0x32, 0xfa, 0xbe, 0x56, 0xfd, 0xfb, 0xdd,
	},
}

func TestOpusPlaybackDecoder(t *testing.T) {
	d, err := NewOpusPlaybackDecoder()
	if err != nil {
		t.Fatal(err)
	}
	if samples, err := d.Decode(nil); err != nil || len(samples) != 0 {
		t.Fatalf("empty packet: samples=%d err=%v", len(samples), err)
	}
	want := opusPlaybackRate / 50 * opusPlaybackChannels // 20 ms, stereo
	var energy int64
	for i, packet := range opusTestPackets {
		samples, err := d.Decode(packet)
		if err != nil {
			t.Fatalf("packet %d: %v", i+1, err)
		}
		if len(samples) != want {
			t.Fatalf("packet %d: %d samples, want %d", i+1, len(samples), want)
		}
		for _, s := range samples {
			energy += int64(s) * int64(s)
		}
	}
	if energy == 0 {
		t.Fatal("decoded audio is silent")
	}
}
