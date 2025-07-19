package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync4loong/pkg/logger"

	"github.com/cespare/xxhash/v2"
	"github.com/pkg/sftp"
)

// stripedLock provides a set of locks for concurrent access to different keys.
// This avoids holding a global lock or having a map of locks that grows indefinitely.
type stripedLock struct {
	locks []sync.Mutex
}

// newStripedLock creates a new stripedLock with a given number of locks.
func newStripedLock(count int) *stripedLock {
	if count <= 0 {
		count = 1024 // Default to a reasonable number of locks
	}
	return &stripedLock{
		locks: make([]sync.Mutex, count),
	}
}

// Lock locks the mutex associated with the given key.
func (sl *stripedLock) Lock(key string) {
	h := xxhash.Sum64String(key)
	sl.locks[h%uint64(len(sl.locks))].Lock()
}

// Unlock unlocks the mutex associated with the given key.
func (sl *stripedLock) Unlock(key string) {
	h := xxhash.Sum64String(key)
	sl.locks[h%uint64(len(sl.locks))].Unlock()
}

type SFTPBackend struct {
	manager SFTPManager
	config  *SFTPConfig
	locks   *stripedLock
}

type SFTPConfig struct {
	Host              string `mapstructure:"host" validate:"required"`
	Port              int    `mapstructure:"port" validate:"required,min=1,max=65535"`
	Username          string `mapstructure:"username" validate:"required"`
	Password          string `mapstructure:"password"`
	PrivateKey        string `mapstructure:"private_key"`
	ConnectionTimeout int    `mapstructure:"connection_timeout" validate:"min=1,max=300"`
	EnableResume      bool   `mapstructure:"enable_resume"`
}

func NewSFTPBackend(config *SFTPConfig) (*SFTPBackend, error) {
	manager := NewBasicSFTPManager(config)

	backend := &SFTPBackend{
		manager: manager,
		config:  config,
		locks:   newStripedLock(1024),
	}

	logger.Info("SFTP backend initialized with connection manager", nil)
	return backend, nil
}

// getClient returns the SFTP client from the manager, creating a connection if necessary
func (s *SFTPBackend) getClient() (*sftp.Client, error) {
	conn, err := s.manager.GetConnection()
	if err != nil {
		return nil, fmt.Errorf("get SFTP connection: %w", err)
	}
	return conn.GetClient(), nil
}

func (s *SFTPBackend) GetBackendType() BackendType {
	return BackendTypeSFTP
}

func (s *SFTPBackend) GetCacheIdentifier() string {
	return fmt.Sprintf("%s:%d", s.config.Host, s.config.Port)
}

func (s *SFTPBackend) Close() error {
	if s.manager != nil {
		return s.manager.Close()
	}
	return nil
}

func (s *SFTPBackend) CheckFileExists(ctx context.Context, key string) (*FileMetadata, error) {
	client, err := s.getClient()
	if err != nil {
		return nil, fmt.Errorf("get client: %w", err)
	}

	remotePath := filepath.Clean(key)
	stat, err := client.Stat(remotePath)
	if err != nil {
		if os.IsNotExist(err) {
			return &FileMetadata{Exists: false}, nil
		}
		return nil, fmt.Errorf("stat remote file: %w", err)
	}

	return &FileMetadata{
		Exists:       true,
		Size:         stat.Size(),
		LastModified: stat.ModTime(),
	}, nil
}

func (s *SFTPBackend) UploadFile(ctx context.Context, filePath, key string, opts *UploadOptions) error {
	s.lockFile(key)
	defer s.unlockFile(key)

	if opts.Overwrite {
		metadata, err := s.CheckFileExists(ctx, key)
		if err != nil {
			return fmt.Errorf("check file exists: %w", err)
		}

		if metadata.Exists {
			client, err := s.getClient()
			if err != nil {
				return fmt.Errorf("get client: %w", err)
			}
			if err := client.Remove(key); err != nil {
				return fmt.Errorf("remove file: %w", err)
			}

			logger.Info("file exists, removing due to overwrite", map[string]any{"key": key})
		}
	}

	localSize, err := s.getLocalFileSize(filePath)
	if err != nil {
		return fmt.Errorf("get local file size: %w", err)
	}

	fileHash, err := s.calculateFileHash(filePath)
	if err != nil {
		return fmt.Errorf("calculate file hash: %w", err)
	}
	tempKey := s.generateTempKey(key, fileHash)
	remoteSize, exists, err := s.getRemoteFileSize(tempKey)
	if err != nil {
		return fmt.Errorf("get remote file size: %w", err)
	}

	startOffset := int64(0)
	skipUpload := false
	if s.config.EnableResume && exists {
		if remoteSize == localSize {
			skipUpload = true
		} else if remoteSize > 0 && remoteSize < localSize {
			startOffset = remoteSize
		}
	}

	if !skipUpload {
		if err := s.uploadWithResume(ctx, filePath, tempKey, startOffset); err != nil {
			return fmt.Errorf("upload with resume: %w", err)
		}
	}

	if err := s.renameFile(tempKey, key); err != nil {
		return fmt.Errorf("rename file: %w", err)
	}
	return nil
}

