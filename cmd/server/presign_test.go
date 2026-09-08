package main

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// A presigned URL is bound to the host it was signed for, so the public
// endpoint has to show up in the URL a device is handed. No network: minio.New
// and PresignedGetObject are local arithmetic.
func TestPresignAgainstPublicEndpoint(t *testing.T) {
	creds := credentials.NewStaticV4("AKIAFAKE", "s3cr3t", "")
	host, opts, err := presignEndpoint("https://minio.example.test", creds)
	if err != nil {
		t.Fatalf("presignEndpoint: %v", err)
	}
	// A region in hand is a bucket-location lookup not made: this test signs
	// without ever touching the network.
	opts.Region = "us-east-1"
	client, err := minio.New(host, opts)
	if err != nil {
		t.Fatalf("minio.New: %v", err)
	}
	u, err := client.PresignedGetObject(context.Background(), "bkt", "Movies/x.mkv", 6*time.Hour, url.Values{})
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	if u.Scheme != "https" {
		t.Errorf("scheme = %q, want https", u.Scheme)
	}
	if u.Host != "minio.example.test" {
		t.Errorf("host = %q, want minio.example.test", u.Host)
	}
	if u.Path != "/bkt/Movies/x.mkv" {
		t.Errorf("path = %q, want /bkt/Movies/x.mkv", u.Path)
	}
	if u.Query().Get("X-Amz-Signature") == "" {
		t.Errorf("no X-Amz-Signature in %s", u)
	}
}

// Unset MINIO_PUBLIC_ENDPOINT is the LAN client, unchanged: plain http and the
// storage host itself.
func TestPresignAgainstLANEndpoint(t *testing.T) {
	client, err := minio.New("192.168.1.40:9000", &minio.Options{
		Creds:  credentials.NewStaticV4("AKIAFAKE", "s3cr3t", ""),
		Secure: false,
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("minio.New: %v", err)
	}
	u, err := client.PresignedGetObject(context.Background(), "bkt", "Movies/x.mkv", 6*time.Hour, url.Values{})
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	if u.Scheme != "http" {
		t.Errorf("scheme = %q, want http", u.Scheme)
	}
	if u.Host != "192.168.1.40:9000" {
		t.Errorf("host = %q, want 192.168.1.40:9000", u.Host)
	}
	if u.Path != "/bkt/Movies/x.mkv" {
		t.Errorf("path = %q, want /bkt/Movies/x.mkv", u.Path)
	}
	if u.Query().Get("X-Amz-Signature") == "" {
		t.Errorf("no X-Amz-Signature in %s", u)
	}
}

func TestPresignEndpoint(t *testing.T) {
	creds := credentials.NewStaticV4("AKIAFAKE", "s3cr3t", "")
	cases := []struct {
		name    string
		raw     string
		host    string
		secure  bool
		wantErr bool
	}{
		{name: "https", raw: "https://minio.kopsa.info", host: "minio.kopsa.info", secure: true},
		{name: "http", raw: "http://minio.lan", host: "minio.lan"},
		{name: "port", raw: "http://192.168.1.40:9000", host: "192.168.1.40:9000"},
		{name: "trailing slash", raw: "https://minio.kopsa.info/", host: "minio.kopsa.info", secure: true},
		{name: "no scheme", raw: "minio.kopsa.info:9000", wantErr: true},
		{name: "path", raw: "https://minio.kopsa.info/media", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, opts, err := presignEndpoint(tc.raw, creds)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("presignEndpoint(%q) = %q, want an error", tc.raw, host)
				}
				return
			}
			if err != nil {
				t.Fatalf("presignEndpoint(%q): %v", tc.raw, err)
			}
			if host != tc.host {
				t.Errorf("host = %q, want %q", host, tc.host)
			}
			if opts.Secure != tc.secure {
				t.Errorf("secure = %v, want %v", opts.Secure, tc.secure)
			}
		})
	}
}
