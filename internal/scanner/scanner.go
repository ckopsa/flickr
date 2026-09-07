// Package scanner is the library scan pipeline.
//
// A scan is a stream of discrete jobs — list bucket → diff against known
// etags → probe changed files → identify → batch-persist — rather than a
// monolithic full rescan. Unchanged objects (same etag) are skipped, so a
// crash or restart never re-probes the whole library; a reconciliation
// pass removes rows for deleted objects.
//
// Identification (file → title/season/episode) is deterministic and kept
// separate from enrichment; user overrides persist across scans.
package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"

	"flickr/internal/model"
	"flickr/internal/store"
)

// ProbeVersion is bumped whenever Probe extracts new information, so an
// incremental scan re-probes existing items instead of skipping them on a
// matching etag. v2: added chapter extraction. v3: fps + telecine detection.
// v4: adds subtitle streams. v5: all audio streams (audio_tracks) + external
// subtitle sidecars.
const ProbeVersion = 5

// IdentityVersion is bumped whenever Identify learns new tricks. Unlike a
// ProbeVersion bump, refreshing identity needs no ffprobe and no bandwidth —
// the scan recomputes it from the object key alone for items whose stored
// identity_version is older (and that the user hasn't overridden).
// v2: directory-aware identification (Shows/<name>/Season <n>, Movies/<name (year)>).
// v3: nothing under a category directory stays unidentified — bonus material
// becomes kind "extra" under its work, unnumbered files under a show stay
// episodes of that show, and a year-less file under Movies/ is still a movie.
// v4: the Audiobooks/, Music/ and Books/ grammars (kinds "audiobook_part",
// "track", "book", with Author and Part), checked before the video grammars
// so nothing under those directories can be read as a movie or an episode.
// v5: a track keeps its own name (TrackTitle, the filename after the ordinal)
// beside the album it belongs to.
const IdentityVersion = 5

// mediumExts is the admitted-extension table: which object keys the scan
// picks up at all, and which medium each one is. Everything else in the
// bucket (artwork, .nfo files, checksums) is ignored, as it always was.
// .pdf is deliberately absent: a PDF has no reading order to project into
// sections and needs its own renderer — a later bead admits it.
var mediumExts = map[string]string{
	// video: the original set, unchanged
	".mkv": model.MediumVideo, ".mp4": model.MediumVideo, ".m4v": model.MediumVideo,
	".avi": model.MediumVideo, ".mov": model.MediumVideo, ".webm": model.MediumVideo,
	".ts": model.MediumVideo, ".wmv": model.MediumVideo,
	// audio
	".m4b": model.MediumAudio, ".m4a": model.MediumAudio, ".mp3": model.MediumAudio,
	".flac": model.MediumAudio, ".opus": model.MediumAudio, ".ogg": model.MediumAudio,
	".aac": model.MediumAudio, ".wav": model.MediumAudio,
	// text
	".epub": model.MediumText,
}

// mediumOf reports an object key's medium from its extension, or "" when the
// key is not a media file the scan admits.
func mediumOf(objectKey string) string {
	return mediumExts[strings.ToLower(path.Ext(objectKey))]
}

// textSubtitleCodecs are the subtitle codecs ffmpeg can convert to WebVTT.
// Bitmap formats (hdmv_pgs_subtitle, dvd_subtitle) are probed and listed, but
// marked unsupported — turning pictures into text would need OCR.
var textSubtitleCodecs = map[string]bool{
	"subrip": true, "srt": true, "ass": true, "ssa": true,
	"mov_text": true, "webvtt": true, "text": true,
}

type Status struct {
	Running   bool      `json:"running"`
	Paused    bool      `json:"paused,omitempty"` // yielding to active playback
	StartedAt time.Time `json:"started_at,omitempty"`
	Total     int       `json:"total"`
	Probed    int       `json:"probed"`
	Skipped   int       `json:"skipped"`
	Removed   int64     `json:"removed"`
	Errors    int       `json:"errors"`
	LastError string    `json:"last_error,omitempty"`
	// IdentityRefreshed counts items whose identity was recomputed from the
	// key alone (no re-probe) because IdentityVersion moved.
	IdentityRefreshed int `json:"identity_refreshed,omitempty"`
	// SidecarRefreshed counts items whose external subtitle sidecars were
	// re-attached (added/removed/replaced .srt next to the video) without a
	// re-probe — the video itself was unchanged.
	SidecarRefreshed int `json:"sidecar_refreshed,omitempty"`
}

type Scanner struct {
	Client  *minio.Client
	Bucket  string
	Library *store.Library
	Workers int
	// Yield reports whether scanning should pause right now (e.g. a
	// playback session is live and needs the storage bandwidth). Probes
	// wait while it returns true — playback always outranks scanning.
	Yield func() bool

	mu     sync.Mutex
	status Status
}

