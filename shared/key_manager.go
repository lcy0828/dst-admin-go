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
	
	"github.com/go-ini/ini"
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

// KeyManagerInterface 密钥管理器接口
type KeyManagerInterface interface {
	GetKey() string
	SetKey(string) error
	ValidateKey(string) bool
	GenerateNewKey() error
	SetKeyChangedCallback(func(string))
	StopWatching()
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
	
	log.Printf("保存密钥到文件: %s", km.keyFile)
	
	data, err := json.MarshalIndent(km.securityKey, "", "  ")
	if err != nil {
		log.Printf("序列化密钥数据失败: %v", err)
		return err
	}
	
	// 直接写入文件
	err = ioutil.WriteFile(km.keyFile, data, 0600)
	if err != nil {
		log.Printf("写入密钥文件失败: %v", err)
		return err
	}
	
	log.Printf("密钥文件成功保存: %s", km.keyFile)
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

// 新增一个基于配置文件的密钥管理器
type ConfigKeyManager struct {
	KeyManager
	configFile  string
	section     string
	keyName     string
}

// NewKeyManagerWithConfig 创建一个基于配置文件的密钥管理器
// configFile: 配置文件路径
// section: 配置文件中的节名
// keyName: 配置文件中的键名
func NewKeyManagerWithConfig(configFile, section, keyName string) (*ConfigKeyManager, error) {
	log.Printf("初始化基于配置文件的密钥管理器: %s [%s].%s", configFile, section, keyName)
	
	ckm := &ConfigKeyManager{
		KeyManager: KeyManager{
			stopWatchChan: make(chan struct{}),
			watchInterval: 10 * time.Second, // 默认每10秒检查一次文件变化
		},
		configFile: configFile,
		section:    section,
		keyName:    keyName,
	}
	
	// 如果配置文件不存在，先直接创建一个简单的配置文件
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		log.Printf("配置文件不存在，将直接创建简单配置文件: %s", configFile)
		
		// 确保目录存在
		dir := filepath.Dir(configFile)
		if dir != "" && dir != "." {
			log.Printf("确保目录存在: %s", dir)
			if err := os.MkdirAll(dir, 0755); err != nil {
				log.Printf("创建目录失败: %v", err)
			} else {
				log.Printf("目录已创建或已存在")
			}
		}
		
		// 生成32字节的随机密钥
		log.Printf("生成随机密钥...")
		keyBytes := make([]byte, 32)
		_, err := rand.Read(keyBytes)
		if err != nil {
			log.Printf("生成随机密钥失败: %v", err)
			return nil, fmt.Errorf("生成随机密钥失败: %v", err)
		}
		
		// Base64编码密钥
		keyValue := base64.StdEncoding.EncodeToString(keyBytes)
		log.Printf("成功生成随机密钥，长度为 %d 字符", len(keyValue))
		
		// 创建简单配置文件
		configContent := fmt.Sprintf("[%s]\n%s = %s\n", section, keyName, keyValue)
		log.Printf("尝试直接创建配置文件...")
		
		// 先尝试临时目录
		tempFile := filepath.Join(os.TempDir(), "temp_config.ini")
		if err := ioutil.WriteFile(tempFile, []byte(configContent), 0644); err != nil {
			log.Printf("写入临时文件失败: %v", err)
		} else {
			// 复制到目标路径
			if err := copyFile(tempFile, configFile); err != nil {
				log.Printf("复制临时文件到目标位置失败: %v", err)
			} else {
				log.Printf("成功创建配置文件: %s", configFile)
				
				// 设置密钥
				ckm.securityKey.Key = keyValue
				
				// 启动文件监控
				go ckm.watchConfigFile()
				
				return ckm, nil
			}
		}
	}
	
	// 尝试加载现有密钥
	log.Printf("尝试加载现有密钥...")
	err := ckm.loadKeyFromConfig()
	if err != nil {
		// 如果是密钥项不存在的错误，或者文件不存在的错误，生成新密钥
		if os.IsNotExist(err) || strings.Contains(err.Error(), "未找到密钥项") || strings.Contains(err.Error(), "配置文件不存在") {
			log.Printf("配置文件中未找到密钥设置 [%s].%s，将创建新密钥", section, keyName)
			
			// 直接创建一个新的随机密钥
			keyBytes := make([]byte, 32)
			_, err := rand.Read(keyBytes)
			if err != nil {
				log.Printf("生成随机密钥失败: %v", err)
				return nil, fmt.Errorf("生成随机密钥失败: %v", err)
			}
			
			ckm.securityKey.Key = base64.StdEncoding.EncodeToString(keyBytes)
			log.Printf("已成功生成新密钥，长度为 %d 字符", len(ckm.securityKey.Key))
			
			// 直接尝试保存到配置文件
			if saveErr := ckm.saveKeyToConfig(); saveErr != nil {
				log.Printf("警告: 保存密钥到配置文件失败: %v", saveErr)
				log.Printf("将继续使用内存中的密钥，但不会持久化")
			}
		} else {
			// 其他错误则返回
			return nil, err
		}
	} else {
		log.Printf("成功从配置文件加载密钥")
	}
	
	// 启动文件监控，热加载密钥
	go ckm.watchConfigFile()
	
	return ckm, nil
}

