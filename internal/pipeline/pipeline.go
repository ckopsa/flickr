// Package pipeline is the media pipeline: the ONLY package that knows
// FFmpeg exists.
//
// An abstract job description ("transcode this to h264/aac, seek to 750s,
// segment for HLS") is turned into an FFmpeg invocation here and nowhere
// else. BuildArgs is pure and unit-testable; Session owns the subprocess.
package pipeline

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"flickr/internal/model"
)

const SegmentSeconds = 4

type Job struct {
	InputURL    string
	Target      model.TranscodeTarget
	OutputDir   string
	SeekSeconds float64
	HW          *HWAccel // nil = software encoding
}

// hwEncoder returns the hardware encoder this job will use for its video
// target, or "" when encoding in software (or copying).
func (j Job) hwEncoder() string {
	if j.HW == nil || j.Target.VideoCodec == "" {
		return ""
	}
	return j.HW.Encoders[j.Target.VideoCodec]
}

// SeekAudioPrerollSeconds is how far before a seek target the input is
// opened when audio is re-encoded. Sync-framed audio codecs (TrueHD, DTS)
// cannot start decoding mid-stream: without pre-roll the first seconds of
// audio are silently dropped, and players react badly to the hole (missing
// audio track, garbled pitch). The pre-roll is decoded and discarded by an
// output-side trim so both streams start exactly at the target.
const SeekAudioPrerollSeconds = 10.0

// seekSplit returns the input-side and output-side seek for a job.
// Output-side trimming needs decoded frames, so it only applies when both
// streams are re-encoded; callers upgrade copy-video jobs before seeking.
func (j Job) seekSplit() (in, out float64) {
	if j.SeekSeconds <= 0 {
		return 0, 0
	}
	if j.Target.AudioCodec != "" && j.Target.VideoCodec != "" {
		p := min(SeekAudioPrerollSeconds, j.SeekSeconds)
		return j.SeekSeconds - p, p
	}
	return j.SeekSeconds, 0
}

// BuildArgs is a pure function: job description -> ffmpeg argv (sans binary).
func BuildArgs(j Job) []string {
	t := j.Target
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "warning"}
	if hwEnc := j.hwEncoder(); hwEnc != "" {
		args = append(args, hwInitArgs(j.HW.Kind, j.HW.Device)...)
	}
	seekIn, seekOut := j.seekSplit()
	if seekIn > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", seekIn))
	}
	args = append(args, "-i", j.InputURL)
	if seekOut > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", seekOut))
	}

	if t.VideoCodec == "" {
		args = append(args, "-c:v", "copy")
	} else {
		var filters []string
		if t.Detelecine {
			// Soft telecine: the decoder already emits the original
			// progressive film frames (no materialized duplicates!) — just
			// re-time them onto a uniform grid at the film rate. Never use
			// fieldmatch/decimate here: with no duplicates to drop, decimate
			// discards real frames and desyncs video against audio.
			filters = append(filters, fmt.Sprintf("fps=%.6f", t.FPS))
		}
		if t.Tonemap {
			filters = append(filters,
				"zscale=t=linear:npl=100,tonemap=hable,zscale=p=bt709:t=bt709:m=bt709,format=yuv420p")
		}
		if t.Height > 0 {
			filters = append(filters, fmt.Sprintf("scale=-2:%d", t.Height))
		}
		enc := ResolveEncoder(j.HW, t.VideoCodec)
		// Decode and filters stay in software; hardware encoders get frames
		// uploaded (vaapi) or converted (qsv) at the end of the chain.
		if j.hwEncoder() != "" {
			switch j.HW.Kind {
			case "vaapi":
				filters = append(filters, "format=nv12,hwupload")
			case "qsv", "rkmpp":
				filters = append(filters, "format=nv12")
			}
		}
		if len(filters) > 0 {
			args = append(args, "-vf", joinFilters(filters))
		}
		args = append(args, "-c:v", enc)
		args = append(args, presetArgs(enc)...)
		if t.VideoBitrateBps > 0 {
			args = append(args, "-b:v", strconv.FormatInt(t.VideoBitrateBps, 10))
		}
		// Keyframe cadence aligned to segment length so HLS segments split cleanly.
		gop := SegmentSeconds * 30
		if t.FPS > 0 {
			gop = int(float64(SegmentSeconds)*t.FPS + 0.5)
		}
		args = append(args, "-g", strconv.Itoa(gop), "-sc_threshold", "0")
	}

	if t.AudioCodec == "" {
		args = append(args, "-c:a", "copy")
	} else {
		// async=1 keeps decoded audio glued to its timestamps (silence-fill
		// small gaps, trim drift) so the HLS mux stays monotonic.
		args = append(args, "-af", "aresample=async=1")
		args = append(args, "-c:a", t.AudioCodec, "-ac", "2")
		if t.AudioBitrateBps > 0 {
			args = append(args, "-b:a", strconv.FormatInt(t.AudioBitrateBps, 10))
		}
	}

	// Segment format is negotiated per client (see decision engine): TS is
	// the compatibility floor (some cast receivers reject fMP4); fMP4 is
	// used when the stream carries HEVC, which cannot ride in TS.
	args = append(args,
		"-f", "hls",
		"-hls_time", strconv.Itoa(SegmentSeconds),
		"-hls_playlist_type", "event",
	)
	if t.SegmentFormat == "fmp4" {
		args = append(args,
			"-hls_segment_type", "fmp4",
			"-hls_fmp4_init_filename", "init.mp4",
			"-hls_segment_filename", filepath.Join(j.OutputDir, "seg%05d.m4s"),
		)
	} else {
		args = append(args,
			"-hls_segment_filename", filepath.Join(j.OutputDir, "seg%05d.ts"),
		)
	}
	args = append(args, filepath.Join(j.OutputDir, "index.m3u8"))
	return args
}

