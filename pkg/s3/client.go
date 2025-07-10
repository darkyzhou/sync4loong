package s3

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

type Config struct {
	Endpoint   string
	Region     string
	Bucket     string
	AccessKey  string
	SecretKey  string
	MaxRetries int
}

func CreateS3Client(config *Config) (*s3.S3, error) {
	maxRetries := config.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	session, err := session.NewSession(&aws.Config{
		Region:           aws.String(config.Region),
		Endpoint:         aws.String(config.Endpoint),
		S3ForcePathStyle: aws.Bool(true),
		MaxRetries:       aws.Int(maxRetries),
		Credentials:      credentials.NewStaticCredentials(config.AccessKey, config.SecretKey, ""),
	})
	if err != nil {
		return nil, err
	}

	s3Client := s3.New(session)
	return s3Client, nil
}
