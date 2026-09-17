package runtimefiles

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"dont/shared"
)

const runtimeCommandDocumentDirectory = "command-requests"

var runtimeCommandRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{16,80}$`)

func ValidateCommandDocument(document shared.RuntimeCommandDocument) error {
	if !runtimeCommandRequestID.MatchString(document.RequestID) || len(document.Data) < 1 ||
		len(document.Data) > shared.MaximumRuntimeCommandDocumentBytes || !utf8.Valid(document.Data) || !json.Valid(document.Data) {
		return errors.New("Runtime 命令请求文档无效")
	}
	sum := sha256.Sum256(document.Data)
	if !strings.EqualFold(document.SHA256, hex.EncodeToString(sum[:])) {
		return errors.New("Runtime 命令请求文档校验和不匹配")
	}
	var identity struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(document.Data, &identity); err != nil || identity.RequestID != document.RequestID {
		return errors.New("Runtime 命令请求文档身份不匹配")
	}
	return nil
}

func PublishCommandDocument(ctx context.Context, saveRoot, cluster, shard string, document shared.RuntimeCommandDocument) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateCommandDocument(document); err != nil {
		return err
	}
	worldRoot, err := trustedShardPath(saveRoot, cluster, shard)
	if err != nil {
		return err
	}
	runtimeRoot := filepath.Join(worldRoot, "dst-admin")
	info, err := os.Lstat(runtimeRoot)
	if err != nil {
		return fmt.Errorf("读取 Runtime 命令目录: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Runtime 命令目录不安全")
	}
	directory := filepath.Join(runtimeRoot, runtimeCommandDocumentDirectory)
	if err := os.Mkdir(directory, 0750); err != nil && !os.IsExist(err) {
		return fmt.Errorf("创建 Runtime 命令请求目录: %w", err)
	}
	info, err = os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("读取 Runtime 命令请求目录: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Runtime 命令请求目录不安全")
	}
	path := filepath.Join(directory, document.RequestID+".json")
	temporary, err := os.CreateTemp(directory, ".command-request-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0640); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(document.Data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(temporaryName, path); err != nil {
		if !os.IsExist(err) {
			return err
		}
		current, readErr := readRegularCommandDocument(path)
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(current, document.Data) {
			return errors.New("Runtime 命令请求 ID 已被其他内容占用")
		}
	}
	if handle, openErr := os.Open(directory); openErr == nil {
		_ = handle.Sync()
		_ = handle.Close()
	}
	return nil
}

func readRegularCommandDocument(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > shared.MaximumRuntimeCommandDocumentBytes {
		return nil, errors.New("Runtime 命令请求文件不安全")
	}
	return os.ReadFile(path)
}
