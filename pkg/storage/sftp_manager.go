package storage

import (
	"fmt"
	"sync"
	"sync/atomic"
	"sync4loong/pkg/logger"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SFTPConnection defines the interface for SFTP connections
type SFTPConnection interface {
	GetClient() *sftp.Client
	GetReconnectCount() uint64
	Close() error
}

// SFTPManager is an interface for managing SFTP connections with automatic reconnection
type SFTPManager interface {
	NewClient() (SFTPConnection, error)
	GetConnection() (SFTPConnection, error)
	Close() error
}

// SFTPConn is a wrapped *sftp.Client with reconnection capabilities
type SFTPConn struct {
	sync.Mutex
	sshConn    *ssh.Client
	sftpClient *sftp.Client
	shutdown   chan bool
	closed     bool
	reconnects uint64
}

// NewSFTPConn creates an SFTPConn, mostly used for testing and should not really be used otherwise.
func NewSFTPConn(client *ssh.Client, sftpClient *sftp.Client) *SFTPConn {
	return &SFTPConn{
		sshConn:    client,
		sftpClient: sftpClient,
		shutdown:   make(chan bool, 1),
		closed:     false,
		reconnects: 0,
	}
}

// GetClient returns the underlying *sftp.Client
func (s *SFTPConn) GetClient() *sftp.Client {
	s.Lock()
	defer s.Unlock()
	return s.sftpClient
}

// GetReconnectCount returns the number of times this connection has reconnected
func (s *SFTPConn) GetReconnectCount() uint64 {
	return atomic.LoadUint64(&s.reconnects)
}

// Close closes the underlying connections
func (s *SFTPConn) Close() error {
	s.Lock()
	defer s.Unlock()
	if s.closed == true {
		return fmt.Errorf("connection was already closed")
	}

	s.shutdown <- true
	s.closed = true
	if s.sshConn != nil {
		s.sshConn.Close()
		return s.sshConn.Wait()
	}
	return nil
}

// BasicSFTPManager implements SFTPManager and supports basic reconnection on disconnect
// for SFTPConn returned by NewClient
type BasicSFTPManager struct {
	conn      *SFTPConn
	config    *SFTPConfig
	connMutex sync.Mutex
}

// NewBasicSFTPManager returns a BasicSFTPManager
func NewBasicSFTPManager(config *SFTPConfig) *BasicSFTPManager {
	manager := &BasicSFTPManager{
		config: config,
	}
	return manager
}

// createSSHConfig creates SSH client configuration from SFTPConfig
func (m *BasicSFTPManager) createSSHConfig() (*ssh.ClientConfig, error) {
	sshConfig := &ssh.ClientConfig{
		User:            m.config.Username,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Duration(m.config.ConnectionTimeout) * time.Second,
	}

	// Set authentication method
	if m.config.PrivateKey != "" {
		// Use private key authentication
		key, err := ssh.ParsePrivateKey([]byte(m.config.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("parse private key: %w", err)
		}
		sshConfig.Auth = []ssh.AuthMethod{ssh.PublicKeys(key)}
	} else if m.config.Password != "" {
		// Use password authentication
		sshConfig.Auth = []ssh.AuthMethod{ssh.Password(m.config.Password)}
	} else {
		return nil, fmt.Errorf("either password or private key must be provided")
	}

	return sshConfig, nil
}

// connectSSH establishes SSH and SFTP connections
func (m *BasicSFTPManager) connectSSH() (*ssh.Client, *sftp.Client, error) {
	sshConfig, err := m.createSSHConfig()
	if err != nil {
		return nil, nil, err
	}

	addr := fmt.Sprintf("%s:%d", m.config.Host, m.config.Port)
	conn, err := ssh.Dial("tcp", addr, sshConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to dial ssh: %w", err)
	}

	sftpConn, err := sftp.NewClient(conn)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("failed to initialize sftp subsystem: %w", err)
	}

	return conn, sftpConn, nil
}

// handleReconnects manages automatic reconnection for a connection
func (m *BasicSFTPManager) handleReconnects(c *SFTPConn) {
	closed := make(chan error, 1)
	go func() {
		closed <- c.sshConn.Wait()
	}()

	select {
	case <-c.shutdown:
		if c.sshConn != nil {
			c.sshConn.Close()
		}
		return
	case res := <-closed:
		logger.Warn("SFTP connection closed, reconnecting", map[string]any{
			"error": res.Error(),
			"host":  m.config.Host,
			"port":  m.config.Port,
		})

		// Attempt to reconnect
		conn, sftpConn, err := m.connectSSH()
		if err != nil {
			logger.Error("failed to reconnect SFTP", err, map[string]any{
				"host": m.config.Host,
				"port": m.config.Port,
			})
			// In a production system, you might want to implement exponential backoff
			// and retry logic here instead of panicking
			panic("Failed to reconnect: " + err.Error())
		}

		atomic.AddUint64(&c.reconnects, 1)
		c.Lock()
		c.sftpClient = sftpConn
		c.sshConn = conn
		c.Unlock()

		logger.Info("SFTP connection reconnected successfully", map[string]any{
			"host":            m.config.Host,
			"port":            m.config.Port,
			"reconnect_count": c.GetReconnectCount(),
		})

		// Continue monitoring the new connection
		m.handleReconnects(c)
	}
}

// NewClient returns an SFTPConn and ensures the underlying connection reconnects on failure
func (m *BasicSFTPManager) NewClient() (SFTPConnection, error) {
	m.connMutex.Lock()
	defer m.connMutex.Unlock()

	// Close existing connection if any
	if m.conn != nil {
		m.conn.Close()
	}

	conn, sftpConn, err := m.connectSSH()
	if err != nil {
		return nil, err
	}

	wrapped := &SFTPConn{
		sshConn:    conn,
		sftpClient: sftpConn,
		shutdown:   make(chan bool, 1),
		closed:     false,
		reconnects: 0,
	}

	go m.handleReconnects(wrapped)
	m.conn = wrapped

	return wrapped, nil
}

// GetConnection returns the existing connection the manager knows about.
// If there is no connection, we create a new one instead.
func (m *BasicSFTPManager) GetConnection() (SFTPConnection, error) {
	m.connMutex.Lock()
	if m.conn != nil {
		defer m.connMutex.Unlock()
		return m.conn, nil
	}
	m.connMutex.Unlock()

	return m.NewClient()
}

// Close closes the connection managed by this manager
func (m *BasicSFTPManager) Close() error {
	m.connMutex.Lock()
	defer m.connMutex.Unlock()

	if m.conn != nil {
		err := m.conn.Close()
		m.conn = nil
		return err
	}
	return nil
}
