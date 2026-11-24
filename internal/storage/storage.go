// Package storage is the S3-compatible object store (Cloudflare R2, AWS S3, MinIO) backups
// are uploaded to. Everything else depends on the Store interface, not on the AWS SDK.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Object is one stored backup.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

// Store is the bucket one app's backups live in.
type Store interface {
	Upload(ctx context.Context, path, key string) error
	List(ctx context.Context, prefix string) ([]Object, error)
	Download(ctx context.Context, key, dest string) error
	Delete(ctx context.Context, keys []string) error
}

// S3Config is what it takes to reach one bucket.
type S3Config struct {
	Endpoint        string // empty for AWS S3; https://<account>.r2.cloudflarestorage.com for R2
	Region          string // default "auto"
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
}

// S3 implements Store.
type S3 struct {
	client *s3.Client
	up     *transfermanager.Client
	bucket string
}

// NewS3 builds a client. Checksums are computed only where the API requires them: R2 and
// MinIO reject the extra trailing checksums newer SDK versions add by default. The transfer
// manager has its own setting that overrides the client's, so it is set here too.
func NewS3(s3Config S3Config) *S3 {
	region := s3Config.Region
	if region == "" {
		region = "auto"
	}
	opts := s3.Options{
		Region:                     region,
		Credentials:                credentials.NewStaticCredentialsProvider(s3Config.AccessKeyID, s3Config.SecretAccessKey, ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if s3Config.Endpoint != "" {
		opts.BaseEndpoint = aws.String(s3Config.Endpoint)
		opts.UsePathStyle = true
	}
	client := s3.New(opts)
	up := transfermanager.New(client, func(options *transfermanager.Options) {
		options.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
	return &S3{client: client, up: up, bucket: s3Config.Bucket}
}

func (store *S3) Upload(ctx context.Context, path, key string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	_, err = store.up.UploadObject(ctx, &transfermanager.UploadObjectInput{Bucket: &store.bucket, Key: &key, Body: file})
	if err != nil {
		return fmt.Errorf("upload %s: %w", key, err)
	}
	return nil
}

func (store *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	pages := s3.NewListObjectsV2Paginator(store.client, &s3.ListObjectsV2Input{Bucket: &store.bucket, Prefix: &prefix})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, item := range page.Contents {
			out = append(out, Object{Key: aws.ToString(item.Key), Size: aws.ToInt64(item.Size), Modified: aws.ToTime(item.LastModified)})
		}
	}
	return out, nil
}

func (store *S3) Download(ctx context.Context, key, dest string) error {
	res, err := store.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &store.bucket, Key: &key})
	if err != nil {
		return fmt.Errorf("download %s: %w", key, err)
	}
	defer func() { _ = res.Body.Close() }()
	destFile, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(destFile, res.Body); err != nil {
		_ = destFile.Close()
		_ = os.Remove(dest)
		return fmt.Errorf("download %s: %w", key, err)
	}
	if err := destFile.Close(); err != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("download %s: close destination: %w", key, err)
	}
	return nil
}

const deleteBatch = 1000 // the S3 API limit per request

// Delete removes keys in batches of deleteBatch (the S3 API maximum per request).
func (store *S3) Delete(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	for len(keys) > 0 {
		batchSize := min(deleteBatch, len(keys))
		batch := keys[:batchSize]
		keys = keys[batchSize:]
		ids := make([]types.ObjectIdentifier, batchSize)
		for idx, key := range batch {
			ids[idx] = types.ObjectIdentifier{Key: aws.String(key)}
		}
		res, err := store.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: &store.bucket, Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("delete: %w", err)
		}
		var errs []error
		for _, delErr := range res.Errors {
			errs = append(errs, fmt.Errorf("%s: %s", aws.ToString(delErr.Key), aws.ToString(delErr.Message)))
		}
		if len(errs) > 0 {
			return fmt.Errorf("delete: %w", errors.Join(errs...))
		}
	}
	return nil
}
