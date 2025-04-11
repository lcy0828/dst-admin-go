package logparser

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 默认保存间隔
const defaultSaveInterval = time.Minute

// LogPosition 日志位置记录
type LogPosition struct {
	FilePath     string    `json:"file_path"`     // 日志文件路径
	LastPosition int64     `json:"last_position"` // 最后读取位置
	LastModTime  time.Time `json:"last_mod_time"` // 最后修改时间
	UpdatedAt    time.Time `json:"updated_at"`    // 更新时间
}

// PositionManager 位置管理器
type PositionManager struct {
	positions      map[string]*LogPosition // 位置记录映射，键为日志文件路径
	positionsMutex sync.RWMutex            // 保护并发访问的互斥锁
	positionsFile  string                  // 位置记录文件路径
	saveInterval   time.Duration           // 保存间隔
	stopChan       chan struct{}           // 停止通道
	isRunning      bool                    // 是否正在运行
}

// NewPositionManager 创建新的位置管理器
func NewPositionManager(positionsFile string) (*PositionManager, error) {
	// 确保目录存在
	dir := filepath.Dir(positionsFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	manager := &PositionManager{
		positions:     make(map[string]*LogPosition),
		positionsFile: positionsFile,
		saveInterval:  time.Minute, // 默认每分钟保存一次
		stopChan:      make(chan struct{}),
	}

	// 加载位置记录
	if err := manager.loadPositions(); err != nil {
		log.Printf("[PositionManager] 加载位置记录失败: %v，将创建新的位置记录", err)
	}

	return manager, nil
}

// Start 启动位置管理器
func (m *PositionManager) Start() {
	if m.isRunning {
		return
	}

	m.isRunning = true
	go m.autoSaveLoop()
}

// Stop 停止位置管理器
func (m *PositionManager) Stop() {
	if !m.isRunning {
		return
	}

	close(m.stopChan)
	m.isRunning = false

	// 保存位置记录
	if err := m.savePositions(); err != nil {
		log.Printf("[PositionManager] 保存位置记录失败: %v", err)
	}
}

// autoSaveLoop 自动保存循环
func (m *PositionManager) autoSaveLoop() {
	ticker := time.NewTicker(m.saveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := m.savePositions(); err != nil {
				log.Printf("[PositionManager] 自动保存位置记录失败: %v", err)
			}
		case <-m.stopChan:
			return
		}
	}
}

// loadPositions 加载位置记录
func (m *PositionManager) loadPositions() error {
	// 检查文件是否存在
	if _, err := os.Stat(m.positionsFile); os.IsNotExist(err) {
		return nil // 文件不存在，返回空记录
	}

	// 读取文件内容
	data, err := os.ReadFile(m.positionsFile)
	if err != nil {
		return err
	}

	// 解析JSON
	var positions map[string]*LogPosition
	if err := json.Unmarshal(data, &positions); err != nil {
		return err
	}

	// 更新位置记录
	m.positionsMutex.Lock()
	m.positions = positions
	m.positionsMutex.Unlock()

	log.Printf("[PositionManager] 成功加载 %d 条位置记录", len(positions))
	return nil
}

// savePositions 保存位置记录
func (m *PositionManager) savePositions() error {
	m.positionsMutex.RLock()
	positions := m.positions
	m.positionsMutex.RUnlock()

	// 序列化为JSON
	data, err := json.MarshalIndent(positions, "", "  ")
	if err != nil {
		return err
	}

	// 写入文件
	if err := os.WriteFile(m.positionsFile, data, 0644); err != nil {
		return err
	}

	log.Printf("[PositionManager] 成功保存 %d 条位置记录", len(positions))
	return nil
}

// GetPosition 获取位置记录
func (m *PositionManager) GetPosition(filePath string) *LogPosition {
	m.positionsMutex.RLock()
	defer m.positionsMutex.RUnlock()

	position, exists := m.positions[filePath]
	if !exists {
		return nil
	}
	return position
}

// UpdatePosition 更新位置记录
func (m *PositionManager) UpdatePosition(filePath string, lastPosition int64, lastModTime time.Time) {
	m.positionsMutex.Lock()
	defer m.positionsMutex.Unlock()

	position, exists := m.positions[filePath]
	if !exists {
		position = &LogPosition{
			FilePath: filePath,
		}
		m.positions[filePath] = position
	}

	position.LastPosition = lastPosition
	position.LastModTime = lastModTime
	position.UpdatedAt = time.Now()
}

// RemovePosition 移除位置记录
func (m *PositionManager) RemovePosition(filePath string) {
	m.positionsMutex.Lock()
	defer m.positionsMutex.Unlock()

	delete(m.positions, filePath)
}

// GetAllPositions 获取所有位置记录
func (m *PositionManager) GetAllPositions() map[string]*LogPosition {
	m.positionsMutex.RLock()
	defer m.positionsMutex.RUnlock()

	// 创建副本
	positions := make(map[string]*LogPosition, len(m.positions))
	for k, v := range m.positions {
		positions[k] = v
	}
	return positions
}
