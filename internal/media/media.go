// Package media is the D1-revised upload path (lumen §5): the portal posts
// multipart to lumen, lumen probes + strips EXIF BEFORE the object becomes
// public, relays to S3-compatible storage, and returns the absolute CDN
// URL. Provider choice (HUMAN-GATE #9) is ENV VALUES — this code speaks
// the API, not a vendor.
package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const MaxBytes = 20 << 20 // 20 MiB — generous for originals, hostile to abuse

// Processed is the sanitized artifact: re-encoded bytes (EXIF and every
// other ancillary chunk gone with them by construction), natural dims,
// content address.
type Processed struct {
	Key         string // media/<xx>/<sha>.<ext> — content-addressed, immutable
	Data        []byte
	SHA256      string
	ContentType string
	Width       int
	Height      int
	Ext         string
}

// Process decodes the upload (REAL decode — client-declared content types
// are a suggestion, the pixels are the truth), re-encodes it in its own
// format (stripping metadata as a side effect of rebuild), and derives the
// key. Allowed set is jpeg/png (lumen §6: "pin the format list to what the
// gallery needs"); webp/gif are a deliberate 415 until there's an encoder
// story we trust — recorded, not silently dropped.
func Process(raw []byte) (Processed, error) {
	if len(raw) == 0 {
		return Processed{}, fmt.Errorf("empty upload")
	}
	cfg, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return Processed{}, fmt.Errorf("undecodable image: %w", err)
	}
	var buf bytes.Buffer
	var ext, ctype string
	switch format {
	case "jpeg":
		ext, ctype = "jpg", "image/jpeg"
		if err := jpeg.Encode(&buf, cfg, &jpeg.Options{Quality: 90}); err != nil {
			return Processed{}, err
		}
	case "png":
		ext, ctype = "png", "image/png"
		if err := png.Encode(&buf, cfg); err != nil {
			return Processed{}, err
		}
	default:
		return Processed{}, fmt.Errorf("unsupported format %q (jpeg|png only today)", format)
	}
	sum := sha256.Sum256(buf.Bytes())
	hexSum := hex.EncodeToString(sum[:])
	b := cfg.Bounds()
	return Processed{
		Key:         fmt.Sprintf("media/%s/%s.%s", hexSum[:2], hexSum, ext),
		Data:        buf.Bytes(),
		SHA256:      hexSum,
		ContentType: ctype,
		Width:       b.Dx(),
		Height:      b.Dy(),
		Ext:         ext,
	}, nil
}

// Store is the object-relay seam (faked in unit tests, S3 in life).
type Store interface {
	Put(ctx context.Context, key, contentType string, data []byte) error
	EnsureBucket(ctx context.Context) error
}

type s3Store struct {
	client       *s3.Client
	bucket       string
	createBucket bool // dev escape hatch (minio); real providers: false
}

type Config struct {
	Endpoint     string // "" → AWS; minio dev: http://localhost:9000
	Region       string
	Bucket       string
	CDNBase      string
	AccessKey    string
	SecretKey    string
	ForcePath    bool // minio/private: path-style addressing
	CreateBucket bool // dev-only bucket bootstrap
}

func NewS3(ctx context.Context, c Config) (Store, string, error) {
	if c.Bucket == "" {
		return nil, "", fmt.Errorf("media bucket not configured")
	}
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(orDefault(c.Region, "us-east-1")),
	}
	if c.AccessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, "")))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, "", err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
		}
		o.UsePathStyle = c.ForcePath
	})
	return &s3Store{client: client, bucket: c.Bucket, createBucket: c.CreateBucket}, c.CDNBase, nil
}

func (s *s3Store) Put(ctx context.Context, key, contentType string, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType),
		// content-addressed key ⇒ immutable caching is HONEST (lumen §5)
		CacheControl: aws.String("public, max-age=31536000, immutable"),
	})
	return err
}

func (s *s3Store) EnsureBucket(ctx context.Context) error {
	if !s.createBucket {
		return nil
	}
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	if err == nil {
		return nil
	}
	_, err = s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(s.bucket)})
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return err
	}
	return nil
}

// URL composes the absolute serving URL: CDN base in prod, endpoint+bucket
// in dev. The portal embeds the result VERBATIM (FE never constructs).
func URL(cdnBase, endpoint, bucket, key string) string {
	base := cdnBase
	if base == "" {
		base = strings.TrimSuffix(endpoint, "/") + "/" + bucket
	}
	return base + "/" + key
}

func orDefault(v, d string) string {
	if v != "" {
		return v
	}
	return d
}