// 辅助函数：复制文件
func copyFile(src, dst string) error {
	log.Printf("复制文件 %s -> %s", src, dst)
	
	// 读取源文件
	data, err := ioutil.ReadFile(src)
	if err != nil {
		return fmt.Errorf("读取源文件失败: %v", err)
	}
	
	// 确保目标目录存在
	dir := filepath.Dir(dst)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建目标目录失败: %v", err)
		}
	}
	
	// 写入目标文件
	if err := ioutil.WriteFile(dst, data, 0644); err != nil {
		return fmt.Errorf("写入目标文件失败: %v", err)
	}
	
	return nil
}

// 从配置文件加载密钥
func (ckm *ConfigKeyManager) loadKeyFromConfig() error {
	ckm.mutex.Lock()
	defer ckm.mutex.Unlock()
	
	log.Printf("尝试从配置文件加载密钥: %s [%s].%s", ckm.configFile, ckm.section, ckm.keyName)
	
	// 检查配置文件是否存在
	fileInfo, err := os.Stat(ckm.configFile)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("配置文件不存在: %s", ckm.configFile)
		} else {
			log.Printf("检查配置文件状态失败: %v", err)
		}
		return err
	}
	
	if fileInfo.IsDir() {
		err := fmt.Errorf("指定的配置文件路径是一个目录: %s", ckm.configFile)
		log.Print(err)
		return err
	}
	
	// 读取配置文件
	cfg, err := ini.Load(ckm.configFile)
	if err != nil {
		log.Printf("读取配置文件失败: %v", err)
		return err
	}
	
	// 获取密钥值
	section := cfg.Section(ckm.section)
	if !section.HasKey(ckm.keyName) {
		err := fmt.Errorf("配置文件中未找到密钥项 [%s].%s", ckm.section, ckm.keyName)
		log.Print(err)
		return err
	}
	
	keyValue := section.Key(ckm.keyName).String()
	if keyValue == "" {
		err := fmt.Errorf("配置文件中密钥项值为空 [%s].%s", ckm.section, ckm.keyName)
		log.Print(err)
		return err
	}
	
	ckm.securityKey.Key = keyValue
	ckm.lastModified = fileInfo.ModTime()
	
	log.Printf("从配置文件成功加载密钥: %s [%s].%s", ckm.configFile, ckm.section, ckm.keyName)
	return nil
}

