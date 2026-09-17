package luajit

import (
	"context"
	"crypto/sha256"
	"dont/internal/operationprogress"
	"dont/shared"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func Download(ctx context.Context, rawURL, token, directory string, r shared.LuaJITRelease) (string, error) {
	if r.Size < 1 || r.Size > MaxPackageBytes || !packageID.MatchString(r.SHA256) {
		return "", ErrInvalidPackage
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 8 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("不允许重定向安装包下载凭据")
	}}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return "", fmt.Errorf("下载安装包失败: HTTP %d", response.StatusCode)
	}
	if response.ContentLength >= 0 && response.ContentLength != r.Size {
		return "", ErrInvalidPackage
	}
	f, err := os.CreateTemp(directory, "package-*.zip")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(f.Name())
		}
	}()
	hash := sha256.New()
	counter := &downloadCounter{ctx: ctx, total: r.Size}
	n, err := io.Copy(io.MultiWriter(f, hash, counter), io.LimitReader(response.Body, r.Size+1))
	if err != nil {
		return "", err
	}
	if n != r.Size || hex.EncodeToString(hash.Sum(nil)) != r.SHA256 {
		return "", errors.New("LuaJIT 安装包下载不完整或 SHA-256 不匹配")
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	ok = true
	return f.Name(), nil
}

type downloadCounter struct {
	ctx            context.Context
	total, current int64
	last           time.Time
}

func (c *downloadCounter) Write(p []byte) (int, error) {
	c.current += int64(len(p))
	if time.Since(c.last) > time.Second {
		operationprogress.Report(c.ctx, operationprogress.Update{Stage: "luajit.download", Percent: 5 + int(15*c.current/c.total), Message: "正在下载 LuaJIT 安装包", CurrentBytes: c.current, TotalBytes: c.total})
		c.last = time.Now()
	}
	return len(p), c.ctx.Err()
}
