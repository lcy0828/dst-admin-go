package shared

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SecurityKey 代表通信安全密钥
type SecurityKey struct {
	Key string `json:"key"` // Base64编码的安全密钥
}

// KeyManager 密钥管理器
type KeyManager struct {
	keyFile        string
	securityKey    SecurityKey
	mutex          sync.RWMutex
	lastModified   time.Time
	stopWatchChan  chan struct{}
	watchInterval  time.Duration
	keyChangedCb   func(string)
}

// NewKeyManager 创建一个新的密钥管理器
func NewKeyManager(keyFile string) (*KeyManager, error) {
	km := &KeyManager{
		keyFile:       keyFile,
		stopWatchChan: make(chan struct{}),
		watchInterval: 10 * time.Second, // 默认每10秒检查一次文件变化
	}
	
	// 尝试加载现有密钥
	err := km.loadKey()
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		
		// 如果密钥文件不存在，则生成新密钥
		log.Println("通信密钥文件不存在，创建新密钥文件:", keyFile)
		err = km.GenerateNewKey()
		if err != nil {
			return nil, err
		}
	}
	
	// 启动文件监控，热加载密钥
	go km.watchKeyFile()
	
	return km, nil
}

// SetKeyChangedCallback 设置密钥变更回调函数
func (km *KeyManager) SetKeyChangedCallback(cb func(string)) {
	km.mutex.Lock()
	defer km.mutex.Unlock()
	km.keyChangedCb = cb
}

// 监控密钥文件变化
func (km *KeyManager) watchKeyFile() {
	ticker := time.NewTicker(km.watchInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-km.stopWatchChan:
			return
		case <-ticker.C:
			// 检查文件是否被修改
			fileInfo, err := os.Stat(km.keyFile)
			if err != nil {
				log.Printf("监控密钥文件错误: %v", err)
				continue
			}
			
			modTime := fileInfo.ModTime()
			
			km.mutex.RLock()
			lastMod := km.lastModified
			km.mutex.RUnlock()
			
			// 如果文件被修改，重新加载
			if modTime.After(lastMod) {
				log.Println("检测到密钥文件变更，重新加载密钥")
				oldKey := km.GetKey()
				err := km.loadKey()
				if err != nil {
					log.Printf("重新加载密钥失败: %v", err)
				} else {
					newKey := km.GetKey()
					if oldKey != newKey {
						log.Println("密钥已更新")
						// 触发回调
						km.mutex.RLock()
						cb := km.keyChangedCb
						km.mutex.RUnlock()
						if cb != nil {
							cb(newKey)
						}
					}
				}
			}
		}
	}
}

// StopWatching 停止监控密钥文件
func (km *KeyManager) StopWatching() {
	close(km.stopWatchChan)
}

// loadKey 加载密钥文件
func (km *KeyManager) loadKey() error {
	km.mutex.Lock()
	defer km.mutex.Unlock()
	
	// 获取绝对路径
	absPath, err := filepath.Abs(km.keyFile)
	if err != nil {
		log.Printf("无法获取密钥文件的绝对路径: %v，将使用原始路径", err)
		absPath = km.keyFile
	}
	
	log.Printf("尝试加载密钥文件: %s", absPath)
	
	// 检查文件是否存在
	fileInfo, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("密钥文件不存在: %s", absPath)
		} else {
			log.Printf("检查密钥文件状态失败: %v", err)
		}
		return err
	}
	
	if fileInfo.IsDir() {
		err := fmt.Errorf("指定的密钥文件路径是一个目录: %s", absPath)
		log.Print(err)
		return err
	}
	
	// 读取文件内容
	data, err := ioutil.ReadFile(absPath)
	if err != nil {
		log.Printf("读取密钥文件失败: %v", err)
		return err
	}
	
	// 解析JSON
	err = json.Unmarshal(data, &km.securityKey)
	if err != nil {
		log.Printf("解析密钥文件内容失败: %v", err)
		return err
	}
	
	km.lastModified = fileInfo.ModTime()
	log.Printf("密钥文件成功加载: %s", absPath)
	return nil
}