func encoderFor(codec string) string {
	switch codec {
	case "h264":
		return "libx264"
	case "hevc":
		return "libx265"
	default:
		return codec
	}
}

func joinFilters(fs []string) string {
	out := fs[0]
	for _, f := range fs[1:] {
		out += "," + f
	}
	return out
}

// Session is one live transcode: an ffmpeg process writing HLS segments.
type Session struct {
	ID  string
	Job Job
	cmd *exec.Cmd
}

func (s *Session) start() error {
	if err := os.MkdirAll(s.Job.OutputDir, 0o755); err != nil {
		return err
	}
	logFile, err := os.Create(filepath.Join(s.Job.OutputDir, "ffmpeg.log"))
	if err != nil {
		return err
	}
	s.cmd = exec.Command("ffmpeg", BuildArgs(s.Job)...)
	s.cmd.Stdout = logFile
	s.cmd.Stderr = logFile
	if err := s.cmd.Start(); err != nil {
		logFile.Close()
		return err
	}
	go func() {
		s.cmd.Wait()
		logFile.Close()
	}()
	return nil
}

func (s *Session) stop() {
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	os.RemoveAll(s.Job.OutputDir)
}

// WaitForPlaylist blocks until ffmpeg has produced the HLS playlist (or times out).
func (s *Session) WaitForPlaylist(timeout time.Duration) error {
	playlist := filepath.Join(s.Job.OutputDir, "index.m3u8")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, err := os.Stat(playlist); err == nil && st.Size() > 0 {
			return nil
		}
		if s.cmd.ProcessState != nil {
			return fmt.Errorf("ffmpeg exited before producing a playlist (see %s)",
				filepath.Join(s.Job.OutputDir, "ffmpeg.log"))
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for HLS playlist")
}

type SessionManager struct {
	StreamsRoot string
	HW          *HWAccel // detected at startup; nil = software
	mu          sync.Mutex
	sessions    map[string]*Session
	// lastAccess is when each session's HLS output was last fetched. A
	// client that vanishes (tab closed, cast device unplugged) never sends
	// a stop — idle sessions are reaped instead of transcoding to file-end.
	lastAccess map[string]time.Time
}

func NewSessionManager(streamsRoot string, hw *HWAccel) *SessionManager {
	return &SessionManager{
		StreamsRoot: streamsRoot, HW: hw,
		sessions:   map[string]*Session{},
		lastAccess: map[string]time.Time{},
	}
}

func (m *SessionManager) Create(inputURL string, target model.TranscodeTarget, seekSeconds float64) (*Session, error) {
	buf := make([]byte, 6)
	rand.Read(buf)
	id := hex.EncodeToString(buf)
	s := &Session{
		ID: id,
		Job: Job{
			InputURL:    inputURL,
			Target:      target,
			OutputDir:   filepath.Join(m.StreamsRoot, id),
			SeekSeconds: seekSeconds,
			HW:          m.HW,
		},
	}
	if err := s.start(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.lastAccess[id] = time.Now()
	m.mu.Unlock()
	return s, nil
}

// Touch records that a session's output was just fetched, deferring the reaper.
func (m *SessionManager) Touch(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[id]; ok {
		m.lastAccess[id] = time.Now()
	}
}

// ReapIdle stops sessions that nobody has fetched from within maxIdle and
// returns their ids (for logging).
func (m *SessionManager) ReapIdle(maxIdle time.Duration) []string {
	m.mu.Lock()
	ids := idleSessionIDs(m.lastAccess, time.Now(), maxIdle)
	m.mu.Unlock()
	for _, id := range ids {
		m.Stop(id)
	}
	return ids
}

// idleSessionIDs is the pure core of the reaper: which sessions have not
// been accessed within maxIdle as of now.
func idleSessionIDs(lastAccess map[string]time.Time, now time.Time, maxIdle time.Duration) []string {
	var ids []string
	for id, at := range lastAccess {
		if now.Sub(at) > maxIdle {
			ids = append(ids, id)
		}
	}
	return ids
}

// ActiveCount reports live transcode sessions (used to pause scanning).
func (m *SessionManager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

func (m *SessionManager) Get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *SessionManager) Stop(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	delete(m.sessions, id)
	delete(m.lastAccess, id)
	m.mu.Unlock()
	if s != nil {
		s.stop()
	}
}

func (m *SessionManager) StopAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Stop(id)
	}
}
