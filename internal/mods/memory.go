package mods

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MemoryMetadataProvider is wired only when DST_ADMIN_ENV=test.
type MemoryMetadataProvider struct {
	Items map[string]SteamMod
}

func NewMemoryMetadataProvider() *MemoryMetadataProvider {
	now := time.Date(2026, time.August, 8, 10, 0, 0, 0, time.UTC)
	return &MemoryMetadataProvider{Items: map[string]SteamMod{
		"378160973": {
			ID: "378160973", Name: "Global Positions", AuthorID: "76561198000000001", Author: "E2E Author",
			Description: "Share map positions with other players.", PreviewURL: "https://steamusercontent.example/378160973.webp",
			Subscriptions: 2500000, Score: 0.96, UpdatedAt: now, Dependencies: []string{"123456789"}, Tags: []string{"Server", "Utility"},
		},
		"123456789": {
			ID: "123456789", Name: "Shared Library", AuthorID: "76561198000000002", Author: "Dependency Author",
			Description: "Dependency used by the test Mod.", PreviewURL: "https://steamusercontent.example/123456789.webp",
			Subscriptions: 100000, Score: 0.88, UpdatedAt: now.Add(-time.Hour), Tags: []string{"Library"},
		},
		"987654321": {
			ID: "987654321", Name: "Runtime Compatibility Mod", AuthorID: "76561198000000003", Author: "Fallback Author",
			Description: "Exercises the external Lua compatibility path in test mode.", PreviewURL: "https://steamusercontent.example/987654321.webp",
			Subscriptions: 42000, Score: 0.91, UpdatedAt: now.Add(-2 * time.Hour), Tags: []string{"Compatibility"},
		},
	}}
}

func (p *MemoryMetadataProvider) Search(_ context.Context, query string, page, pageSize int) (SearchResult, error) {
	if err := validateSearch(query, page, pageSize); err != nil {
		return SearchResult{}, err
	}
	items := make([]SteamMod, 0)
	for _, item := range p.Items {
		if validModID(query) && item.ID != query {
			continue
		}
		if !validModID(query) && !strings.Contains(strings.ToLower(item.Name), strings.ToLower(query)) {
			continue
		}
		items = append(items, item)
	}
	return SearchResult{Items: items, Total: len(items), Page: page, PageSize: pageSize}, nil
}

func (p *MemoryMetadataProvider) Details(_ context.Context, ids []string) (map[string]SteamMod, error) {
	result := make(map[string]SteamMod)
	for _, id := range uniqueModIDs(ids) {
		if item, ok := p.Items[id]; ok {
			result[id] = item
		}
	}
	return result, nil
}

type MemoryDownloadRunner struct {
	InstallRoot string
	AppID       string
}

// Download creates deterministic workshop files and is wired only in test mode.
func (r *MemoryDownloadRunner) Download(_ context.Context, ids []string, _ bool, output io.Writer) error {
	for _, id := range uniqueModIDs(ids) {
		directory := filepath.Join(r.InstallRoot, "steamapps", "workshop", "content", r.AppID, id)
		if err := os.MkdirAll(directory, 0750); err != nil {
			return err
		}
		content := memoryModInfo(id)
		if err := os.WriteFile(filepath.Join(directory, "modinfo.lua"), []byte(content), 0640); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(output, "workshop item %s downloaded\n", id)
	}
	return nil
}

func memoryModInfo(id string) string {
	if id == "378160973" {
		return `name = ChooseTranslationTable({ zh = "全球定位", [1] = "Global Positions" })
author = "E2E Author"
version = "1.0.0"
description = "E2E Mod"
configuration_options = {
  { name = "show_players", label = "显示玩家", hover = "在地图显示玩家位置", options = {
      { description = "启用", data = true }, { description = "关闭", data = false },
    }, default = true },
  { name = "position_color", label = "位置颜色", options = {
      { description = "森林绿", data = "green" }, { description = "暖橙", data = "orange" },
    }, default = "green" },
}
`
	}
	if id == "987654321" {
		return `name = DSTRuntimeOnlyFunction("Runtime Compatibility Mod")`
	}
	return `name = "Shared Library"
author = "Dependency Author"
version = "1.0.0"
configuration_options = {}
`
}

// MemoryModInfoParser is an explicit test adapter for a Mod that needs the Lua fallback.
type MemoryModInfoParser struct {
	Base ModInfoParser
}

func (p *MemoryModInfoParser) Parse(ctx context.Context, modID, path string) (ParserResult, error) {
	if modID != "987654321" {
		return p.Base.Parse(ctx, modID, path)
	}
	return ParserResult{
		Parser: "lua", FallbackUsed: true,
		FallbackReason: "execute modinfo.lua: unknown DST runtime global DSTRuntimeOnlyFunction",
		Warnings:       []string{"Go 主解析器不兼容此 Mod，已使用外部 Lua fallback"},
		Values: map[string]interface{}{
			"name": "Runtime Compatibility Mod",
			"configuration_options": []interface{}{
				map[string]interface{}{
					"name": "compatibility_mode", "label": "兼容模式", "default": true,
					"options": []interface{}{
						map[string]interface{}{"description": "启用", "data": true},
						map[string]interface{}{"description": "关闭", "data": false},
					},
				},
			},
		},
	}, nil
}

var _ MetadataProvider = (*MemoryMetadataProvider)(nil)
var _ DownloadRunner = (*MemoryDownloadRunner)(nil)
var _ ModInfoParser = (*MemoryModInfoParser)(nil)
