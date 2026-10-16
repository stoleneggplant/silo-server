package scanner

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/mediaprobe"
	"github.com/Silo-Server/silo-server/internal/processmetrics"
)

const (
	// dtsxSamplePackets is how many packets from the start of a DTS track
	// the DTS:X check reads. Every DTS:X frame carries the extension, so a
	// short run decides it.
	dtsxSamplePackets = 32
	// dtsxSampleOutputLimit bounds FFprobe's hex dump of those packets: 32
	// of the largest Master Audio frames dump to about 4 MB.
	dtsxSampleOutputLimit = 16 << 20
	dtsxSampleTimeout     = time.Minute
)

// applyDTSXProfiles marks DTS-HD tracks that carry DTS:X when FFprobe did
// not report it. FFmpeg recognizes only some DTS:X streams, so each DTS-HD
// track whose profile does not already name DTS:X has its first packets
// read and checked for the extension (see mediaprobe.DetectDTSX). A check
// that fails leaves the profile as FFprobe reported it.
func applyDTSXProfiles(ctx context.Context, ffprobePath, filePath string, probe *ProbeData) {
	if probe == nil {
		return
	}
	for i := range probe.AudioTracks {
		track := &probe.AudioTracks[i]
		if !dtsxCandidate(*track) {
			continue
		}
		sample, err := sampleAudioPackets(ctx, ffprobePath, filePath, i, dtsxSamplePackets)
		if err != nil {
			slog.DebugContext(ctx, "scanner: DTS:X check skipped",
				"component", "scanner", "path", filePath, "audio_track", i, "error", err)
			continue
		}
		track.Profile = mediaprobe.DetectDTSX(sample).WithProfile(track.Profile)
	}
}

// dtsxCandidate reports whether a probed audio track is a DTS-HD carrier
// (Master Audio or High Resolution Audio) not already reported as DTS:X.
// Core-only DTS and DTS Express cannot carry DTS:X.
func dtsxCandidate(track AudioTrackInfo) bool {
	if !strings.EqualFold(track.Codec, "dts") {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(track.Profile)) {
	case "DTS-HD MA", "DTS-HD HRA":
		return true
	default:
		return false
	}
}

// sampleAudioPackets returns the payload of the first count packets of the
// audioIndex-th audio stream, concatenated in stream order.
func sampleAudioPackets(ctx context.Context, ffprobePath, filePath string, audioIndex, count int) ([]byte, error) {
	sampleCtx, cancel := context.WithTimeout(ctx, dtsxSampleTimeout)
	defer cancel()
	cmd := exec.CommandContext(sampleCtx, ffprobePath,
		"-v", "error",
		"-select_streams", "a:"+strconv.Itoa(audioIndex),
		"-read_intervals", "%+#"+strconv.Itoa(count),
		"-show_entries", "packet=data",
		"-show_data",
		"-print_format", "json",
		filePath,
	)
	killProcessGroupOnCancel(cmd)
	cmd.WaitDelay = defaultProbeLimits.waitDelay
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("opening ffprobe packet output: %w", err)
	}
	if err := cmd.Start(); err != nil {
		processmetrics.Record(processmetrics.Probe, nil, err, sampleCtx.Err())
		return nil, fmt.Errorf("starting ffprobe packet sample: %w", err)
	}
	stopClose := context.AfterFunc(sampleCtx, func() { _ = stdout.Close() })
	output, readErr := io.ReadAll(io.LimitReader(stdout, dtsxSampleOutputLimit+1))
	stopClose()
	if len(output) > dtsxSampleOutputLimit {
		_ = stdout.Close()
	}
	waitErr := cmd.Wait()
	processmetrics.Record(processmetrics.Probe, cmd.ProcessState, waitErr, sampleCtx.Err())
	switch {
	case probeTimedOut(ctx, sampleCtx):
		return nil, fmt.Errorf("%w: packet sample of %s did not finish within %s", errProbeTimeout, filePath, dtsxSampleTimeout)
	case len(output) > dtsxSampleOutputLimit:
		return nil, errors.New("ffprobe packet sample exceeded its output limit")
	case readErr != nil:
		return nil, fmt.Errorf("reading ffprobe packet sample: %w", readErr)
	case waitErr != nil:
		return nil, fmt.Errorf("ffprobe packet sample failed: %w", waitErr)
	}

	var parsed struct {
		Packets []struct {
			Data string `json:"data"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(output, &parsed); err != nil {
		return nil, fmt.Errorf("parsing ffprobe packet sample: %w", err)
	}
	var sample []byte
	for _, packet := range parsed.Packets {
		data, err := decodeFFprobeHexDump(packet.Data)
		if err != nil {
			return nil, err
		}
		sample = append(sample, data...)
	}
	return sample, nil
}

// decodeFFprobeHexDump decodes FFprobe's -show_data dump. Each line is an
// eight-digit offset and ": ", then up to 16 bytes as hex in groups of two
// bytes, padded to 41 columns, then the same bytes as ASCII:
//
//	00000000: 7ffe 8001 fc3c 7ff0 22c0 0138 0001 ef83  .....<..\"..8....
func decodeFFprobeHexDump(dump string) ([]byte, error) {
	const offsetWidth, hexWidth = 10, 41
	var out []byte
	lines := bufio.NewScanner(strings.NewReader(dump))
	for lines.Scan() {
		line := lines.Text()
		if line == "" {
			continue
		}
		if len(line) < offsetWidth || line[offsetWidth-2:offsetWidth] != ": " {
			return nil, fmt.Errorf("unexpected ffprobe data line %q", line)
		}
		hexPart := strings.ReplaceAll(line[offsetWidth:min(len(line), offsetWidth+hexWidth)], " ", "")
		decoded, err := hex.DecodeString(hexPart)
		if err != nil {
			return nil, fmt.Errorf("decoding ffprobe data line: %w", err)
		}
		out = append(out, decoded...)
	}
	return out, lines.Err()
}