func (s *SFTPBackend) calculateFileHash(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()

	h := xxhash.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}

	return fmt.Sprintf("%016x", h.Sum64()), nil
}

func (s *SFTPBackend) getLocalFileSize(filePath string) (int64, error) {
	stat, err := os.Stat(filePath)
	if err != nil {
		return 0, err
	}
	return stat.Size(), nil
}

func (s *SFTPBackend) getRemoteFileSize(remoteKey string) (int64, bool, error) {
	client, err := s.getClient()
	if err != nil {
		return 0, false, fmt.Errorf("get client: %w", err)
	}

	remotePath := filepath.Clean(remoteKey)

	stat, err := client.Stat(remotePath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil // File does not exist
		}
		return 0, false, fmt.Errorf("stat remote file: %w", err)
	}

	return stat.Size(), true, nil
}

func (s *SFTPBackend) generateTempKey(key, hash string) string {
	dir := filepath.Dir(key)
	filename := filepath.Base(key)
	tempFilename := fmt.Sprintf(".%s.%s", filename, hash)

	if dir == "." {
		return tempFilename
	}
	return filepath.Join(dir, tempFilename)
}

func (s *SFTPBackend) uploadWithResume(ctx context.Context, filePath, tempKey string, startOffset int64) error {
	client, err := s.getClient()
	if err != nil {
		return fmt.Errorf("get client: %w", err)
	}

	remotePath := filepath.Clean(tempKey)

	// Ensure remote directory exists
	if err := client.MkdirAll(filepath.Dir(remotePath)); err != nil {
		return fmt.Errorf("create remote directory: %w", err)
	}

	// Open local file
	localFile, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open local file: %w", err)
	}
	defer func() { _ = localFile.Close() }()

	// Seek to start offset for resume
	if startOffset > 0 {
		if _, err := localFile.Seek(startOffset, 0); err != nil {
			return fmt.Errorf("seek local file: %w", err)
		}
	}

	// Open remote file for writing
	var remoteFile io.WriteCloser
	if startOffset > 0 {
		// Open for append
		remoteFile, err = client.OpenFile(remotePath, os.O_WRONLY|os.O_APPEND)
	} else {
		// Create new file
		remoteFile, err = client.Create(remotePath)
	}
	if err != nil {
		return fmt.Errorf("open remote file: %w", err)
	}
	defer func() { _ = remoteFile.Close() }()

	// Copy data from local file to remote file
	if _, err := copyWithContext(ctx, remoteFile, localFile); err != nil {
		return fmt.Errorf("copy file data: %w", err)
	}

	return nil
}

func (s *SFTPBackend) renameFile(tempKey, finalKey string) error {
	client, err := s.getClient()
	if err != nil {
		return fmt.Errorf("get client: %w", err)
	}

	tempPath := filepath.Clean(tempKey)
	finalPath := filepath.Clean(finalKey)

	// Ensure final directory exists
	if err := client.MkdirAll(filepath.Dir(finalPath)); err != nil {
		return fmt.Errorf("create final directory: %w", err)
	}

	// Rename temp file to final file
	if err := client.Rename(tempPath, finalPath); err != nil {
		return fmt.Errorf("rename file: %w", err)
	}

	return nil
}

func (s *SFTPBackend) lockFile(key string) {
	s.locks.Lock(key)
}

func (s *SFTPBackend) unlockFile(key string) {
	s.locks.Unlock(key)
}

type readerFunc func(p []byte) (n int, err error)

func (rf readerFunc) Read(p []byte) (n int, err error) { return rf(p) }

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, readerFunc(func(p []byte) (int, error) {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
			return src.Read(p)
		}
	}))
}
