package runtimefiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"dont/shared"
)

const (
	ConfigurationScopeShared      = "shared"
	ConfigurationScopeWorld       = "world"
	ConfigurationScopeMod         = "mod"
	ConfigurationScopeTokenStatus = "token-status"

	maximumConfigurationFileBytes = int64(4 * 1024 * 1024)
	maximumConfigurationReadBytes = int64(8 * 1024 * 1024)
)

var configurationNames = map[string][]string{
	ConfigurationScopeShared:      {"cluster.ini", "adminlist.txt", "blocklist.txt", "whitelist.txt"},
	ConfigurationScopeWorld:       {"server.ini", "leveldataoverride.lua"},
	ConfigurationScopeMod:         {"modoverrides.lua"},
	ConfigurationScopeTokenStatus: {},
}

var requiredConfigurationNames = map[string]map[string]bool{
	ConfigurationScopeShared:      {"cluster.ini": true},
	ConfigurationScopeWorld:       {"server.ini": true, "leveldataoverride.lua": true},
	ConfigurationScopeMod:         {"modoverrides.lua": true},
	ConfigurationScopeTokenStatus: {},
}

// ReadConfiguration reads only the fixed DST configuration surface. The
// request contains logical room and world directories, never a host path.
func ReadConfiguration(ctx context.Context, saveRoot, cluster, shard, scope string) (shared.RuntimeConfigurationResult, error) {
	names, valid := configurationNames[scope]
	if !valid {
		return shared.RuntimeConfigurationResult{}, errors.New("Runtime 配置读取范围不受支持")
	}
	shardRoot, err := trustedShardPath(saveRoot, cluster, shard)
	if err != nil {
		return shared.RuntimeConfigurationResult{}, err
	}
	root := shardRoot
	if scope == ConfigurationScopeShared {
		root = filepath.Dir(shardRoot)
	}
	result := shared.RuntimeConfigurationResult{Complete: true, Files: make([]shared.RuntimeConfigurationFile, 0, len(names))}
	if scope == ConfigurationScopeTokenStatus {
		data, info, exists, err := readTrustedRegular(filepath.Join(filepath.Dir(shardRoot), "cluster_token.txt"), maximumConfigurationFileBytes)
		if err != nil {
			return shared.RuntimeConfigurationResult{}, err
		}
		status := &shared.RuntimeClusterTokenStatus{Exists: exists}
		if exists {
			token := strings.TrimSpace(string(data))
			sum := sha256.Sum256([]byte(token))
			status.Configured = token != ""
			status.SHA256 = hex.EncodeToString(sum[:])
			status.Mode = uint32(info.Mode().Perm())
			status.UpdatedAt = info.ModTime().UTC()
			if token != "" {
				tail := token
				if len(tail) > 4 {
					tail = tail[len(tail)-4:]
				}
				status.MaskedValue = "****" + tail
			}
		}
		result.TokenStatus = status
		return result, nil
	}
	var total int64
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return shared.RuntimeConfigurationResult{}, err
		}
		data, info, exists, err := readTrustedRegular(filepath.Join(root, name), maximumConfigurationFileBytes)
		if err != nil {
			return shared.RuntimeConfigurationResult{}, err
		}
		if !exists {
			if requiredConfigurationNames[scope][name] {
				return shared.RuntimeConfigurationResult{}, os.ErrNotExist
			}
			result.Files = append(result.Files, shared.RuntimeConfigurationFile{Name: name})
			continue
		}
		total += int64(len(data))
		if total > maximumConfigurationReadBytes {
			return shared.RuntimeConfigurationResult{}, errors.New("Runtime 配置集合超过 8 MiB")
		}
		sum := sha256.Sum256(data)
		result.Files = append(result.Files, shared.RuntimeConfigurationFile{
			Name: name, Exists: true, Mode: uint32(info.Mode().Perm()), Size: int64(len(data)),
			SHA256: hex.EncodeToString(sum[:]), UpdatedAt: info.ModTime().UTC(), Data: data,
		})
	}
	return result, nil
}

// ValidateConfiguration treats Agent output as untrusted and also requires
// explicit records for missing optional files.
func ValidateConfiguration(scope string, result shared.RuntimeConfigurationResult) error {
	names, valid := configurationNames[scope]
	if !valid || !result.Complete || result.RevisionConflict || len(result.Warnings) != 0 || result.PublicationID != "" || result.Offset != 0 || result.NextOffset != 0 || result.Size != 0 || result.SHA256 != "" || len(result.Files) != len(names) {
		return errors.New("Runtime 配置集合元数据无效")
	}
	if scope == ConfigurationScopeTokenStatus {
		return validateClusterTokenStatus(result.TokenStatus)
	}
	if result.TokenStatus != nil {
		return errors.New("Runtime 配置集合包含意外的 Token 状态")
	}
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	seen := make(map[string]bool, len(names))
	var total int64
	for _, file := range result.Files {
		if !allowed[file.Name] || seen[file.Name] || filepath.Base(file.Name) != file.Name {
			return errors.New("Runtime 配置文件名无效")
		}
		seen[file.Name] = true
		if !file.Exists {
			if requiredConfigurationNames[scope][file.Name] || file.Mode != 0 || file.Size != 0 || file.SHA256 != "" || !file.UpdatedAt.IsZero() || len(file.Data) != 0 {
				return errors.New("Runtime 缺失配置文件元数据无效")
			}
			continue
		}
		if file.Mode == 0 || file.Mode&^uint32(0o777) != 0 || file.Size < 0 || file.Size > maximumConfigurationFileBytes || file.Size != int64(len(file.Data)) ||
			len(file.SHA256) != sha256.Size*2 || file.UpdatedAt.IsZero() {
			return errors.New("Runtime 配置文件元数据无效")
		}
		sum := sha256.Sum256(file.Data)
		if !strings.EqualFold(file.SHA256, hex.EncodeToString(sum[:])) {
			return errors.New("Runtime 配置文件校验和不匹配")
		}
		total += file.Size
		if total > maximumConfigurationReadBytes {
			return errors.New("Runtime 配置集合超过 8 MiB")
		}
	}
	return nil
}

func validateClusterTokenStatus(status *shared.RuntimeClusterTokenStatus) error {
	if status == nil {
		return errors.New("Runtime 未返回 Cluster Token 状态")
	}
	if !status.Exists {
		if status.Configured || status.MaskedValue != "" || status.SHA256 != "" || status.Mode != 0 || !status.UpdatedAt.IsZero() {
			return errors.New("Runtime 缺失 Token 状态元数据无效")
		}
		return nil
	}
	if status.Mode == 0 || status.Mode&^uint32(0o777) != 0 || len(status.SHA256) != sha256.Size*2 || status.UpdatedAt.IsZero() {
		return errors.New("Runtime Token 状态元数据无效")
	}
	if _, err := hex.DecodeString(status.SHA256); err != nil {
		return errors.New("Runtime Token 状态摘要无效")
	}
	if status.Configured {
		if !strings.HasPrefix(status.MaskedValue, "****") || len(status.MaskedValue) < 5 || len(status.MaskedValue) > 8 {
			return errors.New("Runtime Token 掩码无效")
		}
	} else if status.MaskedValue != "" {
		return errors.New("Runtime 空 Token 不应包含掩码")
	}
	return nil
}
