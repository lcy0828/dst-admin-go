package rooms

import (
	"errors"
	"strings"
	"unicode"
)

const MinClusterTokenLength = 16

var (
	ErrClusterTokenRequired = errors.New("在线服务器需要填写 Klei 集群令牌")
	ErrClusterTokenInvalid  = errors.New("Klei 集群令牌不完整或格式无效")
)

func ValidateClusterToken(value string, allowEmpty bool) error {
	token := strings.TrimSpace(value)
	if token == "" {
		if allowEmpty {
			return nil
		}
		return ErrClusterTokenRequired
	}
	if len(token) < MinClusterTokenLength || len(token) > 4096 {
		return ErrClusterTokenInvalid
	}
	for _, character := range value {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return ErrClusterTokenInvalid
		}
	}
	return nil
}
