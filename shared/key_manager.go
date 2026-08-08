package shared

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-ini/ini"
)

type SecurityKey struct {
	Key string `json:"key"`
}

type KeyManagerInterface interface {
	GetKey() string
	SetKey(string) error
	ValidateKey(string) bool
	GenerateNewKey() error
	SetKeyChangedCallback(func(string))
	StopWatching()
}

type KeyManager struct {
	keyFile       string
	securityKey   SecurityKey
	mutex         sync.RWMutex
	lastModified  time.Time
	stopWatchChan chan struct{}
	stopOnce      sync.Once
	watchInterval time.Duration
	keyChangedCb  func(string)
}

func NewKeyManager(keyFile string) (*KeyManager, error) {
	keyFile = strings.TrimSpace(keyFile)
	if keyFile == "" {
		return nil, errors.New("密钥文件路径不能为空")
	}
	manager := &KeyManager{
		keyFile:       keyFile,
		stopWatchChan: make(chan struct{}),
		watchInterval: 10 * time.Second,
	}
	if err := manager.loadKey(); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		if err := manager.GenerateNewKey(); err != nil {
			return nil, err
		}
	}
	go manager.watchKeyFile()
	return manager, nil
}

func (manager *KeyManager) SetKeyChangedCallback(callback func(string)) {
	manager.mutex.Lock()
	manager.keyChangedCb = callback
	manager.mutex.Unlock()
}

func (manager *KeyManager) watchKeyFile() {
	ticker := time.NewTicker(manager.watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-manager.stopWatchChan:
			return
		case <-ticker.C:
			info, err := os.Stat(manager.keyFile)
			if err != nil {
				log.Printf("监控密钥文件失败: %v", err)
				continue
			}
			manager.mutex.RLock()
			changed := info.ModTime().After(manager.lastModified)
			manager.mutex.RUnlock()
			if !changed {
				continue
			}
			oldKey := manager.GetKey()
			if err := manager.loadKey(); err != nil {
				log.Printf("重新加载密钥失败: %v", err)
				continue
			}
			newKey := manager.GetKey()
			if secureKeyEqual(oldKey, newKey) {
				continue
			}
			log.Printf("检测到密钥文件变更，已重新加载")
			manager.notifyChanged(newKey)
		}
	}
}

func (manager *KeyManager) StopWatching() {
	manager.stopOnce.Do(func() { close(manager.stopWatchChan) })
}

func (manager *KeyManager) loadKey() error {
	path := manager.keyFile
	if !filepath.IsAbs(path) && filepath.Base(path) == path {
		if workingDirectory, err := os.Getwd(); err == nil {
			path = filepath.Join(workingDirectory, path)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("指定的密钥文件路径是目录: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取密钥文件失败: %w", err)
	}
	var value SecurityKey
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("解析密钥文件失败: %w", err)
	}
	if err := validateSecurityKey(value.Key); err != nil {
		return err
	}
	if err := EnsurePrivateFile(path); err != nil {
		return err
	}
	manager.mutex.Lock()
	manager.keyFile = path
	manager.securityKey = value
	manager.lastModified = info.ModTime()
	manager.mutex.Unlock()
	log.Printf("密钥文件已加载: %s", path)
	return nil
}

func (manager *KeyManager) GenerateNewKey() error {
	key, err := generateSecurityKey()
	if err != nil {
		return err
	}
	return manager.replaceKey(key, true)
}

func (manager *KeyManager) GetKey() string {
	manager.mutex.RLock()
	defer manager.mutex.RUnlock()
	return manager.securityKey.Key
}

func (manager *KeyManager) SetKey(newKey string) error {
	if err := validateSecurityKey(newKey); err != nil {
		return err
	}
	if secureKeyEqual(manager.GetKey(), newKey) {
		return nil
	}
	if err := manager.replaceKey(newKey, true); err != nil {
		return err
	}
	return nil
}

func (manager *KeyManager) replaceKey(newKey string, persist bool) error {
	manager.mutex.Lock()
	oldKey := manager.securityKey.Key
	manager.securityKey.Key = newKey
	path := manager.keyFile
	manager.mutex.Unlock()

	if persist {
		data, err := json.MarshalIndent(SecurityKey{Key: newKey}, "", "  ")
		if err != nil {
			return fmt.Errorf("序列化密钥失败: %w", err)
		}
		if err := WritePrivateFile(path, data); err != nil {
			manager.mutex.Lock()
			manager.securityKey.Key = oldKey
			manager.mutex.Unlock()
			return err
		}
	}
	if info, err := os.Stat(path); err == nil {
		manager.mutex.Lock()
		manager.lastModified = info.ModTime()
		manager.mutex.Unlock()
	}
	log.Printf("通信密钥已安全保存")
	return nil
}

func (manager *KeyManager) ValidateKey(key string) bool {
	return secureKeyEqual(manager.GetKey(), key)
}

func (manager *KeyManager) notifyChanged(key string) {
	manager.mutex.RLock()
	callback := manager.keyChangedCb
	manager.mutex.RUnlock()
	if callback != nil {
		callback(key)
	}
}

type ConfigKeyManager struct {
	KeyManager
	configFile string
	section    string
	keyName    string
}

