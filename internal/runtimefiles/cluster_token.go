package runtimefiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"dont/shared"
)

const maximumClusterTokenBytes = int64(16 * 1024)

// ReadClusterToken is intentionally separate from ReadConfiguration so a
// caller must negotiate the dedicated secret-reading capability.
func ReadClusterToken(ctx context.Context, saveRoot, cluster, shard string) (shared.RuntimeClusterTokenReveal, error) {
	if err := ctx.Err(); err != nil {
		return shared.RuntimeClusterTokenReveal{}, err
	}
	shardRoot, err := trustedShardPath(saveRoot, cluster, shard)
	if err != nil {
		return shared.RuntimeClusterTokenReveal{}, err
	}
	data, info, exists, err := readTrustedRegular(filepath.Join(filepath.Dir(shardRoot), "cluster_token.txt"), maximumClusterTokenBytes)
	if err != nil {
		return shared.RuntimeClusterTokenReveal{}, err
	}
	if !exists {
		return shared.RuntimeClusterTokenReveal{}, nil
	}
	token := strings.TrimSpace(string(data))
	if !utf8.ValidString(token) || strings.ContainsAny(token, "\x00\r\n") {
		return shared.RuntimeClusterTokenReveal{}, errors.New("Cluster Token 文件内容无效")
	}
	sum := sha256.Sum256([]byte(token))
	return shared.RuntimeClusterTokenReveal{
		Exists: true, Token: token, SHA256: hex.EncodeToString(sum[:]), UpdatedAt: info.ModTime().UTC(),
	}, nil
}

func ValidateClusterTokenReveal(result shared.RuntimeClusterTokenReveal) error {
	if !result.Exists {
		if result.Token != "" || result.SHA256 != "" || !result.UpdatedAt.IsZero() {
			return errors.New("Runtime 缺失 Cluster Token 元数据无效")
		}
		return nil
	}
	if !utf8.ValidString(result.Token) || strings.ContainsAny(result.Token, "\x00\r\n") || len(result.Token) > int(maximumClusterTokenBytes) || len(result.SHA256) != sha256.Size*2 || result.UpdatedAt.IsZero() {
		return errors.New("Runtime Cluster Token 结果无效")
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(result.Token)))
	if !strings.EqualFold(result.SHA256, hex.EncodeToString(sum[:])) {
		return errors.New("Runtime Cluster Token 校验和不匹配")
	}
	return nil
}