func (s *Scanner) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Scanner) update(f func(*Status)) {
	s.mu.Lock()
	f(&s.status)
	s.mu.Unlock()
}

// Scan runs one incremental scan. Returns immediately with an error if a
// scan is already running.
func (s *Scanner) Scan(ctx context.Context) error {
	s.mu.Lock()
	if s.status.Running {
		s.mu.Unlock()
		return fmt.Errorf("scan already running")
	}
	s.status = Status{Running: true, StartedAt: time.Now()}
	s.mu.Unlock()
	defer s.update(func(st *Status) { st.Running = false })

	known, err := s.Library.Known()
	if err != nil {
		return err
	}

	// Listing pass: buffer media files and subtitle sidecars separately.
	// Sidecars must be matched to videos after the whole listing — lexical
	// object order means a sidecar ("Movie.en.srt") can arrive before OR
	// after its video ("Movie.mkv"), so streaming the match is not possible.
	present := map[string]bool{}
	var media []minio.ObjectInfo
	subsByDir := map[string][]sidecarFile{}
	for obj := range s.Client.ListObjects(ctx, s.Bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		// macOS AppleDouble sidecars (._foo.mkv) are resource-fork junk that
		// share real extensions but always fail probing/parsing.
		if strings.HasPrefix(path.Base(obj.Key), "._") {
			continue
		}
		if isSubtitleKey(obj.Key) {
			d := path.Dir(obj.Key)
			subsByDir[d] = append(subsByDir[d], sidecarFile{Key: obj.Key, ETag: strings.Trim(obj.ETag, `"`)})
			continue
		}
		if mediumOf(obj.Key) == "" {
			continue
		}
		present[obj.Key] = true
		media = append(media, obj)
	}

	var todo []probeJob
	var refresh []store.IdentityUpdate
	var sidecarRefresh []store.SidecarUpdate
	for _, obj := range media {
		subs := matchSidecars(obj.Key, subsByDir[path.Dir(obj.Key)])
		k, ok := known[obj.Key]
		if ok && k.ETag == strings.Trim(obj.ETag, `"`) && k.ProbeVersion == ProbeVersion {
			// Content unchanged — but if identification logic moved on since
			// this row was written, recompute identity from the key alone.
			// No ffprobe, no bandwidth; user overrides are left untouched.
			if k.IdentityVersion < IdentityVersion && !k.IdentityOverridden {
				refresh = append(refresh, store.IdentityUpdate{
					ObjectKey: obj.Key, Identity: Identify(obj.Key),
				})
			}
			// Likewise if the sidecar set changed (subtitle file added,
			// removed, or replaced): re-attach external tracks to the stored
			// media_info without re-probing the unchanged video. The store
			// applies Attach inside its read-modify-write transaction.
			if sig := sidecarSignature(subs); sig != k.SidecarSig {
				key, matched := obj.Key, subs
				sidecarRefresh = append(sidecarRefresh, store.SidecarUpdate{
					ObjectKey:  key,
					SidecarSig: sig,
					Attach:     func(info *model.MediaInfo) { attachSidecars(info, key, matched) },
				})
			}
			s.update(func(st *Status) { st.Skipped++ })
			continue
		}
		todo = append(todo, probeJob{obj: obj, sidecars: subs})
	}
	if len(refresh) > 0 {
		if err := s.Library.UpdateIdentities(refresh, IdentityVersion); err != nil {
			return err
		}
		s.update(func(st *Status) { st.IdentityRefreshed = len(refresh) })
	}
	if len(sidecarRefresh) > 0 {
		if err := s.Library.UpdateSidecars(sidecarRefresh); err != nil {
			return err
		}
		s.update(func(st *Status) { st.SidecarRefreshed = len(sidecarRefresh) })
	}
	s.update(func(st *Status) { st.Total = len(todo) })

	// Bounded-parallelism probe workers feeding a single batch writer.
	workers := s.Workers
	if workers <= 0 {
		workers = 4
	}
	jobs := make(chan probeJob)
	results := make(chan store.Item)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				for s.Yield != nil && s.Yield() {
					s.update(func(st *Status) { st.Paused = true })
					select {
					case <-ctx.Done():
						return
					case <-time.After(2 * time.Second):
					}
				}
				s.update(func(st *Status) { st.Paused = false })
				results <- s.probeOne(ctx, job)
			}
		}()
	}
	go func() {
		for _, job := range todo {
			jobs <- job
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	// Batch writes into transactions instead of row-at-a-time updates.
	const batchSize = 25
	var batch []store.Item
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := s.Library.UpsertBatch(batch); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	for item := range results {
		if item.ProbeError != "" {
			s.update(func(st *Status) { st.Errors++; st.LastError = item.ProbeError })
		}
		batch = append(batch, item)
		s.update(func(st *Status) { st.Probed++ })
		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}

	removed, err := s.Library.DeleteMissing(present)
	if err != nil {
		return err
	}
	s.update(func(st *Status) { st.Removed = removed })
	return nil
}

// ReprobeKey probes a single object on demand and persists the result —
// for newly-added probe fields or files fixed in place. It re-lists the
// video's directory for subtitle sidecars, so dropping an .srt next to a
// file and hitting reprobe attaches it without a full scan.
func (s *Scanner) ReprobeKey(ctx context.Context, objectKey string) (*store.Item, error) {
	stat, err := s.Client.StatObject(ctx, s.Bucket, objectKey, minio.StatObjectOptions{})
	if err != nil {
		return nil, err
	}
	item := s.probeOne(ctx, probeJob{
		obj:      minio.ObjectInfo{Key: objectKey, ETag: stat.ETag, Size: stat.Size},
		sidecars: s.listSidecars(ctx, objectKey),
	})
	if err := s.Library.UpsertBatch([]store.Item{item}); err != nil {
		return nil, err
	}
	return &item, nil
}

// listSidecars lists a video's directory (non-recursive) and returns the
// subtitle sidecars matching it. Listing errors degrade to "no sidecars" —
// a reprobe should not fail outright over a sidecar listing hiccup.
func (s *Scanner) listSidecars(ctx context.Context, videoKey string) []sidecarFile {
	prefix := ""
	if dir := path.Dir(videoKey); dir != "." {
		prefix = dir + "/"
	}
	var found []sidecarFile
	for obj := range s.Client.ListObjects(ctx, s.Bucket, minio.ListObjectsOptions{Prefix: prefix}) {
		if obj.Err != nil {
			return nil
		}
		if isSubtitleKey(obj.Key) {
			found = append(found, sidecarFile{Key: obj.Key, ETag: strings.Trim(obj.ETag, `"`)})
		}
	}
	return matchSidecars(videoKey, found)
}

// probeJob is one probe work unit: the media object plus its matched
// subtitle sidecars from the listing pass.
type probeJob struct {
	obj      minio.ObjectInfo
	sidecars []sidecarFile
}

// probeOne identifies and probes one object, routing the probe by medium:
// video and audio go through ffprobe (an audiobook is a container with
// streams and chapters like any film), text through the EPUB reader.
func (s *Scanner) probeOne(ctx context.Context, job probeJob) store.Item {
	obj := job.obj
	item := store.Item{
		ObjectKey:       obj.Key,
		ETag:            strings.Trim(obj.ETag, `"`),
		Size:            obj.Size,
		ProbeVersion:    ProbeVersion,
		IdentityVersion: IdentityVersion,
		SidecarSig:      sidecarSignature(job.sidecars),
	}
	ident := Identify(obj.Key)
	item.Identity = &ident

	var info *model.MediaInfo
	var err error
	if mediumOf(obj.Key) == model.MediumText {
		info, err = s.probeText(ctx, obj.Key)
	} else {
		info, err = s.probeStream(ctx, obj.Key)
	}
	if err != nil {
		item.ProbeError = fmt.Sprintf("probe %s: %v", obj.Key, err)
		return item
	}
	attachSidecars(info, obj.Key, job.sidecars)
	item.MediaInfo = info
	return item
}

// probeStream hands ffprobe a presigned URL — nothing is copied locally, the
// same way playback reads the bucket.
func (s *Scanner) probeStream(ctx context.Context, objectKey string) (*model.MediaInfo, error) {
	u, err := s.Client.PresignedGetObject(ctx, s.Bucket, objectKey, 15*time.Minute, url.Values{})
	if err != nil {
		return nil, fmt.Errorf("presign: %w", err)
	}
	return Probe(ctx, u.String(), objectKey)
}

// probeText downloads an EPUB to a temporary file and reads its package
// metadata. A zip needs random access (central directory at the end, then
// each member), which a streamed object cannot give; a temp file turns one
// sequential download into a ReaderAt and is gone before the function
// returns.
func (s *Scanner) probeText(ctx context.Context, objectKey string) (*model.MediaInfo, error) {
	f, size, err := s.fetchTemp(ctx, objectKey)
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	return ProbeEpub(f, size)
}

// ReadEpubCover downloads a text object and returns its cover image and
// media type, the way probing reads its package (a zip wants the whole
// file). ErrNoCover when the book names none. The server caches the result
// under data/covers/, so this runs once per book.
func (s *Scanner) ReadEpubCover(ctx context.Context, objectKey string) ([]byte, string, error) {
	f, size, err := s.fetchTemp(ctx, objectKey)
	if err != nil {
		return nil, "", err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	return EpubCover(f, size)
}

// fetchTemp downloads one object into a temporary file and returns it open
// with its size; the caller removes and closes it.
func (s *Scanner) fetchTemp(ctx context.Context, objectKey string) (*os.File, int64, error) {
	obj, err := s.Client.GetObject(ctx, s.Bucket, objectKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, fmt.Errorf("get: %w", err)
	}
	defer obj.Close()
	f, err := os.CreateTemp("", "flickr-epub-*")
	if err != nil {
		return nil, 0, err
	}
	size, err := io.Copy(f, obj)
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, fmt.Errorf("download: %w", err)
	}
	return f, size, nil
}

// --- ffprobe ---

type ffprobeOut struct {
	Format struct {
		Duration string `json:"duration"`
		BitRate  string `json:"bit_rate"`
	} `json:"format"`
	Streams []struct {
		CodecType     string `json:"codec_type"`
		CodecName     string `json:"codec_name"`
		Width         int    `json:"width"`
		Height        int    `json:"height"`
		Channels      int    `json:"channels"`
		ColorTransfer string `json:"color_transfer"`
		AvgFrameRate  string `json:"avg_frame_rate"`
		Disposition   struct {
			Default int `json:"default"`
		} `json:"disposition"`
		Tags struct {
			Language string `json:"language"`
			Title    string `json:"title"`
		} `json:"tags"`
	} `json:"streams"`
	Chapters []struct {
		StartTime string `json:"start_time"`
		Tags      struct {
			Title string `json:"title"`
		} `json:"tags"`
	} `json:"chapters"`
}

// Probe runs ffprobe against a URL and maps the output to MediaInfo.
func Probe(ctx context.Context, inputURL, objectKey string) (*model.MediaInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "quiet", "-print_format", "json",
		"-show_format", "-show_streams", "-show_chapters", inputURL)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	info, err := parseProbe(out, objectKey)
	if err != nil {
		return nil, err
	}
	// MPEG-2 (DVD rips) is where soft telecine lives; a short frame-level
	// probe partway into the file reads the pulldown flags.
	if info.VideoCodec == "mpeg2video" && info.DurationSeconds > 60 {
		info.Telecine = detectTelecine(ctx, inputURL, info.DurationSeconds/4)
	}
	return info, nil
}

