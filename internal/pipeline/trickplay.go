package pipeline

// Trickplay sprite-sheet generation: one frame every N seconds, tiled into
// JPEG grids, for scrub-bar hover previews. Generation decodes the whole
// file, so callers are expected to run it one item at a time and yield to
// live playback — this module only knows how to run ffmpeg for one item.

import (
	"context"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

const (
	TrickplayIntervalSeconds = 10  // one preview frame per this many seconds
	TrickplayTileWidth       = 320 // px; height follows the aspect ratio
	TrickplayCols            = 10
	TrickplayRows            = 10
)

// trickplayTimeout bounds one generation run: the whole file is downloaded
// and decoded, which on slow storage can take a long while — but not forever.
const trickplayTimeout = 30 * time.Minute

// TrickplayIndex is the sidecar the client uses to map a hover time to a
// (sheet, tile) pair. TileHeight is 0 when the sheet could not be measured.
type TrickplayIndex struct {
	IntervalSeconds int `json:"interval_seconds"`
	TileWidth       int `json:"tile_width"`
	TileHeight      int `json:"tile_height"`
	Cols            int `json:"cols"`
	Rows            int `json:"rows"`
	Sheets          int `json:"sheets"`
}

// TrickplaySheets is the number of sprite sheets a file of the given
// duration needs: one frame per interval, cols*rows frames per sheet.
func TrickplaySheets(durationSeconds float64) int {
	frames := int(math.Ceil(durationSeconds / TrickplayIntervalSeconds))
	sheets := (frames + TrickplayCols*TrickplayRows - 1) / (TrickplayCols * TrickplayRows)
	return max(sheets, 1)
}

// TrickplayArgs is pure: input URL + sheet budget -> ffmpeg argv (sans
// binary) writing numbered sprite sheets to outPattern (a %d pattern).
func TrickplayArgs(inputURL string, sheets int, outPattern string) []string {
	return []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-i", inputURL,
		"-an", "-sn",
		"-vf", fmt.Sprintf("fps=1/%d,scale=%d:-2,tile=%dx%d",
			TrickplayIntervalSeconds, TrickplayTileWidth, TrickplayCols, TrickplayRows),
		"-frames:v", strconv.Itoa(sheets),
		"-start_number", "0",
		outPattern,
	}
}

// GenerateTrickplay runs ffmpeg to produce the sprite sheets plus index.json
// for one item, atomically: everything is written to a temp directory that
// is renamed into place only when complete — a killed run never leaves a
// half-written index behind.
func GenerateTrickplay(ctx context.Context, inputURL string, durationSeconds float64, destDir string) error {
	ctx, cancel := context.WithTimeout(ctx, trickplayTimeout)
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(destDir), filepath.Base(destDir)+".tmp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // no-op once renamed into place
	os.Chmod(tmp, 0o755)    // MkdirTemp defaults to 0700

	sheets := TrickplaySheets(durationSeconds)
	args := TrickplayArgs(inputURL, sheets, filepath.Join(tmp, "sheet%d.jpg"))
	out, err := runFFmpeg(ctx, args)
	if err != nil {
		return fmt.Errorf("ffmpeg trickplay: %v: %s", err, tail(out, 500))
	}

	// Short or mis-probed files may yield fewer sheets than budgeted; the
	// index records what actually exists.
	actual := 0
	for ; actual < sheets; actual++ {
		if _, err := os.Stat(filepath.Join(tmp, fmt.Sprintf("sheet%d.jpg", actual))); err != nil {
			break
		}
	}
	if actual == 0 {
		return fmt.Errorf("ffmpeg trickplay produced no sheets: %s", tail(out, 500))
	}

	idx := TrickplayIndex{
		IntervalSeconds: TrickplayIntervalSeconds,
		TileWidth:       TrickplayTileWidth,
		TileHeight:      sheetTileHeight(filepath.Join(tmp, "sheet0.jpg")),
		Cols:            TrickplayCols,
		Rows:            TrickplayRows,
		Sheets:          actual,
	}
	b, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, "index.json"), b, 0o644); err != nil {
		return err
	}
	if err := os.RemoveAll(destDir); err != nil {
		return err
	}
	return os.Rename(tmp, destDir)
}

// HasTrickplay reports whether a complete trickplay set exists at dir.
// Completeness == index present, because writes are atomic.
func HasTrickplay(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "index.json"))
	return err == nil
}

// sheetTileHeight measures one tile's height from a finished sheet (sheet
// height / rows); 0 when the sheet cannot be decoded.
func sheetTileHeight(sheetPath string) int {
	f, err := os.Open(sheetPath)
	if err != nil {
		return 0
	}
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		return 0
	}
	return cfg.Height / TrickplayRows
}

func runFFmpeg(ctx context.Context, args []string) ([]byte, error) {
	return exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput()
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
