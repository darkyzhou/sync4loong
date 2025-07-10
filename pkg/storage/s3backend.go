package storage

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
)

type S3Backend struct {
	client *s3.S3
	bucket string
	config *S3Config
}

type S3Config struct {
	Endpoint                    string `mapstructure:"endpoint" validate:"required,url"`
	Region                      string `mapstructure:"region" validate:"required,min=1"`
	Bucket                      string `mapstructure:"bucket" validate:"required,min=1"`
	AccessKey                   string `mapstructure:"access_key" validate:"required,min=1"`
	SecretKey                   string `mapstructure:"secret_key" validate:"required,min=1"`
	MaxRetries                  int    `mapstructure:"max_retries" validate:"min=0,max=10"`
	FileUploadRetryCount        int    `mapstructure:"file_upload_retry_count" validate:"min=0,max=5"`
	FileUploadRetryDelaySeconds int    `mapstructure:"file_upload_retry_delay_seconds" validate:"min=1,max=30"`
	FileUploadTimeoutSeconds    int    `mapstructure:"file_upload_timeout_seconds" validate:"min=1,max=100000"`
	EnableIntegrityCheck        bool   `mapstructure:"enable_integrity_check"`
}

func NewS3Backend(s3Client *s3.S3, config *S3Config) *S3Backend {
	return &S3Backend{
		client: s3Client,
		bucket: config.Bucket,
		config: config,
	}
}

func (s *S3Backend) CheckFileExists(ctx context.Context, key string) (*FileMetadata, error) {
	headResp, err := s.client.HeadObjectWithContext(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})

	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case "NoSuchKey", "NotFound":
				return &FileMetadata{Exists: false}, nil
			case "Forbidden", "AccessDenied":
				return nil, &StorageError{
					Type:    ErrorTypeAccessDenied,
					Message: "access denied to check file",
					Cause:   err,
				}
			}
		}
		return nil, &StorageError{
			Type:    ErrorTypeNetworkError,
			Message: "failed to check file existence",
			Cause:   err,
		}
	}

	metadata := &FileMetadata{
		Exists: true,
		Size:   *headResp.ContentLength,
	}

	if headResp.LastModified != nil {
		metadata.LastModified = *headResp.LastModified
	}
	if headResp.ContentType != nil {
		metadata.ContentType = *headResp.ContentType
	}
	if headResp.ETag != nil {
		metadata.ETag = *headResp.ETag
	}

	return metadata, nil
}

func (s *S3Backend) UploadFile(ctx context.Context, filePath, key string, opts *UploadOptions) error {
	file, err := os.Open(filePath)
	if err != nil {
		return &StorageError{
			Type:    ErrorTypeInvalidInput,
			Message: "failed to open file",
			Cause:   err,
		}
	}
	defer func() {
		_ = file.Close()
	}()

	if opts != nil && opts.EnableIntegrityCheck && opts.ContentMD5 == "" {
		md5Hash, err := s.calculateFileMD5(file)
		if err != nil {
			return &StorageError{
				Type:    ErrorTypeInternal,
				Message: "failed to calculate file MD5",
				Cause:   err,
			}
		}
		opts.ContentMD5 = md5Hash

		if _, err := file.Seek(0, 0); err != nil {
			return &StorageError{
				Type:    ErrorTypeInternal,
				Message: "failed to reset file pointer",
				Cause:   err,
			}
		}
	}

	return s.UploadFromReader(ctx, file, key, opts)
}

func (s *S3Backend) UploadFromReader(ctx context.Context, reader io.Reader, key string, opts *UploadOptions) error {
	if opts == nil {
		opts = &UploadOptions{}
	}

	timeout := time.Duration(s.config.FileUploadTimeoutSeconds) * time.Second
	uploadCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var body io.ReadSeeker
	if readSeeker, ok := reader.(io.ReadSeeker); ok {
		body = readSeeker
	} else {
		data, err := io.ReadAll(reader)
		if err != nil {
			return &StorageError{
				Type:    ErrorTypeInternal,
				Message: "failed to read data",
				Cause:   err,
			}
		}
		body = strings.NewReader(string(data))
	}

	putInput := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   body,
	}

	if opts.ContentType != "" {
		putInput.ContentType = aws.String(opts.ContentType)
	}
	if opts.ContentMD5 != "" {
		putInput.ContentMD5 = aws.String(opts.ContentMD5)
	}
	if opts.StorageClass != "" {
		putInput.StorageClass = aws.String(opts.StorageClass)
	}

	if len(opts.Metadata) > 0 {
		putInput.Metadata = aws.StringMap(opts.Metadata)
	}

	_, err := s.client.PutObjectWithContext(uploadCtx, putInput)
	if err != nil {
		return s.convertS3Error(err)
	}

	return nil
}

func (s *S3Backend) GetBackendType() BackendType {
	return BackendTypeS3
}

func (s *S3Backend) Close() error {
	return nil
}

func (s *S3Backend) calculateFileMD5(file *os.File) (string, error) {
	hash := md5.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(hash.Sum(nil)), nil
}

func (s *S3Backend) convertS3Error(err error) error {
	if err == nil {
		return nil
	}

	if aerr, ok := err.(awserr.Error); ok {
		switch aerr.Code() {
		case "NoSuchBucket", "NoSuchKey", "NotFound":
			return &StorageError{
				Type:    ErrorTypeNotFound,
				Message: "resource not found",
				Cause:   err,
			}
		case "AccessDenied", "Forbidden":
			return &StorageError{
				Type:    ErrorTypeAccessDenied,
				Message: "access denied",
				Cause:   err,
			}
		case "RequestTimeout", "ServiceUnavailable", "Throttling", "ThrottlingException":
			return &StorageError{
				Type:    ErrorTypeNetworkError,
				Message: "service temporarily unavailable",
				Cause:   err,
			}
		default:
			if strings.Contains(strings.ToLower(aerr.Message()), "timeout") {
				return &StorageError{
					Type:    ErrorTypeNetworkError,
					Message: "request timeout",
					Cause:   err,
				}
			}
		}
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return &StorageError{
			Type:    ErrorTypeNetworkError,
			Message: "upload timeout",
			Cause:   err,
		}
	}

	return &StorageError{
		Type:    ErrorTypeInternal,
		Message: "internal storage error",
		Cause:   err,
	}
}