// detectTelecine samples ~2s of frames and reports whether a significant
// share carry repeat-field (pulldown) flags.
func detectTelecine(ctx context.Context, inputURL string, at float64) bool {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "quiet", "-print_format", "json",
		"-read_intervals", fmt.Sprintf("%.0f%%+2", at),
		"-select_streams", "v:0",
		"-show_frames", "-show_entries", "frame=repeat_pict", inputURL)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	var p struct {
		Frames []struct {
			RepeatPict int `json:"repeat_pict"`
		} `json:"frames"`
	}
	if json.Unmarshal(out, &p) != nil || len(p.Frames) < 10 {
		return false
	}
	repeated := 0
	for _, f := range p.Frames {
		if f.RepeatPict > 0 {
			repeated++
		}
	}
	return float64(repeated)/float64(len(p.Frames)) > 0.2
}

// parseRate parses an ffprobe rational like "30000/1001" into a float.
func parseRate(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	if !ok {
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
	n, err1 := strconv.ParseFloat(num, 64)
	d, err2 := strconv.ParseFloat(den, 64)
	if err1 != nil || err2 != nil || d == 0 {
		return 0
	}
	return n / d
}

// parseProbe maps raw ffprobe JSON to MediaInfo. Split from Probe so the
// mapping is testable without running ffprobe.
func parseProbe(out []byte, objectKey string) (*model.MediaInfo, error) {
	var p ffprobeOut
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, err
	}

	// Audio files are probed with the same tool, but read differently: their
	// "video" stream, when there is one, is embedded cover art (an mjpeg or
	// png attached picture), not a picture to play, so an audio item always
	// ends up with VideoCodec "" and Width/Height 0.
	medium := mediumOf(objectKey)
	if medium == "" {
		medium = model.MediumVideo // reprobing a key the table no longer admits
	}
	info := &model.MediaInfo{
		Medium:    medium,
		Container: strings.TrimPrefix(strings.ToLower(path.Ext(objectKey)), "."),
	}
	info.DurationSeconds, _ = strconv.ParseFloat(p.Format.Duration, 64)
	info.BitrateBps, _ = strconv.ParseInt(p.Format.BitRate, 10, 64)
	for _, st := range p.Streams {
		switch st.CodecType {
		case "video":
			if info.VideoCodec != "" || medium == model.MediumAudio {
				continue // first video stream wins; cover art is not video
			}
			info.VideoCodec = st.CodecName
			info.Width, info.Height = st.Width, st.Height
			info.FPS = parseRate(st.AvgFrameRate)
			switch st.ColorTransfer {
			case "smpte2084":
				info.HDR = "hdr10"
			case "arib-std-b67":
				info.HDR = "hlg"
			}
		case "audio":
			// Scalars stay pinned to the first stream (the decision engine's
			// input); the full track list rides alongside for selection UIs
			// (ordinal = index among audio streams -> -map 0:a:<ordinal>).
			if info.AudioCodec == "" {
				info.AudioCodec = st.CodecName
				info.AudioChannels = st.Channels
			}
			info.AudioTracks = append(info.AudioTracks, model.AudioTrack{
				Ordinal:  len(info.AudioTracks),
				Codec:    st.CodecName,
				Language: st.Tags.Language,
				Title:    st.Tags.Title,
				Channels: st.Channels,
				Default:  st.Disposition.Default == 1,
			})
		case "subtitle":
			info.Subtitles = append(info.Subtitles, model.SubtitleTrack{
				Ordinal:   len(info.Subtitles), // index among subtitle streams only (-map 0:s:<n>)
				Codec:     st.CodecName,
				Language:  st.Tags.Language,
				Title:     st.Tags.Title,
				Supported: textSubtitleCodecs[st.CodecName],
			})
		}
	}
	switch medium {
	case model.MediumAudio:
		if info.AudioCodec == "" {
			return nil, fmt.Errorf("no audio stream found")
		}
	default:
		if info.VideoCodec == "" {
			return nil, fmt.Errorf("no video stream found")
		}
	}
	for i, ch := range p.Chapters {
		start, _ := strconv.ParseFloat(ch.StartTime, 64)
		title := ch.Tags.Title
		if title == "" {
			title = fmt.Sprintf("Chapter %d", i+1)
		}
		info.Chapters = append(info.Chapters, model.Chapter{StartSeconds: start, Title: title})
	}
	return info, nil
}

