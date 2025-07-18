package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// mockFileInfo implements os.FileInfo for testing.
type mockFileInfo struct {
	name    string
	size    int64
	modTime time.Time
}

func (m *mockFileInfo) Name() string       { return m.name }
func (m *mockFileInfo) Size() int64        { return m.size }
func (m *mockFileInfo) Mode() os.FileMode  { return 0 }
func (m *mockFileInfo) ModTime() time.Time { return m.modTime }
func (m *mockFileInfo) IsDir() bool        { return false }
func (m *mockFileInfo) Sys() interface{}   { return nil }

// mockSftpClient is a mock implementation of sftpClientInterface.
type mockSftpClient struct {
	mock.Mock
}

func (m *mockSftpClient) Stat(p string) (os.FileInfo, error) {
	args := m.Called(p)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(os.FileInfo), args.Error(1)
}

func (m *mockSftpClient) MkdirAll(path string) error {
	args := m.Called(path)
	return args.Error(0)
}

func (m *mockSftpClient) Create(path string) (io.WriteCloser, error) {
	args := m.Called(path)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(io.WriteCloser), args.Error(1)
}

func (m *mockSftpClient) OpenFile(path string, f int) (io.WriteCloser, error) {
	args := m.Called(path, f)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(io.WriteCloser), args.Error(1)
}

func (m *mockSftpClient) Rename(oldname, newname string) error {
	args := m.Called(oldname, newname)
	return args.Error(0)
}

func (m *mockSftpClient) Close() error {
	args := m.Called()
	return args.Error(0)
}

// mockWriteCloser is a mock implementation of io.WriteCloser.
type mockWriteCloser struct {
	io.Writer
}

func (m *mockWriteCloser) Close() error { return nil }

// createTempFile creates a temporary file with the given content for testing.
func createTempFile(t *testing.T, content string) (string, func()) {
	t.Helper()
	file, err := os.CreateTemp("", "sftpbackend-test-")
	assert.NoError(t, err)
	_, err = file.WriteString(content)
	assert.NoError(t, err)
	assert.NoError(t, file.Close())
	return file.Name(), func() { os.Remove(file.Name()) }
}

func TestUploadFileWithResume(t *testing.T) {
	key := "test/file.txt"
	finalPath := key
	content := "hello world, this is a test"
	fileSize := int64(len(content))
	hash, _ := calculateTestFileHash(content)
	tempKey := fmt.Sprintf(".%s.%s", filepath.Base(key), hash)
	tempPath := filepath.Join(filepath.Dir(key), tempKey)

	localFile, cleanup := createTempFile(t, content)
	defer cleanup()

	tests := []struct {
		name          string
		enableResume  bool
		setupMocks    func(*mockSftpClient)
		expectedError string
	}{
		{
			name:         "Full upload successfully",
			enableResume: true,
			setupMocks: func(m *mockSftpClient) {
				m.On("Stat", tempPath).Return(nil, os.ErrNotExist).Once()
				m.On("MkdirAll", filepath.Dir(tempPath)).Return(nil).Once()
				m.On("Create", tempPath).Return(&mockWriteCloser{io.Discard}, nil).Once()
				m.On("MkdirAll", filepath.Dir(finalPath)).Return(nil).Once()
				m.On("Rename", tempPath, finalPath).Return(nil).Once()
			},
		},
		{
			name:         "Resume upload successfully",
			enableResume: true,
			setupMocks: func(m *mockSftpClient) {
				remoteSize := int64(10)
				m.On("Stat", tempPath).Return(&mockFileInfo{size: remoteSize}, nil).Once()
				m.On("MkdirAll", filepath.Dir(tempPath)).Return(nil).Once()
				m.On("OpenFile", tempPath, os.O_WRONLY|os.O_APPEND).Return(&mockWriteCloser{io.Discard}, nil).Once()
				m.On("MkdirAll", filepath.Dir(finalPath)).Return(nil).Once()
				m.On("Rename", tempPath, finalPath).Return(nil).Once()
			},
		},
		{
			name:         "Skip upload when temp file is complete",
			enableResume: true,
			setupMocks: func(m *mockSftpClient) {
				m.On("Stat", tempPath).Return(&mockFileInfo{size: fileSize}, nil).Once()
				m.On("MkdirAll", filepath.Dir(finalPath)).Return(nil).Once()
				m.On("Rename", tempPath, finalPath).Return(nil).Once()
			},
		},
		{
			name:         "Upload without resume enabled",
			enableResume: false,
			setupMocks: func(m *mockSftpClient) {
				m.On("Stat", tempPath).Return(nil, os.ErrNotExist).Once()
				m.On("MkdirAll", filepath.Dir(tempPath)).Return(nil).Once()
				m.On("Create", tempPath).Return(&mockWriteCloser{io.Discard}, nil).Once()
				m.On("MkdirAll", filepath.Dir(finalPath)).Return(nil).Once()
				m.On("Rename", tempPath, finalPath).Return(nil).Once()
			},
		},
		{
			name:         "Stat returns an error",
			enableResume: true,
			setupMocks: func(m *mockSftpClient) {
				m.On("Stat", tempPath).Return(nil, assert.AnError).Once()
			},
			expectedError: "get remote file size: stat remote file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &mockSftpClient{}
			backend := &SFTPBackend{
				client: mockClient,
				config: &SFTPConfig{
					EnableResume: tt.enableResume,
				},
				locks: newStripedLock(10),
			}

			tt.setupMocks(mockClient)

			err := backend.UploadFile(context.Background(), localFile, key, nil)

			if tt.expectedError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectedError)
			} else {
				assert.NoError(t, err)
			}

			mockClient.AssertExpectations(t)
		})
	}
}

func calculateTestFileHash(content string) (string, error) {
	h := xxhash.New()
	if _, err := io.Copy(h, strings.NewReader(content)); err != nil {
		return "", err
	}
	return fmt.Sprintf("%016x", h.Sum64()), nil
}
