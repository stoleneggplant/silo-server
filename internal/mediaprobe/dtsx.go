package mediaprobe

import (
	"encoding/binary"
	"math/bits"
	"strings"
)

// DTS:X detection from the DTS bitstream.
//
// A DTS:X stream is a DTS-HD carrier whose extension substream (EXSS) holds
// one more component: the DTS:X extension, which opens with a 32-bit marker.
// The markers and where a carrier puts them follow the harletty-bridge dca
// decoder (github.com/harletty/harletty-bridge, Apache-2.0):
//
//   - DTS-HD Master Audio: the extension ends the XLL lossless frame, at a
//     32-bit-aligned offset from the XLL frame's start.
//   - DTS-HD High Resolution Audio (a lossy carrier, core + XXCH): the
//     extension follows the EXSS audio asset, within a few bytes of its end.
//
// 0x02000850 marks standard DTS:X. 0xF14000D0, D1, D3 and D4 mark the IMAX
// variants. FFmpeg flags only 0x02000850 and 0xF14000D0, and only inside
// XLL, so a stream it reports as plain "DTS-HD MA" or "DTS-HD HRA" can still
// carry DTS:X.

const (
	dtsSyncCore = 0x7FFE8001
	dtsSyncEXSS = 0x64582025
	dtsSyncXLL  = 0x41A29547

	dtsSyncX        = 0x02000850
	dtsSyncXIMAXD0  = 0xF14000D0
	dtsSyncXIMAXD1  = 0xF14000D1
	dtsSyncXIMAXD3  = 0xF14000D3
	dtsSyncXIMAXD4  = 0xF14000D4
	dtsLossyXWindow = 32
)

// DTSX is the DTS:X variant a stream carries.
type DTSX int

const (
	// DTSXNone means no DTS:X extension was found.
	DTSXNone DTSX = iota
	// DTSXStandard is standard DTS:X.
	DTSXStandard
	// DTSXIMAX is DTS:X IMAX Enhanced.
	DTSXIMAX
)

// DTSXDetection is what DetectDTSX found in a stream sample.
type DTSXDetection struct {
	Variant DTSX
	// Lossless reports a DTS-HD Master Audio carrier (XLL). It is meaningful
	// only when Variant is not DTSXNone.
	Lossless bool
}

// WithProfile appends the DTS:X variant to an FFprobe DTS-HD profile the
// way FFmpeg names it ("DTS-HD MA" becomes "DTS-HD MA + DTS:X" or
// "DTS-HD MA + DTS:X IMAX"). It returns profile unchanged when Variant is
// DTSXNone. jellyfin-ffmpeg reports the same names for the streams it
// recognizes, so readers of the profile treat both alike.
func (d DTSXDetection) WithProfile(profile string) string {
	switch d.Variant {
	case DTSXStandard:
		return profile + " + DTS:X"
	case DTSXIMAX:
		return profile + " + DTS:X IMAX"
	default:
		return profile
	}
}

// ProfileNamesDTSX reports whether an FFprobe audio profile already names
// DTS:X.
func ProfileNamesDTSX(profile string) bool {
	return strings.Contains(strings.ToLower(profile), "dts:x")
}

// dtsxMarker classifies a 32-bit word as a DTS:X extension marker.
func dtsxMarker(word uint32) DTSX {
	switch word {
	case dtsSyncX:
		return DTSXStandard
	case dtsSyncXIMAXD0, dtsSyncXIMAXD1, dtsSyncXIMAXD3, dtsSyncXIMAXD4:
		return DTSXIMAX
	default:
		return DTSXNone
	}
}

// DetectDTSX scans a sample of a DTS elementary stream (frames concatenated
// in stream order, as a demuxer delivers them) for a DTS:X extension.
//
// Every frame of a DTS:X stream carries the extension, so a stream counts as
// DTS:X only when at least two frames, and at least half of the extension
// substreams that parsed, carry a marker. A 32-bit marker can occur by chance
// in coded audio; requiring it across frames keeps that from labeling a plain
// DTS-HD stream.
func DetectDTSX(sample []byte) DTSXDetection {
	var parsed, standard, imax, lossless int
	for pos := 0; pos+4 <= len(sample); {
		switch binary.BigEndian.Uint32(sample[pos:]) {
		case dtsSyncCore:
			if size, ok := dtsCoreFrameSize(sample[pos:]); ok {
				pos += size
				continue
			}
		case dtsSyncEXSS:
			if frame, ok := parseDTSEXSS(sample[pos:]); ok {
				parsed++
				variant, isLossless := frame.dtsx(sample[pos : pos+frame.size])
				switch variant {
				case DTSXStandard:
					standard++
				case DTSXIMAX:
					imax++
				}
				if variant != DTSXNone && isLossless {
					lossless++
				}
				pos += frame.size
				continue
			}
		}
		pos++
	}

	found := standard + imax
	if found < 2 || found*2 < parsed {
		return DTSXDetection{}
	}
	result := DTSXDetection{Variant: DTSXStandard, Lossless: lossless*2 >= found}
	if imax > standard {
		result.Variant = DTSXIMAX
	}
	return result
}