// --- identification ---

var (
	episodeRe = regexp.MustCompile(`(?i)^(.*?)[. _-]+S(\d{1,2})[. _-]?E(\d{1,3})`)
	// Greedy prefix: the LAST year-like token is the release year, so
	// titles containing a year ("Blade Runner 2049 (2017)") parse correctly.
	yearRe = regexp.MustCompile(`^(.*)[. _(-]+((19|20)\d{2})`)

	// Directory-aware patterns. Loose episode markers (3x07, a lone E05) are
	// only trusted when the path already says we're inside a show directory —
	// applied to arbitrary filenames they misfire ("day2", "1080p").
	sxxeyyRe = regexp.MustCompile(`(?i)(?:^|[ ._(-])S(\d{1,2})[ ._-]?E(\d{1,3})`)
	nxmRe    = regexp.MustCompile(`(?i)(?:^|[ ._(-])(\d{1,2})x(\d{2,3})(?:[ ._)-]|$)`)
	loneEpRe = regexp.MustCompile(`(?i)(?:^|[ ._(-])(?:Episode|Ep|E)[ ._]?(\d{1,3})(?:[ ._)-]|$)`)
	// Leading ordinal ("01 - Chen's New Chair.mkv"): a disc-order number, only
	// ever trusted inside a show directory. Bounded to 3 digits so a
	// year-titled file ("2001 A Space Odyssey") can't match.
	leadingNumRe = regexp.MustCompile(`^(\d{1,3})[ ._-]`)
	// Season directories are matched by PREFIX, because real libraries
	// decorate them ("Season 04 - Rise of the Snakes"). A DECIMAL season
	// ("Season 04.2 - Chen Mini-Movies") deliberately fails to match: it is a
	// fan-numbered interstitial block, not season 4, and folding it into
	// season 4 would collide with the real episodes 1-5 there. Unmatched, it
	// lands in season 0 — where specials belong.
	seasonDirRe = regexp.MustCompile(`(?i)^(?:Season|Series)[ ._-]*(\d{1,3})(?:[^.\d]|$)`)
)