// saveKey 保存密钥到文件
func (km *KeyManager) saveKey() error {
	km.mutex.RLock()
	defer km.mutex.RUnlock()
	
	// 获取绝对路径
	absPath, err := filepath.Abs(km.keyFile)
	if err != nil {
		log.Printf("无法获取密钥文件的绝对路径: %v", err)
		absPath = km.keyFile // 如果获取失败，使用原始路径
	}
	
	log.Printf("尝试保存密钥到文件: %s", absPath)
	
	data, err := json.MarshalIndent(km.securityKey, "", "  ")
	if err != nil {
		log.Printf("序列化密钥数据失败: %v", err)
		return err
	}
	
	// 确保目录存在
	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("创建目录失败: %v", err)
		
		// 尝试回退到当前目录
		currentDir, err := os.Getwd()
		if err != nil {
			log.Printf("无法获取当前工作目录: %v", err)
			return err
		}
		
		// 修改文件路径为当前目录中的文件
		newPath := filepath.Join(currentDir, filepath.Base(absPath))
		log.Printf("尝试使用当前目录作为替代: %s", newPath)
		absPath = newPath
	}
	
	// 尝试先写入临时文件
	tempFile := absPath + ".tmp"
	err = ioutil.WriteFile(tempFile, data, 0600)
	if err != nil {
		log.Printf("写入临时密钥文件失败: %v", err)
		
		// 尝试直接写入最终文件
		err = ioutil.WriteFile(absPath, data, 0600)
		if err != nil {
			log.Printf("写入密钥文件失败: %v", err)
			
			// 最后尝试写入用户主目录
			homeDir, err := os.UserHomeDir()
			if err != nil {
				log.Printf("无法获取用户主目录: %v", err)
				return err
			}
			
			homeFilePath := filepath.Join(homeDir, "agent_server_key.json")
			log.Printf("尝试写入到用户主目录: %s", homeFilePath)
			err = ioutil.WriteFile(homeFilePath, data, 0600)
			if err != nil {
				log.Printf("写入到用户主目录也失败: %v", err)
				return err
			}
			
			// 更新文件路径
			km.keyFile = homeFilePath
			log.Printf("密钥文件已保存到用户主目录: %s", homeFilePath)
			return nil
		}
	} else {
		// 如果临时文件写入成功，重命名为最终文件
		if err := os.Rename(tempFile, absPath); err != nil {
			log.Printf("重命名临时文件失败: %v，尝试直接使用临时文件", err)
			// 如果重命名失败，直接使用临时文件
			km.keyFile = tempFile
			log.Printf("使用临时文件作为密钥文件: %s", tempFile)
			return nil
		}
	}
	
	log.Printf("密钥文件保存成功: %s", absPath)
	return nil
}

// GenerateNewKey 生成新的随机密钥
func (km *KeyManager) GenerateNewKey() error {
	km.mutex.Lock()
	defer km.mutex.Unlock()
	
	// 生成32字节的随机密钥
	keyBytes := make([]byte, 32)
	_, err := rand.Read(keyBytes)
	if err != nil {
		return err
	}
	
	// Base64编码密钥
	km.securityKey.Key = base64.StdEncoding.EncodeToString(keyBytes)
	
	// 保存到文件
	err = km.saveKey()
	if err != nil {
		return err
	}
	
	// 更新最后修改时间
	fileInfo, err := os.Stat(km.keyFile)
	if err == nil {
		km.lastModified = fileInfo.ModTime()
	}
	
	return nil
}

// GetKey 获取当前密钥
func (km *KeyManager) GetKey() string {
	km.mutex.RLock()
	defer km.mutex.RUnlock()
	
	return km.securityKey.Key
}

// SetKey 设置新密钥
func (km *KeyManager) SetKey(newKey string) error {
	// 验证密钥格式
	_, err := base64.StdEncoding.DecodeString(newKey)
	if err != nil {
		return errors.New("无效的密钥格式，必须是有效的Base64编码字符串")
	}
	
	km.mutex.Lock()
	
	oldKey := km.securityKey.Key
	km.securityKey.Key = newKey
	
	// 保存到文件
	err = km.saveKey()
	if err != nil {
		// 恢复原密钥
		km.securityKey.Key = oldKey
		km.mutex.Unlock()
		return err
	}
	
	// 更新最后修改时间
	fileInfo, err := os.Stat(km.keyFile)
	if err == nil {
		km.lastModified = fileInfo.ModTime()
	}
	
	km.mutex.Unlock()
	
	return nil
}

// ValidateKey 验证提供的密钥是否与当前密钥匹配
func (km *KeyManager) ValidateKey(key string) bool {
	km.mutex.RLock()
	defer km.mutex.RUnlock()
	
	return km.securityKey.Key == key
} 