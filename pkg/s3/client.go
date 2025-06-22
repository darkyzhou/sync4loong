package s3

import (
	"net/http"
	"sync4loong/pkg/config"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

func CreateS3Client(config *config.Config) (*s3.S3, error) {
	httpClient := &http.Client{
		Timeout: time.Duration(config.S3.ReadTimeoutSeconds) * time.Second,
		Transport: &http.Transport{
			ResponseHeaderTimeout: time.Duration(config.S3.ReadTimeoutSeconds) * time.Second,
			ExpectContinueTimeout: 5 * time.Second,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
		},
	}

	retryer := client.DefaultRetryer{
		NumMaxRetries:    config.S3.MaxRetries,
		MinRetryDelay:    time.Duration(config.S3.RetryDelaySeconds) * time.Second,
		MaxRetryDelay:    time.Duration(config.S3.MaxRetryDelaySeconds) * time.Second,
		MinThrottleDelay: time.Duration(config.S3.RetryDelaySeconds) * time.Second,
		MaxThrottleDelay: time.Duration(config.S3.MaxRetryDelaySeconds) * time.Second,
	}

	session, err := session.NewSession(&aws.Config{
		Region:           aws.String(config.S3.Region),
		Endpoint:         aws.String(config.S3.Endpoint),
		S3ForcePathStyle: aws.Bool(true),
		HTTPClient:       httpClient,
		Retryer:          retryer,
		Credentials: credentials.NewStaticCredentials(
			config.S3.AccessKey,
			config.S3.SecretKey,
			"",
		),
	})
	if err != nil {
		return nil, err
	}

	s3Client := s3.New(session)
	return s3Client, nil
}
