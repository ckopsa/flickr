package main

// The start-up probe: which whisper is this, and is the GPU in it?
//
// `whisper-cli --help` answers the first question and not the second —
// whisper prints its device lines when it LOADS A MODEL. So the worker loads
// the model once, over half a second of generated silence, and logs what
// whisper said about the hardware. It costs one model load at start-up and
// answers the only question the job log could not: whether the card is live.

import (
	"context"
	"encoding/binary"
	"log"
	"os"
	"path/filepath"

	"flickr/internal/pipeline"
)

// The probe's audio: half a second of nothing, in the one format whisper
// reads. Generated here rather than by ffmpeg, so the probe needs no second
// binary and no file on disk to start from.
const (
	probeRate    = 16000 // Hz, mono
	probeSeconds = 0.5
)

// silentWAV is 16 kHz mono 16-bit silence with the 44-byte canonical RIFF
// header in front of it — the shape TranscribeAudioArgs asks ffmpeg for, so
// whisper reads it the same way it reads a film's audio.
func silentWAV() []byte {
	data := int(probeRate * probeSeconds * 2) // 2 bytes a sample
	b := make([]byte, 0, 44+data)
	le := func(v uint32) { b = binary.LittleEndian.AppendUint32(b, v) }
	le16 := func(v uint16) { b = binary.LittleEndian.AppendUint16(b, v) }
	b = append(b, "RIFF"...)
	le(uint32(36 + data)) // everything after this field
	b = append(b, "WAVE"...)
	b = append(b, "fmt "...)
	le(16)            // a PCM fmt chunk is 16 bytes
	le16(1)           // ... of format 1, uncompressed
	le16(1)           // mono
	le(probeRate)     // 16 kHz
	le(probeRate * 2) // bytes a second
	le16(2)           // bytes a frame
	le16(16)          // bits a sample
	b = append(b, "data"...)
	le(uint32(data))
	return append(b, make([]byte, data)...)
}

// probeBackend loads the model once and logs the GPU whisper found. It
// answers that summary so the pass can keep quiet about it afterwards unless
// it changes. A probe that fails is logged and forgiven: the first real item
// says more about a broken whisper than half a second of silence can.
func probeBackend(ctx context.Context, cfg config, d deps) string {
	dir, err := os.MkdirTemp(cfg.WorkDir, "probe-")
	if err != nil {
		log.Printf("transcriber: whisper probe: %v", err)
		return ""
	}
	defer os.RemoveAll(dir)
	wav := filepath.Join(dir, "silence.wav")
	if err := os.WriteFile(wav, silentWAV(), 0o644); err != nil {
		log.Printf("transcriber: whisper probe: %v", err)
		return ""
	}
	if _, err := d.Trans.Whisper(ctx, wav, filepath.Join(dir, "probe")); err != nil {
		log.Printf("transcriber: whisper probe failed: %v", err)
	}
	backend := pipeline.WhisperBackend(d.Trans.LastOutput)
	if backend == "" {
		log.Printf("whisper backend: none reported (CPU build?)")
	} else {
		log.Printf("whisper backend: %s", backend)
	}
	return backend
}
