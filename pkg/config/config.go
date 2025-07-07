package config

import (
	"fmt"

	"github.com/go-playground/validator/v10"
	"github.com/spf13/viper"
)

type Config struct {
	Redis    RedisConfig    `mapstructure:"redis" validate:"required"`
	S3       S3Config       `mapstructure:"s3" validate:"required"`
	Daemon   DaemonConfig   `mapstructure:"daemon" validate:"required"`
	Publish  PublishConfig  `mapstructure:"publish" validate:"required"`
	HTTP     HTTPConfig     `mapstructure:"http" validate:"required"`
	Cache    CacheConfig    `mapstructure:"cache" validate:"required"`
	Asynqmon AsynqmonConfig `mapstructure:"asynqmon" validate:"required"`
}

type RedisConfig struct {
	Addr     string `mapstructure:"addr" validate:"required,hostname_port"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db" validate:"min=0,max=15"`
}

type S3Config struct {
	Endpoint  string `mapstructure:"endpoint" validate:"required,url"`
	Region    string `mapstructure:"region" validate:"required,min=1"`
	Bucket    string `mapstructure:"bucket" validate:"required,min=1"`
	AccessKey string `mapstructure:"access_key" validate:"required,min=1"`
	SecretKey string `mapstructure:"secret_key" validate:"required,min=1"`

	MaxRetries int `mapstructure:"max_retries" validate:"min=0,max=10"`

	FileUploadRetryCount        int `mapstructure:"file_upload_retry_count" validate:"min=0,max=5"`
	FileUploadRetryDelaySeconds int `mapstructure:"file_upload_retry_delay_seconds" validate:"min=1,max=30"`
	FileUploadTimeoutSeconds    int `mapstructure:"file_upload_timeout_seconds" validate:"min=1,max=100000"`

	EnableIntegrityCheck bool `mapstructure:"enable_integrity_check"`
}

type DaemonConfig struct {
	LogLevel           string `mapstructure:"log_level" validate:"required,oneof=debug info warn error fatal"`
	SSHCommand         string `mapstructure:"ssh_command"`
	SSHDebounceMinutes int    `mapstructure:"ssh_debounce_minutes" validate:"min=1"`
	SSHTimeoutMinutes  int    `mapstructure:"ssh_timeout_minutes" validate:"min=1"`
	EnableSSHTask      bool   `mapstructure:"enable_ssh_task"`
}

type PublishConfig struct {
	MaxRetry       int `mapstructure:"max_retry" validate:"required,min=0,max=10"`
	TimeoutMinutes int `mapstructure:"timeout_minutes" validate:"required,min=1,max=1440"`
}

type HTTPConfig struct {
	Addr string `mapstructure:"addr" validate:"required,hostname_port"`
}

type CacheConfig struct {
	MaxConcurrentS3Checks int      `mapstructure:"max_concurrent_s3_checks" validate:"min=1,max=100"`
	AllowedPrefixes       []string `mapstructure:"allowed_prefixes" validate:"required,min=1"`
}

type AsynqmonConfig struct {
	Enabled        bool   `mapstructure:"enabled"`
	RootPath       string `mapstructure:"root_path" validate:"required"`
	ReadOnlyMode   bool   `mapstructure:"read_only_mode"`
	PrometheusAddr string `mapstructure:"prometheus_addr" validate:"omitempty,hostname_port"`
}

func LoadFromFile(filename string) (*Config, error) {
	v := viper.New()

	setDefaults(v)

	v.SetConfigFile(filename)
	v.SetConfigType("toml")

	v.SetEnvPrefix("SYNC4LOONG")
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config
	if err := v.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	validate := validator.New(validator.WithRequiredStructEnabled())
	if err := validate.Struct(&config); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return &config, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("redis.addr", "localhost:6379")
	v.SetDefault("redis.password", "")
	v.SetDefault("redis.db", 0)

	v.SetDefault("s3.region", "us-east-1")
	v.SetDefault("s3.max_retries", 3)
	v.SetDefault("s3.retry_delay_seconds", 2)
	v.SetDefault("s3.max_retry_delay_seconds", 30)
	v.SetDefault("s3.file_upload_retry_count", 2)
	v.SetDefault("s3.file_upload_retry_delay_seconds", 2)
	v.SetDefault("s3.file_upload_timeout_seconds", 4*60*60) // 4 hours
	v.SetDefault("s3.enable_integrity_check", true)

	v.SetDefault("daemon.log_level", "info")
	v.SetDefault("daemon.ssh_command", "")
	v.SetDefault("daemon.ssh_debounce_minutes", 5)
	v.SetDefault("daemon.ssh_timeout_minutes", 1)
	v.SetDefault("daemon.enable_ssh_task", false)

	v.SetDefault("publish.max_retry", 3)
	v.SetDefault("publish.timeout_minutes", 60*6) // 6 hours

	v.SetDefault("http.addr", ":8080")

	v.SetDefault("cache.max_concurrent_s3_checks", 10)
	v.SetDefault("cache.allowed_prefixes", []string{"store/"})

	v.SetDefault("asynqmon.enabled", true)
	v.SetDefault("asynqmon.root_path", "/monitoring")
	v.SetDefault("asynqmon.read_only_mode", false)
	v.SetDefault("asynqmon.prometheus_addr", "")
}