// showCategoryDirs are directory names that mark "everything below is TV".
var showCategoryDirs = map[string]bool{"shows": true, "tv shows": true, "tv": true, "series": true}

// The audio and text category directories. Each one starts a grammar of the
// same shape — <Category>/<Author>/<Title>/<files> — where the middle
// directory is the author, the artist, or the writer: the person the work
// is filed under.
var (
	audiobookCategoryDirs = map[string]bool{"audiobooks": true, "audiobook": true, "audio books": true}
	musicCategoryDirs     = map[string]bool{"music": true, "albums": true}
	bookCategoryDirs      = map[string]bool{"books": true, "ebooks": true, "e-books": true}
)

// partNumRe reads the ordinal of a part or track from the start of its
// filename: "03 - Chapter Three", "Part 3", "Disc 2", "03". Bounded to three
// digits so a year ("1984", "2001 A Space Odyssey") is never a part number.
var partNumRe = regexp.MustCompile(`(?i)^(?:(?:part|pt|disc|cd|chapter|ch|track)[ ._-]*)?(\d{1,3})(?:[ ._)-]|$)`)

// movieCategoryDirs mark "everything below is a standalone title". A file
// under one of these belongs to a movie even when neither the filename nor
// its parent directory carries a year.
var movieCategoryDirs = map[string]bool{
	"movies": true, "movie": true, "films": true, "film": true, "documentaries": true,
}

