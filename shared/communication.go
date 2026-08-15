package shared

import (
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	MaximumRegistrationMessageBytes int64 = 64 * 1024
	// Runtime artifacts are JSON/base64 encoded inside an encrypted command
	// response. Eight MiB covers the 4 MiB artifact contract plus framing while
	// keeping an authenticated peer from allocating an unbounded message.
	MaximumSecureMessageBytes int64 = 8 * 1024 * 1024
)

// SecureConnection 提供加密的WebSocket连接
type SecureConnection struct {
	conn            *websocket.Conn
	localKeyPair    *KeyPair
	remotePublicKey [32]byte
	writeMutex      sync.Mutex
	isServer        bool
	closed          bool
	closedMutex     sync.Mutex
}

// NewSecureConnection 创建新的安全连接
func NewSecureConnection(conn *websocket.Conn, localKeyPair *KeyPair, isServer bool) *SecureConnection {
	conn.SetReadLimit(MaximumSecureMessageBytes)
	return &SecureConnection{
		conn:         conn,
		localKeyPair: localKeyPair,
		isServer:     isServer,
		closed:       false,
	}
}

// SetRemotePublicKey 设置远程公钥
func (s *SecureConnection) SetRemotePublicKey(publicKey [32]byte) {
	s.remotePublicKey = publicKey
}

// SetTimeout 设置连接的读取超时，传入0表示不超时
func (s *SecureConnection) SetTimeout(timeout time.Duration) {
	if timeout > 0 {
		s.conn.SetReadDeadline(time.Now().Add(timeout))
	} else {
		s.conn.SetReadDeadline(time.Time{}) // 清除超时设置
	}
}

// SendEncrypted 发送加密消息
func (s *SecureConnection) SendEncrypted(msg *Message) error {
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	// 使用共享的加密函数加密数据
	encrypted, err := Encrypt(msgBytes, s.remotePublicKey, s.localKeyPair.PrivateKey)
	if err != nil {
		return err
	}

	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()

	s.closedMutex.Lock()
	if s.closed {
		s.closedMutex.Unlock()
		return errors.New("连接已关闭")
	}
	s.closedMutex.Unlock()

	return s.conn.WriteMessage(websocket.BinaryMessage, encrypted)
}

// ReadEncrypted 读取加密消息
func (s *SecureConnection) ReadEncrypted() (*Message, error) {
	_, msgBytes, err := s.conn.ReadMessage()
	if err != nil {
		return nil, err
	}

	// 解密消息
	decrypted, err := Decrypt(msgBytes, s.remotePublicKey, s.localKeyPair.PrivateKey)
	if err != nil {
		return nil, err
	}

	var msg Message
	if err := json.Unmarshal(decrypted, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// StartHeartbeat 开始发送心跳包
func (s *SecureConnection) StartHeartbeat(agentID string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		s.closedMutex.Lock()
		if s.closed {
			s.closedMutex.Unlock()
			return
		}
		s.closedMutex.Unlock()

		heartbeat, err := CreateMessage(TypeHeartbeat, agentID, nil)
		if err != nil {
			log.Printf("创建心跳消息失败: %v", err)
			continue
		}

		if err := s.SendEncrypted(heartbeat); err != nil {
			log.Printf("发送心跳失败: %v", err)
			return
		}
	}
}

// Close 关闭连接
func (s *SecureConnection) Close() error {
	s.closedMutex.Lock()
	defer s.closedMutex.Unlock()

	if s.closed {
		return nil
	}

	s.closed = true
	return s.conn.Close()
}

// GetRawConnection 获取底层的WebSocket连接
func (s *SecureConnection) GetRawConnection() *websocket.Conn {
	return s.conn
}
