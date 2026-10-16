package scanner

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDecodeFFprobeHexDump(t *testing.T) {
	dump := "\n" +
		"00000000: 7ffe 8001 fc3c 7ff0 22c0 0138 0001 ef83  .....<..\"..8....\n" +
		"00000010: 6fff ff                                  o..\n"
	got, err := decodeFFprobeHexDump(dump)
	if err != nil {
		t.Fatalf("decodeFFprobeHexDump: %v", err)
	}
	want := []byte{0x7f, 0xfe, 0x80, 0x01, 0xfc, 0x3c, 0x7f, 0xf0, 0x22, 0xc0, 0x01, 0x38, 0x00, 0x01, 0xef, 0x83, 0x6f, 0xff, 0xff}
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded % x, want % x", got, want)
	}

	if _, err := decodeFFprobeHexDump("not a dump\n"); err == nil {
		t.Fatal("decodeFFprobeHexDump accepted a malformed line")
	}
}

func TestDTSXCandidate(t *testing.T) {
	tests := []struct {
		track AudioTrackInfo
		want  bool
	}{
		{AudioTrackInfo{Codec: "dts", Profile: "DTS-HD MA"}, true},
		{AudioTrackInfo{Codec: "dts", Profile: "DTS-HD HRA"}, true},
		{AudioTrackInfo{Codec: "dts", Profile: "DTS-HD MA + DTS:X"}, false},
		{AudioTrackInfo{Codec: "dts", Profile: "DTS-HD MA + DTS:X IMAX"}, false},
		{AudioTrackInfo{Codec: "dts", Profile: "DTS"}, false},
		{AudioTrackInfo{Codec: "dts", Profile: "DTS Express"}, false},
		{AudioTrackInfo{Codec: "truehd", Profile: "Dolby TrueHD + Dolby Atmos"}, false},
	}
	for _, tt := range tests {
		if got := dtsxCandidate(tt.track); got != tt.want {
			t.Errorf("dtsxCandidate(%+v) = %v, want %v", tt.track, got, tt.want)
		}
	}
}

// The packet sample is the selected stream's bytes as the container stores
// them, so a remux of the same stream to raw DTS starts with it.
func TestSampleAudioPacketsMatchesStreamCopy(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	media := filepath.Join(dir, "two-tracks.mkv")
	raw := filepath.Join(dir, "second-track.dts")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
		}
	}
	run("-v", "error", "-y",
		"-f", "lavfi", "-i", "sine=d=1",
		"-f", "lavfi", "-i", "sine=f=300:d=1",
		"-map", "0", "-map", "1", "-c:a:0", "aac", "-c:a:1", "dca", "-strict", "-2",
		media)
	run("-v", "error", "-y", "-i", media, "-map", "0:a:1", "-c", "copy", "-f", "dts", raw)

	sample, err := sampleAudioPackets(context.Background(), ffprobe, media, 1, 4)
	if err != nil {
		t.Fatalf("sampleAudioPackets: %v", err)
	}
	stream, err := os.ReadFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(sample) == 0 || !bytes.HasPrefix(stream, sample) {
		t.Fatalf("sample of %d bytes is not a prefix of the %d-byte stream", len(sample), len(stream))
	}
	if len(sample) >= len(stream) {
		t.Fatalf("sample read %d bytes, the whole %d-byte stream", len(sample), len(stream))
	}
}
