package mediaprobe

import (
	"encoding/binary"
	"testing"
)

type bitWriter struct {
	data []byte
	bits int
}

func (w *bitWriter) write(n int, v uint64) {
	for i := n - 1; i >= 0; i-- {
		if w.bits%8 == 0 {
			w.data = append(w.data, 0)
		}
		if v>>uint(i)&1 == 1 {
			w.data[len(w.data)-1] |= 0x80 >> uint(w.bits%8)
		}
		w.bits++
	}
}

func filler(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 0x55
	}
	return b
}

// coreFrame is a DTS core frame header padded to size bytes.
func coreFrame(size int) []byte {
	w := bitWriter{}
	w.write(32, dtsSyncCore)
	w.write(1+5+1+7, 0)
	w.write(14, uint64(size-1))
	return append(w.data, filler(size-len(w.data))...)
}

// exssFrame is an extension substream holding asset and then trailer.
func exssFrame(asset, trailer []byte) []byte {
	const headerSize = 16
	size := headerSize + len(asset) + len(trailer)
	w := bitWriter{}
	w.write(32, dtsSyncEXSS)
	w.write(8, 0) // user defined
	w.write(2, 0) // exss index
	w.write(1, 0) // narrow header
	w.write(8, headerSize-1)
	w.write(16, uint64(size-1))
	w.write(1, 1) // static fields
	w.write(2, 0) // reference clock
	w.write(3, 0) // frame duration
	w.write(1, 0) // no timecode
	w.write(3, 0) // one presentation
	w.write(3, 0) // one asset
	w.write(1, 1) // active substream mask
	w.write(8, 0) // active asset mask
	w.write(1, 0) // no mix metadata
	w.write(16, uint64(len(asset)-1))
	header := append(w.data, make([]byte, headerSize-len(w.data))...)
	return append(append(header, asset...), trailer...)
}

// xllAsset is an XLL frame of size bytes with marker at offset (0: none).
func xllAsset(size, offset int, marker uint32) []byte {
	w := bitWriter{}
	w.write(32, dtsSyncXLL)
	w.write(4, 0)
	w.write(8, 16-1) // header size
	w.write(5, 16-1) // frame size field bits
	w.write(16, uint64(size-1))
	frame := append(w.data, filler(size-len(w.data))...)
	if offset > 0 {
		binary.BigEndian.PutUint32(frame[offset:], marker)
	}
	return frame
}

func lossyTrailer(marker uint32) []byte {
	trailer := make([]byte, 6, 40)
	trailer = binary.BigEndian.AppendUint32(trailer, marker)
	return append(trailer, filler(30)...)
}

func stream(frames int, frame func(i int) []byte) []byte {
	var out []byte
	for i := range frames {
		out = append(out, coreFrame(256)...)
		out = append(out, frame(i)...)
	}
	return out
}

func TestDetectDTSX(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "Master Audio with the standard marker",
			data: stream(8, func(int) []byte { return exssFrame(xllAsset(512, 480, dtsSyncX), nil) }),
			want: "DTS-HD MA + DTS:X",
		},
		{
			name: "Master Audio with an IMAX marker FFmpeg does not know",
			data: stream(8, func(int) []byte { return exssFrame(xllAsset(512, 480, dtsSyncXIMAXD3), nil) }),
			want: "DTS-HD MA + DTS:X IMAX",
		},
		{
			name: "lossy carrier with the marker after the asset",
			data: stream(8, func(int) []byte { return exssFrame(filler(300), lossyTrailer(dtsSyncX)) }),
			want: "DTS-HD HRA + DTS:X",
		},
		{
			name: "lossy carrier with an IMAX marker",
			data: stream(8, func(int) []byte { return exssFrame(filler(300), lossyTrailer(dtsSyncXIMAXD1)) }),
			want: "DTS-HD HRA + DTS:X IMAX",
		},
		{
			name: "plain Master Audio",
			data: stream(8, func(int) []byte { return exssFrame(xllAsset(512, 0, 0), nil) }),
		},
		{
			name: "marker misaligned inside the XLL frame",
			data: stream(8, func(int) []byte { return exssFrame(xllAsset(512, 482, dtsSyncX), nil) }),
		},
		{
			name: "marker past the end of the XLL frame",
			data: stream(8, func(int) []byte { return exssFrame(xllAsset(512, 0, 0), lossyTrailer(dtsSyncX)) }),
		},
		{
			name: "marker in one frame only",
			data: stream(8, func(i int) []byte {
				if i == 3 {
					return exssFrame(xllAsset(512, 480, dtsSyncX), nil)
				}
				return exssFrame(xllAsset(512, 0, 0), nil)
			}),
		},
		{
			name: "marker in a minority of frames",
			data: stream(8, func(i int) []byte {
				if i < 3 {
					return exssFrame(xllAsset(512, 480, dtsSyncX), nil)
				}
				return exssFrame(xllAsset(512, 0, 0), nil)
			}),
		},
		{
			name: "core only",
			data: stream(8, func(int) []byte { return nil }),
		},
		{
			name: "truncated last frame",
			data: stream(4, func(int) []byte { return exssFrame(xllAsset(512, 480, dtsSyncX), nil) })[:2900],
			want: "DTS-HD MA + DTS:X",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectDTSX(tt.data)
			base := "DTS-HD HRA"
			if got.Lossless {
				base = "DTS-HD MA"
			}
			if got.Variant == DTSXNone {
				base = ""
			}
			if got := got.WithProfile(base); got != tt.want {
				t.Fatalf("DetectDTSX profile = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectDTSXIgnoresGarbage(t *testing.T) {
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i * 7)
	}
	// An EXSS sync word whose header claims more bytes than remain.
	binary.BigEndian.PutUint32(data[100:], dtsSyncEXSS)
	if got := DetectDTSX(data); got.Variant != DTSXNone {
		t.Fatalf("DetectDTSX = %+v, want none", got)
	}
}
