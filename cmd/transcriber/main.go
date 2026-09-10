package main

// The transcription worker: it runs on whichever box has the GPU, asks flickr
// for silent files and hands the cues back. Nothing about the library lives
// here — flickr says what needs hearing and where the answer goes, this
// binary owns whisper.cpp and the hours it spends.
//
// main reads the env and builds the world; the loop is run() in worker.go.

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"flickr/internal/bearer"
	"flickr/internal/pipeline"
)

// presignFor is how long the worker's own URL for an object stays good: long
// enough for ffmpeg to read a whole film's audio down over the LAN.
const presignFor = 6 * time.Hour

type config struct {
	FlickrURL string // the flickr this worker serves
	WorkDir   string // where a transcript is written before it is delivered
	IdleSleep time.Duration

	// The gate, in one of three shapes (see token.go).
	Issuer       string
	ClientID     string
	ClientSecret string
	StaticToken  string

	WhisperBin      string
	WhisperModel    string
	WhisperLanguage string
	WhisperThreads  int

	// The bucket, when this box can reach it directly.
	MinIOEndpoint  string
	MinIOAccessKey string
	MinIOSecretKey string
	MinIOBucket    string
}

func main() {
	log.SetFlags(log.LstdFlags)
	cfg := readConfig()

	// A signal means stop after this item: whisper has been running for
	// hours and killing it throws all of it away. A second signal is the
	// impatient answer and cancels the run.
	work, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan struct{})
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Printf("transcriber: stopping after this item (signal again to give up on it)")
		close(stop)
		<-sigs
		cancel()
	}()

	d := deps{
		HTTP:   &http.Client{Timeout: 2 * time.Minute},
		Tokens: tokens(cfg),
		Trans: &pipeline.Transcriber{
			Bin: cfg.WhisperBin, Model: cfg.WhisperModel,
			Language: cfg.WhisperLanguage, Threads: cfg.WhisperThreads,
		},
		Presign:  presigner(cfg),
		Sleep:    sleep,
		Now:      time.Now,
		Stopping: stop,
	}
	announce(work, cfg)
	if err := run(work, cfg, d); err != nil {
		log.Fatal(err)
	}
}

func readConfig() config {
	cfg := config{
		FlickrURL:       os.Getenv("FLICKR_URL"),
		WorkDir:         envOr("WORK_DIR", os.TempDir()),
		Issuer:          os.Getenv("OIDC_ISSUER"),
		ClientID:        os.Getenv("TRANSCRIBER_CLIENT_ID"),
		ClientSecret:    os.Getenv("TRANSCRIBER_CLIENT_SECRET"),
		StaticToken:     os.Getenv("FLICKR_TOKEN"),
		WhisperBin:      envOr("WHISPER_BIN", "whisper-cli"),
		WhisperModel:    os.Getenv("WHISPER_MODEL"),
		WhisperLanguage: os.Getenv("WHISPER_LANGUAGE"),
		MinIOEndpoint:   os.Getenv("MINIO_ENDPOINT"),
		MinIOAccessKey:  os.Getenv("MINIO_ACCESS_KEY"),
		MinIOSecretKey:  os.Getenv("MINIO_SECRET_KEY"),
		MinIOBucket:     os.Getenv("MINIO_BUCKET"),
	}
	// Both of these are the job itself: without them there is nothing to ask
	// and nothing to ask with.
	var missing []string
	if cfg.FlickrURL == "" {
		missing = append(missing, "FLICKR_URL (the flickr to ask for work)")
	}
	if cfg.WhisperModel == "" {
		missing = append(missing, "WHISPER_MODEL (a ggml model file)")
	}
	if len(missing) > 0 {
		log.Fatalf("transcriber: %s must be set", strings.Join(missing, " and "))
	}
	// A half-configured client is a worker that gets 401s all night.
	oidc := 0
	for _, v := range []string{cfg.Issuer, cfg.ClientID, cfg.ClientSecret} {
		if v != "" {
			oidc++
		}
	}
	if oidc != 0 && oidc != 3 {
		log.Fatal("transcriber: OIDC_ISSUER, TRANSCRIBER_CLIENT_ID and TRANSCRIBER_CLIENT_SECRET must be set together or not at all")
	}
	var err error
	cfg.IdleSleep, err = time.ParseDuration(envOr("IDLE_SLEEP", "5m"))
	if err != nil {
		log.Fatalf("transcriber: bad IDLE_SLEEP: %v", err)
	}
	if t := os.Getenv("WHISPER_THREADS"); t != "" {
		if cfg.WhisperThreads, err = strconv.Atoi(t); err != nil {
			log.Fatalf("transcriber: bad WHISPER_THREADS: %v", err)
		}
	}
	return cfg
}

