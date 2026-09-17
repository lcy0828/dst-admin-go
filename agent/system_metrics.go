package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/load"
)

func collectAgentSystemMetrics(installations []RuntimeInstallation) map[string]interface{} {
	metrics := map[string]interface{}{
		"cpu_usage_available":  false,
		"load_supported":       false,
		"disk_usage_available": false,
	}

	if values, err := cpu.Percent(0, true); err == nil && len(values) > 0 {
		total := 0.0
		for index, value := range values {
			values[index] = clampPercentage(value)
			total += values[index]
		}
		metrics["cpu_usage"] = clampPercentage(total / float64(len(values)))
		metrics["cpu_core_usage"] = values
		metrics["cpu_usage_available"] = true
	}
	if items, err := cpu.Info(); err == nil && len(items) > 0 {
		metrics["cpu_model"] = strings.TrimSpace(items[0].ModelName)
	}
	if average, err := load.Avg(); err == nil {
		metrics["load1"] = average.Load1
		metrics["load5"] = average.Load5
		metrics["load15"] = average.Load15
		metrics["load_supported"] = true
	}

	for _, path := range agentDiskProbePaths(installations) {
		usage, err := disk.Usage(path)
		if err != nil {
			continue
		}
		metrics["disk_path"] = path
		metrics["disk_total"] = usage.Total
		metrics["disk_used"] = usage.Used
		metrics["disk_available"] = usage.Free
		metrics["disk_usage"] = clampPercentage(usage.UsedPercent)
		metrics["disk_usage_available"] = true
		break
	}

	return metrics
}

func agentDiskProbePaths(installations []RuntimeInstallation) []string {
	candidates := make([]string, 0, len(installations)*2+2)
	for _, installation := range installations {
		candidates = append(candidates, installation.SavePath, installation.ServerPath)
	}
	if current, err := os.Getwd(); err == nil {
		candidates = append(candidates, current)
	}
	if runtime.GOOS == "windows" {
		volume := filepath.VolumeName(os.TempDir())
		if volume == "" {
			volume = `C:`
		}
		candidates = append(candidates, volume+string(os.PathSeparator))
	} else {
		candidates = append(candidates, "/")
	}

	seen := map[string]struct{}{}
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		path := nearestExistingPath(candidate)
		if path == "" {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	return result
}

func nearestExistingPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	path, err := filepath.Abs(value)
	if err != nil {
		return ""
	}
	for {
		info, statErr := os.Stat(path)
		if statErr == nil {
			if !info.IsDir() {
				path = filepath.Dir(path)
			}
			return filepath.Clean(path)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return ""
		}
		path = parent
	}
}

func clampPercentage(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}
