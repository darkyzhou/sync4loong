package s3

import (
	"sync4loong/pkg/config"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

func CreateS3Client(config *config.Config) (*s3.S3, error) {
	session, err := session.NewSession(&aws.Config{
		Region:           aws.String(config.S3.Region),
		Endpoint:         aws.String(config.S3.Endpoint),
		S3ForcePathStyle: aws.Bool(true),
		MaxRetries:       aws.Int(config.S3.MaxRetries),
		Credentials:      credentials.NewStaticCredentials(config.S3.AccessKey, config.S3.SecretKey, ""),
	})
	if err != nil {
		return nil, err
	}

	s3Client := s3.New(session)
	return s3Client, nil
}
