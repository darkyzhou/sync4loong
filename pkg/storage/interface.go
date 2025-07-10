package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

type StorageBackend interface {
	GetBackendType() BackendType
	CheckFileExists(ctx context.Context, key string) (*FileMetadata, error)
	UploadFile(ctx context.Context, filePath, key string, opts *UploadOptions) error
	UploadFromReader(ctx context.Context, reader io.Reader, key string, opts *UploadOptions) error
	Close() error
}

type FileMetadata struct {
	Exists       bool      `json:"exists"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
	ContentType  string    `json:"content_type,omitempty"`
	ETag         string    `json:"etag,omitempty"`
}

type UploadOptions struct {
	ContentType          string            `json:"content_type,omitempty"`
	ContentMD5           string            `json:"content_md5,omitempty"`
	Metadata             map[string]string `json:"metadata,omitempty"`
	StorageClass         string            `json:"storage_class,omitempty"`
	EnableIntegrityCheck bool              `json:"enable_integrity_check"`
}

type BackendType string

const (
	BackendTypeS3 BackendType = "s3"
)

type StorageError struct {
	Type    ErrorType
	Message string
	Cause   error
}

type ErrorType string

const (
	ErrorTypeNotFound     ErrorType = "not_found"
	ErrorTypeAccessDenied ErrorType = "access_denied"
	ErrorTypeNetworkError ErrorType = "network_error"
	ErrorTypeInternal     ErrorType = "internal_error"
	ErrorTypeInvalidInput ErrorType = "invalid_input"
)

func (e *StorageError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s (%v)", e.Type, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Type, e.Message)
}

func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}

	var storageErr *StorageError
	if !errors.As(err, &storageErr) {
		return false
	}

	switch storageErr.Type {
	case ErrorTypeNetworkError:
		return true
	case ErrorTypeInternal:
		return true
	case ErrorTypeNotFound, ErrorTypeAccessDenied, ErrorTypeInvalidInput:
		return false
	default:
		return false
	}
}