func NewKeyManagerWithConfig(configFile, section, keyName string) (*ConfigKeyManager, error) {
	configFile = strings.TrimSpace(configFile)
	section = strings.TrimSpace(section)
	keyName = strings.TrimSpace(keyName)
	if configFile == "" || section == "" || keyName == "" {
		return nil, errors.New("配置文件、配置段和密钥名不能为空")
	}
	manager := &ConfigKeyManager{
		KeyManager: KeyManager{
			stopWatchChan: make(chan struct{}),
			watchInterval: 10 * time.Second,
		},
		configFile: configFile,
		section:    section,
		keyName:    keyName,
	}
	if err := manager.loadKeyFromConfig(); err != nil {
		if !os.IsNotExist(err) && !errors.Is(err, errConfigKeyMissing) {
			return nil, err
		}
		if err := manager.GenerateNewKeyToConfig(); err != nil {
			return nil, err
		}
	}
	go manager.watchConfigFile()
	return manager, nil
}

var errConfigKeyMissing = errors.New("配置文件中未找到有效密钥")

func (manager *ConfigKeyManager) loadKeyFromConfig() error {
	info, err := os.Stat(manager.configFile)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("指定的配置文件路径是目录: %s", manager.configFile)
	}
	config, err := ini.Load(manager.configFile)
	if err != nil {
		return fmt.Errorf("读取配置文件失败: %w", err)
	}
	key := strings.TrimSpace(config.Section(manager.section).Key(manager.keyName).String())
	if key == "" {
		return errConfigKeyMissing
	}
	if err := validateSecurityKey(key); err != nil {
		return err
	}
	if err := EnsurePrivateFile(manager.configFile); err != nil {
		return err
	}
	manager.mutex.Lock()
	manager.securityKey.Key = key
	manager.lastModified = info.ModTime()
	manager.mutex.Unlock()
	log.Printf("通信密钥已从配置文件加载: %s", manager.configFile)
	return nil
}

func (manager *ConfigKeyManager) saveKeyToConfig(key string) error {
	var config *ini.File
	if _, err := os.Stat(manager.configFile); err == nil {
		loaded, loadErr := ini.Load(manager.configFile)
		if loadErr != nil {
			return fmt.Errorf("读取配置文件失败: %w", loadErr)
		}
		config = loaded
	} else if os.IsNotExist(err) {
		config = ini.Empty()
	} else {
		return fmt.Errorf("检查配置文件失败: %w", err)
	}
	section, err := config.GetSection(manager.section)
	if err != nil {
		section, err = config.NewSection(manager.section)
		if err != nil {
			return fmt.Errorf("创建配置段失败: %w", err)
		}
	}
	section.Key(manager.keyName).SetValue(key)
	var output bytes.Buffer
	if _, err := config.WriteTo(&output); err != nil {
		return fmt.Errorf("序列化配置文件失败: %w", err)
	}
	return WritePrivateFile(manager.configFile, output.Bytes())
}

func (manager *ConfigKeyManager) watchConfigFile() {
	ticker := time.NewTicker(manager.watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-manager.stopWatchChan:
			return
		case <-ticker.C:
			info, err := os.Stat(manager.configFile)
			if err != nil {
				log.Printf("监控配置文件失败: %v", err)
				continue
			}
			manager.mutex.RLock()
			changed := info.ModTime().After(manager.lastModified)
			manager.mutex.RUnlock()
			if !changed {
				continue
			}
			oldKey := manager.GetKey()
			if err := manager.loadKeyFromConfig(); err != nil {
				log.Printf("重新加载通信密钥失败: %v", err)
				continue
			}
			newKey := manager.GetKey()
			if secureKeyEqual(oldKey, newKey) {
				continue
			}
			log.Printf("检测到配置文件中的通信密钥变更")
			manager.notifyChanged(newKey)
		}
	}
}

func (manager *ConfigKeyManager) GenerateNewKeyToConfig() error {
	key, err := generateSecurityKey()
	if err != nil {
		return err
	}
	return manager.persistKey(key, false)
}

func (manager *ConfigKeyManager) GetKey() string {
	return manager.KeyManager.GetKey()
}

func (manager *ConfigKeyManager) SetKey(newKey string) error {
	if err := validateSecurityKey(newKey); err != nil {
		return err
	}
	if secureKeyEqual(manager.GetKey(), newKey) {
		return nil
	}
	return manager.persistKey(newKey, false)
}

func (manager *ConfigKeyManager) persistKey(newKey string, notify bool) error {
	manager.mutex.Lock()
	oldKey := manager.securityKey.Key
	manager.securityKey.Key = newKey
	manager.mutex.Unlock()
	if err := manager.saveKeyToConfig(newKey); err != nil {
		manager.mutex.Lock()
		manager.securityKey.Key = oldKey
		manager.mutex.Unlock()
		return err
	}
	if info, err := os.Stat(manager.configFile); err == nil {
		manager.mutex.Lock()
		manager.lastModified = info.ModTime()
		manager.mutex.Unlock()
	}
	log.Printf("通信密钥已安全保存到配置文件")
	if notify {
		manager.notifyChanged(newKey)
	}
	return nil
}

func (manager *ConfigKeyManager) ValidateKey(key string) bool {
	return secureKeyEqual(manager.GetKey(), key)
}

func (manager *ConfigKeyManager) StopWatching() {
	manager.KeyManager.StopWatching()
}

func (manager *ConfigKeyManager) GenerateNewKey() error {
	return manager.GenerateNewKeyToConfig()
}

func generateSecurityKey() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("生成通信密钥失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func validateSecurityKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("密钥不能为空")
	}
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(decoded) < 32 {
		return errors.New("无效的密钥格式，必须是至少 32 字节的 Base64 字符串")
	}
	return nil
}

// ValidateSecurityKey checks the shared Agent key format without exposing the
// key or including it in an error message.
func ValidateSecurityKey(key string) error {
	return validateSecurityKey(key)
}

func secureKeyEqual(expected, actual string) bool {
	if expected == "" || len(expected) != len(actual) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}
