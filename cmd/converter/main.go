package main

// The conversion worker: it runs on the box with the card, reads flickr's
// item list, and rewrites every file that some device in the house would
// otherwise have transcoded — MP4, H.264, an AAC stereo track first — back
// into the bucket where the original was. Nothing about the library lives
// here: flickr's probe says what each file is, internal/pipeline says what
// to do about it, this binary owns ffmpeg and the hours.
//
// It starts in DRY_RUN: the first pass logs the plan for the whole library
// and its totals and writes nothing. DRY_RUN=0 is the decision to convert.
//
// main reads the env and builds the world; the loop is run() in worker.go.

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"flickr/internal/bearer"
)

type config struct {
	FlickrURL string // the flickr whose library this is
	WorkDir   string // where a file is written before it is uploaded
	IdleSleep time.Duration

	// The gate, in one of three shapes (internal/bearer).
	Issuer       string
	ClientID     string
	ClientSecret string
	StaticToken  string

	// The bucket: read, written and pruned by this worker, so all four.
	MinIOEndpoint  string
	MinIOAccessKey string
	MinIOSecretKey string
	MinIOBucket    string

	DryRun       bool   // report the plan, touch nothing
	KeepOriginal bool   // leave the source object beside the new one
	Device       string // the render node h264_vaapi encodes on
	FFmpeg       string
	FFprobe      string
}

func main() {
	log.SetFlags(log.LstdFlags)
	cfg := readConfig()

	// A signal means stop after this item: a re-encode has been running for
	// minutes and the bucket is only written once it is verified, so the
	// worst a first signal loses is that. A second signal cancels the run.
	work, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan struct{})
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Printf("converter: stopping after this item (signal again to give up on it)")
		close(stop)
		<-sigs
		cancel()
	}()

	d := deps{
		HTTP:     &http.Client{Timeout: 2 * time.Minute},
		Tokens:   tokens(cfg),
		Bucket:   openBucket(cfg),
		Run:      runCommand,
		Sleep:    sleep,
		Now:      time.Now,
		Stopping: stop,
	}
	announce(cfg)
	if err := run(work, cfg, d); err != nil {
		log.Fatal(err)
	}
}

func readConfig() config {
	cfg := config{
		FlickrURL:      os.Getenv("FLICKR_URL"),
		WorkDir:        envOr("WORK_DIR", os.TempDir()),
		Issuer:         os.Getenv("OIDC_ISSUER"),
		ClientID:       os.Getenv("CONVERTER_CLIENT_ID"),
		ClientSecret:   os.Getenv("CONVERTER_CLIENT_SECRET"),
		StaticToken:    os.Getenv("FLICKR_TOKEN"),
		MinIOEndpoint:  os.Getenv("MINIO_ENDPOINT"),
		MinIOAccessKey: os.Getenv("MINIO_ACCESS_KEY"),
		MinIOSecretKey: os.Getenv("MINIO_SECRET_KEY"),
		MinIOBucket:    os.Getenv("MINIO_BUCKET"),
		DryRun:         envOr("DRY_RUN", "1") != "0",
		KeepOriginal:   os.Getenv("KEEP_ORIGINAL") == "1",
		Device:         envOr("VAAPI_DEVICE", "/dev/dri/renderD128"),
		FFmpeg:         envOr("FFMPEG", "ffmpeg"),
		FFprobe:        envOr("FFPROBE", "ffprobe"),
	}
	var missing []string
	if cfg.FlickrURL == "" {
		missing = append(missing, "FLICKR_URL (the flickr whose library this is)")
	}
	for _, kv := range []struct{ k, v string }{
		{"MINIO_ENDPOINT", cfg.MinIOEndpoint}, {"MINIO_ACCESS_KEY", cfg.MinIOAccessKey},
		{"MINIO_SECRET_KEY", cfg.MinIOSecretKey}, {"MINIO_BUCKET", cfg.MinIOBucket},
	} {
		if kv.v == "" {
			missing = append(missing, kv.k+" (the worker writes the bucket, so it needs all four)")
		}
	}
	if len(missing) > 0 {
		log.Fatalf("converter: %s must be set", strings.Join(missing, "; "))
	}
	oidc := 0
	for _, v := range []string{cfg.Issuer, cfg.ClientID, cfg.ClientSecret} {
		if v != "" {
			oidc++
		}
	}
	if oidc != 0 && oidc != 3 {
		log.Fatal("converter: OIDC_ISSUER, CONVERTER_CLIENT_ID and CONVERTER_CLIENT_SECRET must be set together or not at all")
	}
	var err error
	cfg.IdleSleep, err = time.ParseDuration(envOr("IDLE_SLEEP", "30m"))
	if err != nil {
		log.Fatalf("converter: bad IDLE_SLEEP: %v", err)
	}
	return cfg
}

// tokens picks the shape of the gate this worker is behind.
func tokens(cfg config) bearer.Source {
	switch {
	case cfg.ClientID != "":
		log.Printf("converter: signing in to %s as %s", cfg.Issuer, cfg.ClientID)
		return &bearer.ClientCredentials{
			HTTP:     &http.Client{Timeout: 30 * time.Second},
			TokenURL: bearer.TokenURL(cfg.Issuer),
			ID:       cfg.ClientID, Secret: cfg.ClientSecret,
		}
	case cfg.StaticToken != "":
		log.Printf("converter: using the bearer in FLICKR_TOKEN")
		return bearer.Static(cfg.StaticToken)
	default:
		log.Printf("converter: no token configured, asking anonymously")
		return bearer.None{}
	}
}

// announce prints the settings (never a secret). Whether the card works is
// said by run()'s own probe, from a real encode.
func announce(cfg config) {
	mode := "CONVERTING — originals removed once the new file is verified and uploaded"
	if cfg.DryRun {
		mode = "DRY RUN — the plan is logged, nothing is written (DRY_RUN=0 converts)"
	} else if cfg.KeepOriginal {
		mode = "CONVERTING — originals kept beside the new files (KEEP_ORIGINAL=1)"
	}
	log.Printf("converter: flickr %s, bucket %s/%s, work dir %s, device %s, idle sleep %s",
		cfg.FlickrURL, cfg.MinIOEndpoint, cfg.MinIOBucket, cfg.WorkDir, cfg.Device, cfg.IdleSleep)
	log.Printf("converter: %s", mode)
}

// sleep waits, or gives up waiting when the worker is stopping.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