// 保存密钥到配置文件
func (ckm *ConfigKeyManager) saveKeyToConfig() error {
	ckm.mutex.RLock()
	defer ckm.mutex.RUnlock()
	
	log.Printf("开始保存密钥到配置文件: %s [%s].%s", ckm.configFile, ckm.section, ckm.keyName)
	
	// 确保目录存在
	dir := filepath.Dir(ckm.configFile)
	if dir != "" && dir != "." {
		log.Printf("确保目录存在: %s", dir)
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Printf("创建配置文件目录失败: %v", err)
			return fmt.Errorf("创建配置文件目录失败: %v", err)
		}
		log.Printf("目录已存在或已创建: %s", dir)
	}
	
	// 使用直接写入的方式创建配置文件
	if _, err := os.Stat(ckm.configFile); os.IsNotExist(err) {
		log.Printf("配置文件不存在，创建新文件: %s", ckm.configFile)
		
		// 创建简单的配置内容
		configContent := fmt.Sprintf("[%s]\n%s = %s\n", 
			ckm.section, ckm.keyName, ckm.securityKey.Key)
		
		log.Printf("尝试直接写入配置文件...")
		err = ioutil.WriteFile(ckm.configFile, []byte(configContent), 0644)
		if err != nil {
			log.Printf("直接写入配置文件失败: %v", err)
			
			// 尝试写入临时文件
			tempFile := filepath.Join(os.TempDir(), "temp_config.ini")
			log.Printf("尝试写入临时文件: %s", tempFile)
			if err := ioutil.WriteFile(tempFile, []byte(configContent), 0644); err != nil {
				log.Printf("写入临时文件失败: %v", err)
				return err
			}
			
			log.Printf("临时文件写入成功，尝试移动到目标位置")
			if err := os.Rename(tempFile, ckm.configFile); err != nil {
				log.Printf("移动临时文件失败: %v", err)
				return err
			}
			
			log.Printf("文件已成功移动")
		} else {
			log.Printf("配置文件直接写入成功")
		}
		
		log.Printf("密钥已成功保存到新配置文件: %s [%s].%s", ckm.configFile, ckm.section, ckm.keyName)
		return nil
	}
	
	// 如果文件存在，则加载并更新
	log.Printf("配置文件已存在，尝试加载并更新: %s", ckm.configFile)
	
	// 读取现有配置文件
	cfg, err := ini.Load(ckm.configFile)
	if err != nil {
		log.Printf("读取配置文件失败: %v，将创建新配置", err)
		cfg = ini.Empty()
	} else {
		log.Printf("成功加载现有配置文件")
	}
	
	// 设置密钥值
	log.Printf("设置配置项 [%s].%s", ckm.section, ckm.keyName)
	section, err := cfg.GetSection(ckm.section)
	if err != nil {
		// 如果节不存在，创建新节
		log.Printf("配置文件中不存在节 [%s]，创建新节", ckm.section)
		section, err = cfg.NewSection(ckm.section)
		if err != nil {
			log.Printf("创建配置节失败: %v", err)
			return err
		}
		log.Printf("成功创建配置节 [%s]", ckm.section)
	}
	
	section.Key(ckm.keyName).SetValue(ckm.securityKey.Key)
	log.Printf("成功设置配置项值")
	
	// 保存到临时文件然后重命名，确保原子性
	tempFile := ckm.configFile + ".tmp"
	log.Printf("保存配置到临时文件: %s", tempFile)
	if err := cfg.SaveTo(tempFile); err != nil {
		log.Printf("保存到临时文件失败: %v", err)
		
		// 尝试直接保存
		log.Printf("尝试直接保存到目标文件: %s", ckm.configFile)
		if err := cfg.SaveTo(ckm.configFile); err != nil {
			log.Printf("直接保存也失败: %v", err)
			return err
		}
		log.Printf("直接保存成功")
	} else {
		// 重命名临时文件
		log.Printf("临时文件保存成功，尝试重命名到: %s", ckm.configFile)
		if err := os.Rename(tempFile, ckm.configFile); err != nil {
			log.Printf("重命名临时文件失败: %v", err)
			
			// 尝试复制内容
			log.Printf("尝试复制临时文件内容")
			tempData, readErr := ioutil.ReadFile(tempFile)
			if readErr != nil {
				log.Printf("读取临时文件失败: %v", readErr)
				return err // 返回原始重命名错误
			}
			
			if writeErr := ioutil.WriteFile(ckm.configFile, tempData, 0644); writeErr != nil {
				log.Printf("写入目标文件失败: %v", writeErr)
				return err // 返回原始重命名错误
			}
			
			log.Printf("复制临时文件内容成功")
			os.Remove(tempFile) // 尝试删除临时文件
		} else {
			log.Printf("重命名临时文件成功")
		}
	}
	
	// 验证文件是否成功保存
	if _, err := os.Stat(ckm.configFile); err != nil {
		log.Printf("验证配置文件保存失败: %v", err)
		return err
	}
	
	log.Printf("密钥已成功保存到配置文件: %s [%s].%s", ckm.configFile, ckm.section, ckm.keyName)
	return nil
}

