package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type ObjectStore interface {
	Put(context.Context, string, io.ReadSeeker, int64, string) error
	Open(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
	Ping(context.Context) error
}

type LocalStore struct{ Directory string }

func (s *LocalStore) path(key string) (string, error) {
	if strings.Contains(key, "..") || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return "", fmt.Errorf("invalid object key")
	}
	return filepath.Join(s.Directory, filepath.FromSlash(key)), nil
}
func (s *LocalStore) Put(ctx context.Context, key string, r io.ReadSeeker, size int64, contentType string) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = io.Copy(f, r)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}
func (s *LocalStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}
func (s *LocalStore) Delete(ctx context.Context, key string) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
func (s *LocalStore) Ping(ctx context.Context) error {
	if err := os.MkdirAll(s.Directory, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Directory, ".health-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

type S3Store struct {
	client *s3.Client
	bucket string
}

func newObjectStore(ctx context.Context, c StorageConfig) (ObjectStore, error) {
	if c.Driver == "local" {
		return &LocalStore{Directory: c.Directory}, nil
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(c.Region))
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = c.PathStyle
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
		}
	})
	return &S3Store{client: client, bucket: c.Bucket}, nil
}
func (s *S3Store) Put(ctx context.Context, key string, r io.ReadSeeker, size int64, contentType string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: r, ContentLength: aws.Int64(size), ContentType: aws.String(contentType)})
	return err
}
func (s *S3Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	r, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	return r.Body, nil
}
func (s *S3Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err
}
func (s *S3Store) Ping(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	return err
}
