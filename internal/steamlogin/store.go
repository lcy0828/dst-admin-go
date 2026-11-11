package steamlogin

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"dont/shared"
)

type credentials struct {
	Mode     string `json:"mode"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

func (m *Manager) cipher(id string, create bool) (cipher.AEAD, error) {
	path := filepath.Join(m.root, id+".key")
	key, err := os.ReadFile(path)
	// Read the shared key for legacy profiles. New writes use independent keys
	// so recovery never replaces a key needed by another installation.
	if os.IsNotExist(err) && !create {
		key, err = os.ReadFile(filepath.Join(m.root, "key"))
	}
	if os.IsNotExist(err) && create {
		if err = os.MkdirAll(m.root, 0o700); err != nil {
			return nil, err
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		err = shared.WritePrivateFile(path, key)
	}
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("Steam 登录密钥文件无效")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (m *Manager) load(id string) (credentials, error) {
	data, err := os.ReadFile(filepath.Join(m.root, id+".json"))
	if os.IsNotExist(err) {
		return credentials{Mode: "anonymous"}, nil
	}
	if err != nil {
		return credentials{}, err
	}
	aead, err := m.cipher(id, false)
	if err != nil {
		return credentials{}, err
	}
	if len(data) < aead.NonceSize() {
		return credentials{}, errors.New("Steam 登录配置无效")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte(id))
	if err != nil {
		return credentials{}, errors.New("Steam 登录配置解密失败")
	}
	var value credentials
	if err = json.Unmarshal(plain, &value); err != nil {
		return credentials{}, err
	}
	if value.Mode != "anonymous" && value.Mode != "account" {
		return credentials{}, ErrInvalid
	}
	if value.Mode == "account" && Validate(shared.SteamLoginRequest{
		Action:               "configure",
		Mode:                 value.Mode,
		Username:             value.Username,
		Password:             value.Password,
		UseCachedCredentials: value.Password == "",
	}) != nil {
		return credentials{}, ErrInvalid
	}
	return value, nil
}

func (m *Manager) save(id string, value credentials) error {
	aead, err := m.cipher(id, true)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(value)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	return shared.WritePrivateFile(filepath.Join(m.root, id+".json"), aead.Seal(nonce, nonce, plain, []byte(id)))
}

func (m *Manager) reset(id string) error {
	for _, suffix := range []string{".json", ".key"} {
		if err := os.Remove(filepath.Join(m.root, id+suffix)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