// 监控配置文件变化
func (ckm *ConfigKeyManager) watchConfigFile() {
	ticker := time.NewTicker(ckm.watchInterval)
	defer ticker.Stop()
	
	log.Printf("开始监控配置文件变化: %s", ckm.configFile)
	
	// 获取初始文件信息
	fileInfo, err := os.Stat(ckm.configFile)
	var lastModTime time.Time
	if err == nil {
		lastModTime = fileInfo.ModTime()
	} else {
		lastModTime = time.Now()
		log.Printf("获取初始文件信息失败: %v，使用当前时间作为初始时间", err)
	}
	
	for {
		select {
		case <-ckm.stopWatchChan:
			log.Printf("停止监控配置文件: %s", ckm.configFile)
			return
		case <-ticker.C:
			// 检查文件是否被修改
			fileInfo, err := os.Stat(ckm.configFile)
			if err != nil {
				log.Printf("监控配置文件错误: %v", err)
				continue
			}
			
			modTime := fileInfo.ModTime()
			
			// 如果文件被修改，重新加载
			if modTime.After(lastModTime) {
				log.Println("检测到配置文件变更，尝试重新加载密钥")
				oldKey := ckm.GetKey()
				
				// 更新最后修改时间
				lastModTime = modTime
				
				// 读取新密钥
				newKey, err := ckm.loadNewKeyFromConfig()
				if err != nil {
					log.Printf("重新加载密钥失败: %v", err)
					continue
				}
				
				// 如果密钥没变，不做处理
				if newKey == oldKey {
					log.Printf("密钥没有变化，无需更新")
					continue
				}
				
				// 更新密钥
				ckm.mutex.Lock()
				oldKeyValue := ckm.securityKey.Key
				ckm.securityKey.Key = newKey
				ckm.mutex.Unlock()
				
				log.Printf("成功重新加载密钥，旧密钥: %s, 新密钥: %s", oldKeyValue, newKey)
				
				// 触发回调
				if ckm.keyChangedCb != nil {
					go ckm.keyChangedCb(newKey)
				}
			}
		}
	}
}

// 从配置文件加载新密钥，但不更新内部状态
func (ckm *ConfigKeyManager) loadNewKeyFromConfig() (string, error) {
	// 读取配置文件
	cfg, err := ini.Load(ckm.configFile)
	if err != nil {
		return "", fmt.Errorf("读取配置文件失败: %v", err)
	}
	
	// 获取密钥值
	section := cfg.Section(ckm.section)
	if !section.HasKey(ckm.keyName) {
		return "", fmt.Errorf("配置文件中未找到密钥项 [%s].%s", ckm.section, ckm.keyName)
	}
	
	keyValue := section.Key(ckm.keyName).String()
	if keyValue == "" {
		return "", fmt.Errorf("配置文件中密钥项值为空 [%s].%s", ckm.section, ckm.keyName)
	}
	
	return keyValue, nil
}

