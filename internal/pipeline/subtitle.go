package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// subtitleExtractTimeout bounds one extraction run. ffmpeg must read the
// whole file to collect every cue, so remuxed-from-storage inputs can take
// a while — but a hung storage box must not pin the handler forever.
// Extraction demuxes the whole container over HTTP, so large files on slow
// storage need headroom (a 1.5GB file at ~15MB/s is ~100s). The result is
// cached, so the cost is paid once per track.
const subtitleExtractTimeout = 5 * time.Minute

// SubtitleArgs is pure: input URL + subtitle ordinal -> ffmpeg argv (sans
// binary) that extracts that one text subtitle stream as WebVTT.
func SubtitleArgs(inputURL string, ordinal int, outPath string) []string {
	return []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-i", inputURL,
		"-map", fmt.Sprintf("0:s:%d", ordinal),
		"-f", "webvtt", outPath,
	}
}

// ExtractSubtitle runs ffmpeg to convert one subtitle stream to WebVTT at
// destPath. Written via a temp file + rename so a killed extraction never
// leaves a half-written file that would then be served from cache forever.
func ExtractSubtitle(ctx context.Context, inputURL string, ordinal int, destPath string) error {
	ctx, cancel := context.WithTimeout(ctx, subtitleExtractTimeout)
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	tmp := destPath + ".tmp"
	cmd := exec.CommandContext(ctx, "ffmpeg", SubtitleArgs(inputURL, ordinal, tmp)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(tmp)
		msg := string(out)
		if len(msg) > 500 {
			msg = msg[len(msg)-500:]
		}
		return fmt.Errorf("ffmpeg subtitle extract: %v: %s", err, msg)
	}
	return os.Rename(tmp, destPath)
}