// extrasDirs mark bonus material: featurettes, deleted scenes, bloopers. They
// belong to the work above them, never to its episode list — and never to the
// main grid. "Specials" is deliberately absent: by convention that is season 0
// of the show, i.e. real episodes.
var extrasDirs = map[string]bool{
	"extras": true, "extra": true, "featurettes": true, "featurette": true,
	"behind the scenes": true, "deleted scenes": true, "bonus": true,
	"bonus features": true, "interviews": true, "trailers": true,
	"shorts": true, "tv shorts": true, "bloopers": true,
}

// Identify deterministically maps an object key to a media identity, using
// the FULL path: a Shows/<name>/Season <n>/ layout names the show even when
// the filename alone doesn't, and Movies/<Name (Year)>/ rescues files like
// "movie.mp4". It never does I/O — same input, same answer, every scan.
//
// Everything below a category directory resolves to that category's work, so
// a file is only "unknown" when the path says nothing at all:
//   - a bonus-material directory (Extras/, Featurettes/, Deleted Scenes/)
//     yields kind "extra" belonging to the work above it — deleted scenes
//     named "S01E01 ..." must not masquerade as the real S01E01;
//   - an unnumbered file under Shows/<name>/ is still an episode of that show
//     (episode 0 = "no number in the path"), because a title-named or
//     disc-ripped file is an episode nobody numbered, not a mystery;
//   - a file under Movies/ with no year anywhere is still that movie.
//
// The audio and text categories are checked first: they name the medium
// outright, and a file under Audiobooks/ must never be read as a movie by the
// filename rules below, whatever its name looks like:
//   - Audiobooks/<Author>/<Title>/<part file> is kind "audiobook_part", the
//     part number read from the filename's leading ordinal (0 = unnumbered);
//     Audiobooks/<Author>/<Title>.m4b is the same kind with Part 0 — a
//     single-file book filed directly under its author.
//   - Music/<Artist>/<Album>/<NN Title> is kind "track": Author is the
//     artist, Title the ALBUM (the work), Part the track number and
//     TrackTitle the name after it ("Title").
//   - Books/<Author>/<Title>.epub and Books/<Author>/<Title>/<file>.epub are
//     kind "book", the title from the file or the directory respectively.
//
// A trailing "(Year)" on the title splits off into Year in all three, and
// anything under a category that fits none of these shapes still resolves to
// that category's kind with whatever the path gives — never "unknown".
func Identify(objectKey string) model.Identity {
	segs := strings.Split(objectKey, "/")
	base := strings.TrimSuffix(segs[len(segs)-1], path.Ext(segs[len(segs)-1]))
	dirs := segs[:len(segs)-1]

	if below, ok := categoryContext(dirs, audiobookCategoryDirs); ok {
		return authoredIdentity("audiobook_part", below, base, true)
	}
	if below, ok := categoryContext(dirs, musicCategoryDirs); ok {
		return authoredIdentity("track", below, base, true)
	}
	if below, ok := categoryContext(dirs, bookCategoryDirs); ok {
		return authoredIdentity("book", below, base, false)
	}

	// Show-directory context: title comes from the directory, numbers from
	// wherever they are (filename first, season directory as fallback).
	if show, dirSeason, ok := showContext(dirs); ok {
		title, year := splitTrailingYear(show)
		if hasExtrasDir(dirs) {
			// Numbers are kept when present: they say WHICH episode the bonus
			// material belongs to, which is what the UI labels it with.
			season, episode, _ := episodeNumbers(base, dirSeason)
			return model.Identity{
				Kind: "extra", Title: title, Year: year, Season: season, Episode: episode,
			}
		}
		season, episode, ok := episodeNumbers(base, dirSeason)
		if !ok {
			// Unnumbered: an episode of this show all the same, ordered by
			// whatever the season directory said (0 = unplaced).
			season, episode = dirSeason, 0
		}
		return model.Identity{
			Kind: "episode", Title: title, Year: year, Season: season, Episode: episode,
		}
	}
	// Filename-only episode pattern (title prefix + SxxEyy) works anywhere.
	if m := episodeRe.FindStringSubmatch(base); m != nil {
		season, _ := strconv.Atoi(m[2])
		episode, _ := strconv.Atoi(m[3])
		kind := "episode"
		if hasExtrasDir(dirs) {
			kind = "extra"
		}
		return model.Identity{
			Kind: kind, Title: cleanTitle(m[1]), Season: season, Episode: episode,
		}
	}
	// Movie context: the title comes from the filename, else the title
	// directory; a year is taken wherever it appears but is never required.
	if m := yearRe.FindStringSubmatch(base); m != nil {
		year, _ := strconv.Atoi(m[2])
		return movieIdentity(cleanTitle(m[1]), year, dirs)
	}
	if titleDir, ok := movieContext(dirs); ok {
		// Movies/<Name (Year)>/<uninformative file>: the title directory is
		// the better source when the filename itself yielded nothing.
		if titleDir == "" { // the file sits directly in the category directory
			return movieIdentity(cleanTitle(base), 0, dirs)
		}
		if m := yearRe.FindStringSubmatch(titleDir); m != nil {
			year, _ := strconv.Atoi(m[2])
			return movieIdentity(cleanTitle(m[1]), year, dirs)
		}
		return movieIdentity(cleanTitle(titleDir), 0, dirs)
	}
	return model.Identity{Kind: "unknown", Title: cleanTitle(base)}
}