// GenerateNewKeyToConfig 生成新的随机密钥并保存到配置文件
func (ckm *ConfigKeyManager) GenerateNewKeyToConfig() error {
	ckm.mutex.Lock()
	
	log.Printf("开始生成新的随机密钥...")
	
	// 确保配置文件目录存在
	dir := filepath.Dir(ckm.configFile)
	if dir != "" && dir != "." {
		log.Printf("确保配置文件目录存在: %s", dir)
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Printf("创建配置文件目录失败: %v", err)
			ckm.mutex.Unlock()
			return fmt.Errorf("创建配置文件目录失败: %v", err)
		}
		log.Printf("配置文件目录已创建或已存在")
	}
	
	// 生成32字节的随机密钥
	log.Printf("生成随机密钥数据...")
	keyBytes := make([]byte, 32)
	n, err := rand.Read(keyBytes)
	if err != nil {
		log.Printf("生成随机密钥数据失败: %v", err)
		ckm.mutex.Unlock()
		return err
	}
	log.Printf("成功读取 %d 字节的随机数据", n)
	
	// Base64编码密钥
	ckm.securityKey.Key = base64.StdEncoding.EncodeToString(keyBytes)
	keyValue := ckm.securityKey.Key // 保存一个副本
	log.Printf("已成功生成新密钥，长度为 %d 字符", len(keyValue))
	
	// 尝试直接创建配置文件（如果不存在）
	if _, err := os.Stat(ckm.configFile); os.IsNotExist(err) {
		log.Printf("配置文件不存在，将直接创建: %s", ckm.configFile)
		configContent := fmt.Sprintf("[%s]\n%s = %s\n", 
			ckm.section, ckm.keyName, keyValue)
		
		// 解锁后再进行文件操作
		ckm.mutex.Unlock()
		
		log.Printf("正在创建新配置文件...")
		if err := ioutil.WriteFile(ckm.configFile, []byte(configContent), 0644); err != nil {
			log.Printf("创建配置文件失败: %v", err)
			// 继续尝试其他方式保存
		} else {
			log.Printf("成功创建并写入新配置文件")
			return nil
		}
	} else {
		// 解锁mutex
		ckm.mutex.Unlock()
	}
	
	// 保存到配置文件
	log.Printf("正在调用saveKeyToConfig保存密钥...")
	
	// saveKeyToConfig会获取自己的读锁，所以我们不需要保持锁定状态
	err = ckm.saveKeyToConfig()
	
	if err != nil {
		log.Printf("保存密钥到配置文件失败: %v", err)
		return err
	}
	
	log.Printf("成功保存新生成的密钥到配置文件")
	return nil
}

// GetKey 获取当前密钥
func (ckm *ConfigKeyManager) GetKey() string {
	ckm.mutex.RLock()
	defer ckm.mutex.RUnlock()
	return ckm.securityKey.Key
}

// SetKey 设置新密钥
func (ckm *ConfigKeyManager) SetKey(newKey string) error {
	if newKey == "" {
		return errors.New("密钥不能为空")
	}
	
	ckm.mutex.Lock()
	defer ckm.mutex.Unlock()
	
	// 检查密钥是否符合Base64格式
	_, err := base64.StdEncoding.DecodeString(newKey)
	if err != nil {
		return fmt.Errorf("无效的密钥格式，应为Base64编码字符串: %v", err)
	}
	
	// 更新密钥
	ckm.securityKey.Key = newKey
	
	// 保存到配置文件
	return ckm.saveKeyToConfig()
}

// ValidateKey 验证提供的密钥是否匹配
func (ckm *ConfigKeyManager) ValidateKey(key string) bool {
	ckm.mutex.RLock()
	defer ckm.mutex.RUnlock()
	return key == ckm.securityKey.Key
}

// StopWatching 停止监控配置文件
func (ckm *ConfigKeyManager) StopWatching() {
	close(ckm.stopWatchChan)
}

// GenerateNewKey 生成新的随机密钥 (为了兼容接口)
func (ckm *ConfigKeyManager) GenerateNewKey() error {
	return ckm.GenerateNewKeyToConfig()
} 