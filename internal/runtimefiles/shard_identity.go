package runtimefiles

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-ini/ini"
)

// ReadConfiguredShardID reads the identity DST uses for telemetry. Disabling
// sharding makes the runtime ID zero regardless of server.ini's configured ID.
// A missing shard_enabled setting retains the explicit server.ini identity for
// compatibility with existing configurations. Callers must resolve worldPath
// within their trusted save root before calling this function.
func ReadConfiguredShardID(worldPath string) (string, error) {
	data, _, exists, err := readTrustedRegular(filepath.Join(filepath.Dir(worldPath), "cluster.ini"), 256*1024)
	if err != nil {
		return "", fmt.Errorf("read cluster.ini shard identity: %w", err)
	}
	if exists {
		cluster, err := ini.Load(data)
		if err != nil {
			return "", fmt.Errorf("parse cluster.ini shard identity: %w", err)
		}
		shardEnabled := cluster.Section("SHARD").Key("shard_enabled")
		if shardEnabled.String() != "" {
			enabled, err := shardEnabled.Bool()
			if err != nil {
				return "", fmt.Errorf("parse cluster.ini shard_enabled: %w", err)
			}
			if !enabled {
				return "0", nil
			}
		}
	}
	data, _, exists, err = readTrustedRegular(filepath.Join(worldPath, "server.ini"), 1024*1024)
	if err != nil {
		return "", fmt.Errorf("read server.ini shard identity: %w", err)
	}
	if !exists {
		return "", fmt.Errorf("server.ini is unavailable: %w", os.ErrNotExist)
	}
	server, err := ini.Load(data)
	if err != nil {
		return "", fmt.Errorf("parse server.ini shard identity: %w", err)
	}
	return strings.TrimSpace(server.Section("SHARD").Key("id").String()), nil
}
