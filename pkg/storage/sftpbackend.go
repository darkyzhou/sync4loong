package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// sftpClientInterface defines the methods we use from the sftp.Client.
// This makes the SFTPBackend testable.
type sftpClientInterface interface {
	Stat(p string) (os.FileInfo, error)
	MkdirAll(path string) error
	Create(path string) (io.WriteCloser, error)
	OpenFile(path string, f int) (io.WriteCloser, error)
	Rename(oldname, newname string) error
	Close() error
}

// sshClientInterface defines the methods we use from the ssh.Client.
type sshClientInterface interface {
	Close() error
}

// sftpClientAdapter wraps *sftp.Client to satisfy sftpClientInterface.
type sftpClientAdapter struct {
	*sftp.Client
}

func (a *sftpClientAdapter) Create(path string) (io.WriteCloser, error) {
	return a.Client.Create(path)
}

func (a *sftpClientAdapter) OpenFile(path string, f int) (io.WriteCloser, error) {
	return a.Client.OpenFile(path, f)
}

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
	client    sftpClientInterface
	sshClient sshClientInterface
	config    *SFTPConfig
	locks     *stripedLock
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
	backend := &SFTPBackend{
		config: config,
		locks:  newStripedLock(1024),
	}

	if err := backend.connect(); err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}

	return backend, nil
}

func (s *SFTPBackend) connect() error {
	// Create SSH client configuration
	sshConfig := &ssh.ClientConfig{
		User:            s.config.Username,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Duration(s.config.ConnectionTimeout) * time.Second,
	}

	// Set authentication method
	if s.config.PrivateKey != "" {
		// Use private key authentication
		key, err := ssh.ParsePrivateKey([]byte(s.config.PrivateKey))
		if err != nil {
			return fmt.Errorf("parse private key: %w", err)
		}
		sshConfig.Auth = []ssh.AuthMethod{ssh.PublicKeys(key)}
	} else if s.config.Password != "" {
		// Use password authentication
		sshConfig.Auth = []ssh.AuthMethod{ssh.Password(s.config.Password)}
	} else {
		return fmt.Errorf("either password or private key must be provided")
	}

	// Connect to SSH server
	addr := fmt.Sprintf("%s:%d", s.config.Host, s.config.Port)
	sshClient, err := ssh.Dial("tcp", addr, sshConfig)
	if err != nil {
		return fmt.Errorf("connect to SSH server: %w", err)
	}

	// Create SFTP client
	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		_ = sshClient.Close()
		return fmt.Errorf("create SFTP client: %w", err)
	}

	s.sshClient = sshClient
	s.client = &sftpClientAdapter{sftpClient}

	return nil
}

func (s *SFTPBackend) GetBackendType() BackendType {
	return BackendTypeSFTP
}

func (s *SFTPBackend) GetCacheIdentifier() string {
	return fmt.Sprintf("%s:%d", s.config.Host, s.config.Port)
}

func (s *SFTPBackend) Close() error {
	if s.client != nil {
		_ = s.client.Close()
	}
	if s.sshClient != nil {
		_ = s.sshClient.Close()
	}
	return nil
}

func (s *SFTPBackend) CheckFileExists(ctx context.Context, key string) (*FileMetadata, error) {
	remotePath := filepath.Clean(key)
	stat, err := s.client.Stat(remotePath)
	if err != nil {
		if os.IsNotExist(err) {
			return &FileMetadata{Exists: false}, nil
		}
		return nil, fmt.Errorf("stat remote file: %w", err)
	}

	return &FileMetadata{
		Size:         stat.Size(),
		LastModified: stat.ModTime(),
	}, nil
}

func (s *SFTPBackend) UploadFile(ctx context.Context, filePath, key string, opts *UploadOptions) error {
	return s.uploadFileWithResume(ctx, filePath, key, opts)
}

// uploadFileWithResume handles file upload with resumable support
func (s *SFTPBackend) uploadFileWithResume(ctx context.Context, filePath, key string, opts *UploadOptions) error {
	// 1. Acquire file lock
	s.lockFile(key)
	defer s.unlockFile(key)

	// 2. Calculate file hash
	fileHash, err := s.calculateFileHash(filePath)
	if err != nil {
		return fmt.Errorf("calculate file hash: %w", err)
	}

	// 3. Generate temporary file name
	tempKey := s.generateTempKey(key, fileHash)

	// 4. Get local file size
	localSize, err := s.getLocalFileSize(filePath)
	if err != nil {
		return fmt.Errorf("get local file size: %w", err)
	}

	// 5. Get remote file size
	remoteSize, exists, err := s.getRemoteFileSize(tempKey)
	if err != nil {
		return fmt.Errorf("get remote file size: %w", err)
	}

	// 6. Determine if resumable upload is needed
	startOffset := int64(0)
	skipUpload := false
	if s.config.EnableResume && exists {
		if remoteSize == localSize {
			// Temporary file is already complete, skip upload
			skipUpload = true
		} else if remoteSize > 0 && remoteSize < localSize {
			// Resume upload from offset
			startOffset = remoteSize
		}
	}

	// 7. Execute upload if not skipped
	if !skipUpload {
		if err := s.uploadWithResume(ctx, filePath, tempKey, startOffset); err != nil {
			return fmt.Errorf("upload with resume: %w", err)
		}
	}

	// 8. Atomic rename
	if err := s.renameFile(tempKey, key); err != nil {
		return fmt.Errorf("rename file: %w", err)
	}

	return nil
}

func (s *SFTPBackend) UploadFromReader(ctx context.Context, reader io.Reader, key string, opts *UploadOptions) error {
	remotePath := filepath.Clean(key)

	s.lockFile(remotePath)
	defer s.unlockFile(remotePath)

	// Ensure remote directory exists
	if err := s.client.MkdirAll(filepath.Dir(remotePath)); err != nil {
		return fmt.Errorf("create remote directory: %w", err)
	}

	// Create remote file
	remoteFile, err := s.client.Create(remotePath)
	if err != nil {
		return fmt.Errorf("create remote file: %w", err)
	}
	defer func() { _ = remoteFile.Close() }()

	// Copy data from reader to remote file
	if _, err := copyWithContext(ctx, remoteFile, reader); err != nil {
		return fmt.Errorf("copy data: %w", err)
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
	remotePath := filepath.Clean(remoteKey)

	stat, err := s.client.Stat(remotePath)
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
	remotePath := filepath.Clean(tempKey)

	// Ensure remote directory exists
	if err := s.client.MkdirAll(filepath.Dir(remotePath)); err != nil {
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
		remoteFile, err = s.client.OpenFile(remotePath, os.O_WRONLY|os.O_APPEND)
	} else {
		// Create new file
		remoteFile, err = s.client.Create(remotePath)
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
	tempPath := filepath.Clean(tempKey)
	finalPath := filepath.Clean(finalKey)

	// Ensure final directory exists
	if err := s.client.MkdirAll(filepath.Dir(finalPath)); err != nil {
		return fmt.Errorf("create final directory: %w", err)
	}

	// Rename temp file to final file
	if err := s.client.Rename(tempPath, finalPath); err != nil {
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
