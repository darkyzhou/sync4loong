package storage

import (
	"fmt"

	"sync4loong/pkg/s3"
)

type StorageFactory struct{}

func NewStorageFactory() *StorageFactory {
	return &StorageFactory{}
}

func (f *StorageFactory) CreateS3Backend(config *S3Config) (StorageBackend, error) {
	if config == nil {
		return nil, fmt.Errorf("S3 configuration is required")
	}

	s3Client, err := s3.CreateS3Client(&s3.Config{
		Endpoint:  config.Endpoint,
		Region:    config.Region,
		Bucket:    config.Bucket,
		AccessKey: config.AccessKey,
		SecretKey: config.SecretKey,
	})
	if err != nil {
		return nil, fmt.Errorf("create S3 client: %w", err)
	}

	return NewS3Backend(s3Client, config), nil
}