// categoryContext finds a category directory on the path and returns the
// directories below it. Nested category directories ("Audio/Audiobooks/...",
// "Audiobooks/Audiobooks/...") are walked through the same way showContext
// tolerates "tv/Shows/...": the grammar starts at the innermost one.
func categoryContext(dirs []string, category map[string]bool) (below []string, ok bool) {
	for i, d := range dirs {
		if !category[strings.ToLower(d)] {
			continue
		}
		below = dirs[i+1:]
		for len(below) > 0 && category[strings.ToLower(below[0])] {
			below = below[1:]
		}
		return below, true
	}
	return nil, false
}

// authoredIdentity resolves the <Author>/<Title>/<file> grammar shared by the
// audio and text categories. below is the path under the category directory;
// numbered says whether the filename's leading ordinal is a part number
// (audiobook parts and tracks) or just part of a title (books).
//
// Shapes, by how many directories sit under the category:
//   - two or more: Author, then the title directory (deeper directories —
//     "Disc 1", "CD2" — are ignored; the file's own ordinal orders it);
//   - one: Author, and the file itself is the whole work (a single-file
//     audiobook, a loose track, a bare .epub);
//   - none: the file sits in the category directory; the filename is all
//     there is, and the author is unknown.
func authoredIdentity(kind string, below []string, base string, numbered bool) model.Identity {
	id := model.Identity{Kind: kind}
	switch {
	case len(below) >= 2:
		id.Author = cleanAuthor(below[0])
		id.Title, id.Year = splitTrailingYear(below[1])
		if numbered {
			id.Part = partNumber(base)
		}
		if kind == "track" {
			id.TrackTitle = trackTitle(base)
		}
	case len(below) == 1:
		id.Author = cleanAuthor(below[0])
		id.Title, id.Year = splitTrailingYear(base)
	default:
		id.Title, id.Year = splitTrailingYear(base)
	}
	if kind == "track" && id.TrackTitle == "" {
		id.TrackTitle = id.Title // a loose track: the file names the work and the track alike
	}
	return id
}

// trackTitle reads a track's own name from its filename: whatever follows the
// leading ordinal and its separator ("07 Karma Police" → "Karma Police",
// "03 - Paranoid Android" → "Paranoid Android"), the whole name when there is
// no ordinal ("Hidden Track"), and the whole name again when the ordinal is
// all there is ("Track_07" → "Track 07") — a track called nothing is worse
// than one called by its number. Tidied like an author directory, not like a
// title: "Mr. Blue Sky" and "Re-Hash" are spelled with their dot and hyphen.
func trackTitle(base string) string {
	rest := base
	if m := partNumRe.FindStringIndex(base); m != nil {
		rest = strings.TrimLeft(base[m[1]:], " ._-")
	}
	if t := cleanAuthor(rest); t != "" {
		return t
	}
	return cleanAuthor(base)
}

