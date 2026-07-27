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
	"net/url"
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
const IdentityVersion = 2

var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true,
	".mov": true, ".webm": true, ".ts": true, ".wmv": true,
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

	// Listing pass: buffer videos and subtitle sidecars separately. Sidecars
	// must be matched to videos after the whole listing — lexical object
	// order means a sidecar ("Movie.en.srt") can arrive before OR after its
	// video ("Movie.mkv"), so streaming the match is not possible.
	present := map[string]bool{}
	var videos []minio.ObjectInfo
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
		if !videoExts[strings.ToLower(path.Ext(obj.Key))] {
			continue
		}
		present[obj.Key] = true
		videos = append(videos, obj)
	}

	var todo []probeJob
	var refresh []store.IdentityUpdate
	var sidecarRefresh []store.SidecarUpdate
	for _, obj := range videos {
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

// probeJob is one probe work unit: the video object plus its matched
// subtitle sidecars from the listing pass.
type probeJob struct {
	obj      minio.ObjectInfo
	sidecars []sidecarFile
}

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

	u, err := s.Client.PresignedGetObject(ctx, s.Bucket, obj.Key, 15*time.Minute, url.Values{})
	if err != nil {
		item.ProbeError = fmt.Sprintf("presign %s: %v", obj.Key, err)
		return item
	}
	info, err := Probe(ctx, u.String(), obj.Key)
	if err != nil {
		item.ProbeError = fmt.Sprintf("probe %s: %v", obj.Key, err)
		return item
	}
	attachSidecars(info, obj.Key, job.sidecars)
	item.MediaInfo = info
	return item
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

	info := &model.MediaInfo{
		Container: strings.TrimPrefix(strings.ToLower(path.Ext(objectKey)), "."),
	}
	info.DurationSeconds, _ = strconv.ParseFloat(p.Format.Duration, 64)
	info.BitrateBps, _ = strconv.ParseInt(p.Format.BitRate, 10, 64)
	for _, st := range p.Streams {
		switch st.CodecType {
		case "video":
			if info.VideoCodec != "" {
				continue // first video stream wins
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
	if info.VideoCodec == "" {
		return nil, fmt.Errorf("no video stream found")
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
	sxxeyyRe    = regexp.MustCompile(`(?i)(?:^|[ ._(-])S(\d{1,2})[ ._-]?E(\d{1,3})`)
	nxmRe       = regexp.MustCompile(`(?i)(?:^|[ ._(-])(\d{1,2})x(\d{2,3})(?:[ ._)-]|$)`)
	loneEpRe    = regexp.MustCompile(`(?i)(?:^|[ ._(-])(?:Episode|Ep|E)[ ._]?(\d{1,3})(?:[ ._)-]|$)`)
	seasonDirRe = regexp.MustCompile(`(?i)^(?:Season|Series)[ ._-]*(\d{1,3})$`)
)

// showCategoryDirs are directory names that mark "everything below is TV".
var showCategoryDirs = map[string]bool{"shows": true, "tv shows": true, "tv": true, "series": true}

// Identify deterministically maps an object key to a media identity, using
// the FULL path: a Shows/<name>/Season <n>/ layout names the show even when
// the filename alone doesn't, and Movies/<Name (Year)>/ rescues files like
// "movie.mp4". It never does I/O — same input, same answer, every scan.
func Identify(objectKey string) model.Identity {
	segs := strings.Split(objectKey, "/")
	base := strings.TrimSuffix(segs[len(segs)-1], path.Ext(segs[len(segs)-1]))
	dirs := segs[:len(segs)-1]

	// Show-directory context: title comes from the directory, numbers from
	// wherever they are (filename first, season directory as fallback).
	if show, dirSeason, ok := showContext(dirs); ok {
		if season, episode, ok := episodeNumbers(base, dirSeason); ok {
			title, year := splitTrailingYear(show)
			return model.Identity{
				Kind: "episode", Title: title, Year: year, Season: season, Episode: episode,
			}
		}
	}
	// Filename-only episode pattern (title prefix + SxxEyy) works anywhere.
	if m := episodeRe.FindStringSubmatch(base); m != nil {
		season, _ := strconv.Atoi(m[2])
		episode, _ := strconv.Atoi(m[3])
		return model.Identity{
			Kind: "episode", Title: cleanTitle(m[1]), Season: season, Episode: episode,
		}
	}
	if m := yearRe.FindStringSubmatch(base); m != nil {
		year, _ := strconv.Atoi(m[2])
		return model.Identity{Kind: "movie", Title: cleanTitle(m[1]), Year: year}
	}
	// Movies/<Name (Year)>/<uninformative file>: the parent directory is the
	// better source when the filename itself yielded nothing.
	if parent, ok := moviesParent(dirs); ok {
		if m := yearRe.FindStringSubmatch(parent); m != nil {
			year, _ := strconv.Atoi(m[2])
			return model.Identity{Kind: "movie", Title: cleanTitle(m[1]), Year: year}
		}
	}
	return model.Identity{Kind: "unknown", Title: cleanTitle(base)}
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
	return 0, 0, false
}

// moviesParent returns the file's parent directory when the path contains a
// Movies-category segment above it.
func moviesParent(dirs []string) (string, bool) {
	for i, d := range dirs {
		if strings.EqualFold(d, "movies") && i < len(dirs)-1 {
			return dirs[len(dirs)-1], true
		}
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