// tokens picks the shape of the gate this worker is behind.
func tokens(cfg config) bearer.Source {
	switch {
	case cfg.ClientID != "":
		log.Printf("transcriber: signing in to %s as %s", cfg.Issuer, cfg.ClientID)
		return &bearer.ClientCredentials{
			HTTP:     &http.Client{Timeout: 30 * time.Second},
			TokenURL: bearer.TokenURL(cfg.Issuer),
			ID:       cfg.ClientID, Secret: cfg.ClientSecret,
		}
	case cfg.StaticToken != "":
		log.Printf("transcriber: using the bearer in FLICKR_TOKEN")
		return bearer.Static(cfg.StaticToken)
	default:
		log.Printf("transcriber: no token configured, asking anonymously")
		return bearer.None{}
	}
}

// presigner signs this worker's own URLs against the LAN endpoint. The queue
// signs its urls for the public host — right for a phone on LTE, wrong for a
// box sitting on the same switch as the storage — so where the bucket's
// address and keys are in hand, the worker signs its own instead.
func presigner(cfg config) func(context.Context, string) (string, error) {
	if cfg.MinIOEndpoint == "" || cfg.MinIOAccessKey == "" || cfg.MinIOSecretKey == "" || cfg.MinIOBucket == "" {
		return nil
	}
	c, err := minio.New(cfg.MinIOEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.MinIOAccessKey, cfg.MinIOSecretKey, ""),
		Secure: false,
	})
	if err != nil {
		log.Fatalf("transcriber: bad MINIO_ENDPOINT %q: %v", cfg.MinIOEndpoint, err)
	}
	log.Printf("transcriber: reading audio from %s/%s directly", cfg.MinIOEndpoint, cfg.MinIOBucket)
	return func(ctx context.Context, key string) (string, error) {
		u, err := c.PresignedGetObject(ctx, cfg.MinIOBucket, key, presignFor, url.Values{})
		if err != nil {
			return "", err
		}
		return u.String(), nil
	}
}

// announce prints the settings (never a secret) and says which whisper it
// found. A missing binary is not fatal here: the first item fails loudly with
// the real error, which is more use than a guess at start-up. --help says
// nothing about hardware, so the GPU line comes after this one, from the
// model load run() probes with (probe.go).
func announce(ctx context.Context, cfg config) {
	log.Printf("transcriber: flickr %s, model %s, language %s, work dir %s, idle sleep %s",
		cfg.FlickrURL, cfg.WhisperModel, envOr("WHISPER_LANGUAGE", "auto"), cfg.WorkDir, cfg.IdleSleep)
	if cfg.WhisperThreads > 0 {
		log.Printf("transcriber: whisper threads %d", cfg.WhisperThreads)
	}
	out, err := exec.CommandContext(ctx, cfg.WhisperBin, "--help").CombinedOutput()
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
	if err != nil && line == "" {
		log.Printf("whisper: not found (%s: %v)", cfg.WhisperBin, err)
		return
	}
	log.Printf("whisper: %s", line)
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