// partNumber reads a leading part/track ordinal from a filename; 0 when the
// name carries none ("Chapter Three", "Epilogue").
func partNumber(base string) int {
	if m := partNumRe.FindStringSubmatch(base); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// cleanAuthor tidies an author directory (and a track's name) without
// cleanTitle's dot and hyphen replacement: "J.R.R. Tolkien" and "Jean-Paul
// Sartre" are spelled that way, where a title's dots are almost always
// scene-release separators.
func cleanAuthor(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "_", " ")), " ")
}

// movieIdentity tags a resolved movie title, downgrading it to bonus material
// when the path runs through an extras directory.
func movieIdentity(title string, year int, dirs []string) model.Identity {
	kind := "movie"
	if hasExtrasDir(dirs) {
		kind = "extra"
	}
	return model.Identity{Kind: kind, Title: title, Year: year}
}

// hasExtrasDir reports whether any directory on the path marks bonus material.
func hasExtrasDir(dirs []string) bool {
	for _, d := range dirs {
		if extrasDirs[strings.ToLower(d)] {
			return true
		}
	}
	return false
}

// showContext scans the directory path for a Shows-category segment followed
// by a show-name directory, plus an optional Season/Series <n> directory.
func showContext(dirs []string) (show string, dirSeason int, ok bool) {
	for i, d := range dirs {
		if !showCategoryDirs[strings.ToLower(d)] || i+1 >= len(dirs) {
			continue
		}
		name := dirs[i+1]
		if showCategoryDirs[strings.ToLower(name)] {
			continue // nested category dirs ("tv/Shows/...") — keep walking in
		}
		if seasonDirRe.MatchString(name) {
			return "", 0, false // Shows/Season 1/... — no show name to take
		}
		for _, rest := range dirs[i+2:] {
			if m := seasonDirRe.FindStringSubmatch(rest); m != nil {
				dirSeason, _ = strconv.Atoi(m[1])
			}
		}
		return name, dirSeason, true
	}
	return "", 0, false
}

// episodeNumbers extracts season/episode from a filename, falling back to
// the season directory for formats that only carry an episode number.
func episodeNumbers(base string, dirSeason int) (season, episode int, ok bool) {
	if m := sxxeyyRe.FindStringSubmatch(base); m != nil {
		season, _ = strconv.Atoi(m[1])
		episode, _ = strconv.Atoi(m[2])
		return season, episode, true
	}
	if m := nxmRe.FindStringSubmatch(base); m != nil {
		season, _ = strconv.Atoi(m[1])
		episode, _ = strconv.Atoi(m[2])
		return season, episode, true
	}
	if m := loneEpRe.FindStringSubmatch(base); m != nil {
		episode, _ = strconv.Atoi(m[1])
		return dirSeason, episode, true
	}
	// Disc-order prefix ("01 - The Surge.mkv"): the weakest signal, so it is
	// tried last, only inside a show directory, and only when a numbered
	// season directory says which season these ordinals belong to. Without
	// that, several unrelated blocks of "01, 02, 03..." (a show's mini-movie
	// folders) would all claim season 0's episodes 1, 2, 3 and interleave;
	// unnumbered, they stay contiguous and in order under their own folder.
	if dirSeason > 0 {
		if m := leadingNumRe.FindStringSubmatch(base); m != nil {
			episode, _ = strconv.Atoi(m[1])
			return dirSeason, episode, true
		}
	}
	return 0, 0, false
}

// movieContext reports whether the path runs through a movie-category
// directory, and names the title directory below it: the deepest directory
// that is neither the category itself nor bonus material, so
// "Movies/Frozen (2013)/Extras/x.mkv" still resolves to "Frozen (2013)". An
// empty title directory means the file sits directly in the category
// directory ("Movies/Cars.mkv") and the filename is the only title there is.
func movieContext(dirs []string) (titleDir string, ok bool) {
	for i, d := range dirs {
		if !movieCategoryDirs[strings.ToLower(d)] {
			continue
		}
		for _, rest := range dirs[i+1:] {
			l := strings.ToLower(rest)
			if movieCategoryDirs[l] || extrasDirs[l] {
				continue
			}
			titleDir = rest
		}
		return titleDir, true
	}
	return "", false
}

// splitTrailingYear separates "Rick and Morty (2013)" into title and year;
// names without a year pass through with year 0.
func splitTrailingYear(name string) (string, int) {
	if m := yearRe.FindStringSubmatch(name); m != nil {
		year, _ := strconv.Atoi(m[2])
		return cleanTitle(m[1]), year
	}
	return cleanTitle(name), 0
}

func cleanTitle(s string) string {
	s = strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}
