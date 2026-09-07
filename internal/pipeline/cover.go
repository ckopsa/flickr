package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// coverExtractTimeout bounds one cover-art extraction. The attached picture
// sits in the container header (MP4 moov, ID3, FLAC PICTURE block), so
// ffmpeg reads the first megabytes and stops after one frame — seconds, not
// minutes, but a hung storage box must not pin the handler.
const coverExtractTimeout = 2 * time.Minute

// CoverArgs is pure: input URL -> ffmpeg argv (sans binary) that writes the
// file's embedded cover art (the attached_pic "video" stream) as one JPEG.
// "0:v:0" is deliberately lowercase: ffmpeg's capital-V specifier excludes
// attached pictures, and for an audio item the attached picture is the only
// video stream there is. A file with no cover makes ffmpeg fail on the map,
// which callers treat as "none".
func CoverArgs(inputURL, outPath string) []string {
	return []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-i", inputURL,
		"-map", "0:v:0", "-frames:v", "1", "-an", "-sn",
		"-q:v", "2",
		"-f", "image2", outPath,
	}
}

// ExtractCover runs ffmpeg to write an item's embedded cover art to
// destPath, via temp file + rename so a killed run never leaves a truncated
// image to be served from cache forever. Returns an error when the file
// carries no attached picture (ffmpeg has nothing to map).
func ExtractCover(ctx context.Context, inputURL, destPath string) error {
	ctx, cancel := context.WithTimeout(ctx, coverExtractTimeout)
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	tmp := destPath + ".tmp.jpg" // image2 wants an image extension
	out, err := exec.CommandContext(ctx, "ffmpeg", CoverArgs(inputURL, tmp)...).CombinedOutput()
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ffmpeg cover extract: %v: %s", err, tail(out, 500))
	}
	if st, err := os.Stat(tmp); err != nil || st.Size() == 0 {
		os.Remove(tmp)
		return fmt.Errorf("ffmpeg cover extract produced no image")
	}
	return os.Rename(tmp, destPath)
}
