package main

// The bucket, as the worker needs it: a URL ffmpeg can read the source
// from, the source's etag (so a file the scan has not caught up with is
// left alone), the upload, and the removal of what the upload replaced.
// The interface is the seam a test fills in memory.

import (
	"context"
	"errors"
	"log"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// presignFor is how long a source URL stays good: a whole film through the
// card, or a whole film's bytes through a remux, with room to spare.
const presignFor = 8 * time.Hour

var errGone = errors.New("the object is not in the bucket")

type bucket interface {
	Presign(ctx context.Context, key string) (string, error)
	// Stat answers the object's etag, or errGone.
	Stat(ctx context.Context, key string) (string, error)
	Put(ctx context.Context, key, path string) error
	Remove(ctx context.Context, key string) error
}

type minioBucket struct {
	c      *minio.Client
	bucket string
}

func openBucket(cfg config) bucket {
	c, err := minio.New(cfg.MinIOEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.MinIOAccessKey, cfg.MinIOSecretKey, ""),
		Secure: false,
	})
	if err != nil {
		log.Fatalf("converter: bad MINIO_ENDPOINT %q: %v", cfg.MinIOEndpoint, err)
	}
	return &minioBucket{c: c, bucket: cfg.MinIOBucket}
}

func (b *minioBucket) Presign(ctx context.Context, key string) (string, error) {
	u, err := b.c.PresignedGetObject(ctx, b.bucket, key, presignFor, url.Values{})
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (b *minioBucket) Stat(ctx context.Context, key string) (string, error) {
	info, err := b.c.StatObject(ctx, b.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return "", errGone
		}
		return "", err
	}
	return strings.Trim(info.ETag, `"`), nil
}

func (b *minioBucket) Put(ctx context.Context, key, path string) error {
	_, err := b.c.FPutObject(ctx, b.bucket, key, path, minio.PutObjectOptions{ContentType: "video/mp4"})
	return err
}

func (b *minioBucket) Remove(ctx context.Context, key string) error {
	return b.c.RemoveObject(ctx, b.bucket, key, minio.RemoveObjectOptions{})
}

// runCommand is the exec seam's real half.
func runCommand(ctx context.Context, name string, args []string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
