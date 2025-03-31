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
	"strings"
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
	
	// 检查是否是直接使用文件名而非路径
	if !filepath.IsAbs(km.keyFile) && !strings.Contains(km.keyFile, "/") && !strings.Contains(km.keyFile, "\\") {
		// 如果是纯文件名，检查当前目录
		workDir, err := os.Getwd()
		if err == nil {
			possiblePath := filepath.Join(workDir, km.keyFile)
			fileInfo, err := os.Stat(possiblePath)
			if err == nil && !fileInfo.IsDir() {
				log.Printf("使用当前目录中的密钥文件: %s", possiblePath)
				km.keyFile = possiblePath
			}
		}
	}
	
	log.Printf("尝试加载密钥文件: %s", km.keyFile)
	
	// 检查文件是否存在
	fileInfo, err := os.Stat(km.keyFile)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("密钥文件不存在: %s", km.keyFile)
		} else {
			log.Printf("检查密钥文件状态失败: %v", err)
		}
		return err
	}
	
	if fileInfo.IsDir() {
		err := fmt.Errorf("指定的密钥文件路径是一个目录: %s", km.keyFile)
		log.Print(err)
		return err
	}
	
	// 读取文件内容
	data, err := ioutil.ReadFile(km.keyFile)
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
	log.Printf("密钥文件成功加载: %s", km.keyFile)
	log.Printf("密钥值: %s", km.securityKey.Key)
	return nil
}

// saveKey 保存密钥到文件
func (km *KeyManager) saveKey() error {
	km.mutex.RLock()
	defer km.mutex.RUnlock()
	
	// 尝试直接在当前目录保存密钥文件
	fileName := "agent_server_key.json"
	workDir, err := os.Getwd()
	if err != nil {
		log.Printf("无法获取当前工作目录: %v", err)
		workDir = "."
	}
	
	localPath := filepath.Join(workDir, fileName)
	log.Printf("尝试直接保存密钥到当前目录: %s", localPath)
	
	data, err := json.MarshalIndent(km.securityKey, "", "  ")
	if err != nil {
		log.Printf("序列化密钥数据失败: %v", err)
		return err
	}
	
	// 尝试写入到当前目录
	err = ioutil.WriteFile(localPath, data, 0600)
	if err != nil {
		log.Printf("写入密钥文件到当前目录失败: %v", err)
		
		// 尝试/tmp目录
		tmpPath := filepath.Join("/tmp", fileName)
		log.Printf("尝试保存到/tmp目录: %s", tmpPath)
		
		err = ioutil.WriteFile(tmpPath, data, 0600)
		if err != nil {
			log.Printf("写入密钥文件到/tmp目录失败: %v", err)
			return err
		}
		
		// 更新文件路径
		km.keyFile = tmpPath
		log.Printf("密钥文件已保存到/tmp目录: %s", tmpPath)
		return nil
	}
	
	// 成功写入当前目录
	km.keyFile = localPath
	log.Printf("密钥文件成功保存到当前目录: %s", localPath)
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
		log.Printf("生成随机密钥失败: %v", err)
		return err
	}
	
	// Base64编码密钥
	km.securityKey.Key = base64.StdEncoding.EncodeToString(keyBytes)
	log.Printf("已成功生成新密钥，长度为 %d 字符", len(km.securityKey.Key))
	
	// 如果路径不是/tmp目录下，则修改为/tmp目录
	if !strings.HasPrefix(km.keyFile, "/tmp/") {
		km.keyFile = "/tmp/agent_server_key.json"
		log.Printf("修改密钥文件路径为: %s", km.keyFile)
	}
	
	// 确保目录存在
	dir := filepath.Dir(km.keyFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("创建目录失败: %v", err)
		return err
	}
	
	// 保存到文件
	data, err := json.MarshalIndent(km.securityKey, "", "  ")
	if err != nil {
		log.Printf("序列化密钥数据失败: %v", err)
		return err
	}
	
	log.Printf("正在写入密钥到文件: %s", km.keyFile)
	err = ioutil.WriteFile(km.keyFile, data, 0600)
	if err != nil {
		log.Printf("写入密钥文件失败: %v", err)
		return err
	}
	
	log.Printf("密钥文件成功保存: %s", km.keyFile)
	
	// 验证文件是否真的写入成功
	if _, err := os.Stat(km.keyFile); err != nil {
		log.Printf("警告: 无法验证文件是否写入成功: %v", err)
	} else {
		log.Printf("确认密钥文件已成功写入")
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