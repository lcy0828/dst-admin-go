package shared

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"

	"golang.org/x/crypto/nacl/box"
)

// KeyPair 表示加密通信的密钥对
type KeyPair struct {
	PublicKey  [32]byte
	PrivateKey [32]byte
}

// GenerateKeyPair 生成新的密钥对
func GenerateKeyPair() (*KeyPair, error) {
	publicKey, privateKey, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &KeyPair{
		PublicKey:  *publicKey,
		PrivateKey: *privateKey,
	}, nil
}

// Encrypt 使用对方的公钥加密数据
func Encrypt(message []byte, recipientPublicKey, senderPrivateKey [32]byte) ([]byte, error) {
	var nonce [24]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return nil, err
	}

	encrypted := box.Seal(nonce[:], message, &nonce, &recipientPublicKey, &senderPrivateKey)
	return encrypted, nil
}

// Decrypt 使用自己的私钥解密数据
func Decrypt(encrypted []byte, senderPublicKey, recipientPrivateKey [32]byte) ([]byte, error) {
	if len(encrypted) < 24 {
		return nil, errors.New("密文太短")
	}
	var nonce [24]byte
	copy(nonce[:], encrypted[:24])
	decrypted, ok := box.Open(nil, encrypted[24:], &nonce, &senderPublicKey, &recipientPrivateKey)
	if !ok {
		return nil, errors.New("解密失败")
	}
	return decrypted, nil
}

// EncodePublicKey 将公钥编码为Base64字符串
func EncodePublicKey(key [32]byte) string {
	return base64.StdEncoding.EncodeToString(key[:])
}

// DecodePublicKey 从Base64字符串解码公钥
func DecodePublicKey(encoded string) ([32]byte, error) {
	var key [32]byte
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return key, err
	}
	if len(decoded) != 32 {
		return key, errors.New("无效的公钥长度")
	}
	copy(key[:], decoded)
	return key, nil
} 