// dtsCoreFrameSize reads the frame size from a core frame header at the start
// of data: sync (32), frame type (1), deficit samples (5), CRC flag (1),
// blocks (7), then the frame size minus one (14). The smallest valid size is
// 96 bytes.
func dtsCoreFrameSize(data []byte) (int, bool) {
	r := dtsBitReader{data: data}
	r.skip(32 + 1 + 5 + 1 + 7)
	size := int(r.read(14)) + 1
	if r.err || size < 96 {
		return 0, false
	}
	return size, true
}

// dtsEXSS is the part of an extension substream header DTS:X detection needs.
type dtsEXSS struct {
	size       int
	assetStart int
	assetEnd   int
}

// parseDTSEXSS reads an extension substream header at the start of data, up
// to its audio asset's size (ETSI TS 102 114 7.5, the same fields the dca
// decoder reads). Only a single presentation with a single asset is
// accepted, as the dca decoder does; anything else reports false and is left
// out of the count.
func parseDTSEXSS(data []byte) (dtsEXSS, bool) {
	r := dtsBitReader{data: data}
	r.skip(32) // sync
	r.skip(8)  // user defined
	exssIndex := int(r.read(2))
	wide := int(r.read(1))
	headerSize := int(r.read(8+4*wide)) + 1
	sizeBits := 16 + 4*wide
	exssSize := int(r.read(sizeBits)) + 1

	if r.read(1) == 1 { // static fields present
		r.skip(2) // reference clock
		r.skip(3) // frame duration
		if r.read(1) == 1 {
			r.skip(36) // timecode
		}
		presentations := int(r.read(3)) + 1
		assets := int(r.read(3)) + 1
		if presentations != 1 || assets != 1 {
			return dtsEXSS{}, false
		}
		activeMask := r.read(exssIndex + 1)
		r.skip(bits.OnesCount32(activeMask) * 8)
		if r.read(1) == 1 { // mix metadata enabled
			r.skip(2) // adjustment level
			maskBits := (int(r.read(2)) + 1) << 2
			configs := int(r.read(2)) + 1
			r.skip(configs * maskBits)
		}
	}
	assetSize := int(r.read(sizeBits)) + 1

	if r.err || exssSize > len(data) || headerSize > exssSize || headerSize+assetSize > exssSize {
		return dtsEXSS{}, false
	}
	return dtsEXSS{size: exssSize, assetStart: headerSize, assetEnd: headerSize + assetSize}, true
}

// dtsx reports the DTS:X marker in one extension substream (exss holds exactly
// the substream), and whether its carrier is lossless. A Master Audio asset
// carries an XLL frame, whose sync word opens it; the marker then sits at a
// 32-bit-aligned offset inside that frame. Without XLL the asset is a lossy
// carrier, and the marker follows the asset.
func (f dtsEXSS) dtsx(exss []byte) (DTSX, bool) {
	asset := exss[f.assetStart:f.assetEnd]
	for i := 0; i+4 <= len(asset); i++ {
		if binary.BigEndian.Uint32(asset[i:]) != dtsSyncXLL {
			continue
		}
		xll := asset[i:]
		r := dtsBitReader{data: xll}
		r.skip(32) // sync
		r.skip(4)  // version
		headerSize := int(r.read(8)) + 1
		frameSize := int(r.read(int(r.read(5))+1)) + 1
		if r.err || headerSize >= frameSize {
			continue
		}
		// A peak-bit-rate-smoothed frame can run past this substream; scan
		// what is here.
		end := min(frameSize, len(xll))
		for at := (headerSize + 3) &^ 3; at+4 <= end; at += 4 {
			if v := dtsxMarker(binary.BigEndian.Uint32(xll[at:])); v != DTSXNone {
				return v, true
			}
		}
		return DTSXNone, true
	}

	window := exss[f.assetEnd:min(len(exss), f.assetEnd+dtsLossyXWindow)]
	for i := 0; i+4 <= len(window); i++ {
		if v := dtsxMarker(binary.BigEndian.Uint32(window[i:])); v != DTSXNone {
			return v, false
		}
	}
	return DTSXNone, false
}

// dtsBitReader reads big-endian bit fields. Reading past the end sets err and
// returns zeros, so a parse checks err once at the end.
type dtsBitReader struct {
	data []byte
	pos  int
	err  bool
}

func (r *dtsBitReader) read(n int) uint32 {
	var v uint32
	for range n {
		byteIndex := r.pos >> 3
		if byteIndex >= len(r.data) {
			r.err = true
			return 0
		}
		v = v<<1 | uint32(r.data[byteIndex]>>(7-uint(r.pos&7)))&1
		r.pos++
	}
	return v
}

func (r *dtsBitReader) skip(n int) {
	r.pos += n
	if r.pos > len(r.data)*8 {
		r.err = true
	}
}
