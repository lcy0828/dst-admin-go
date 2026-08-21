package hostresource

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const unlimitedMemoryThreshold = uint64(1 << 60)

// Limits describes the schedulable resources visible through a Linux cgroup.
// Zero values mean that the corresponding controller does not impose a limit.
type Limits struct {
	CPUCount         int
	CPUSet           []int
	MemoryLimitBytes uint64
	MemoryUsedBytes  uint64
}

func Detect() Limits {
	if runtime.GOOS != "linux" {
		return Limits{}
	}
	return detectAt("/sys/fs/cgroup")
}

func EffectiveCPUCount(hostLogical int, limits Limits) int {
	if hostLogical < 1 {
		hostLogical = 1
	}
	if limits.CPUCount > 0 && limits.CPUCount < hostLogical {
		return limits.CPUCount
	}
	return hostLogical
}

func EffectiveMemory(hostTotal, hostUsed, hostAvailable uint64, limits Limits) (total, used, available uint64) {
	if limits.MemoryLimitBytes == 0 || limits.MemoryLimitBytes >= unlimitedMemoryThreshold || hostTotal > 0 && limits.MemoryLimitBytes >= hostTotal {
		return hostTotal, hostUsed, hostAvailable
	}
	total = limits.MemoryLimitBytes
	used = limits.MemoryUsedBytes
	if used > total {
		used = total
	}
	return total, used, total - used
}

func detectAt(root string) Limits {
	limits := Limits{}
	if raw, ok := readFirst(root, "cpu.max"); ok {
		fields := strings.Fields(raw)
		if len(fields) == 2 && fields[0] != "max" {
			limits.CPUCount = quotaCPUCount(fields[0], fields[1])
		}
	} else if quota, quotaOK := readFirst(root, filepath.Join("cpu", "cpu.cfs_quota_us")); quotaOK {
		if period, periodOK := readFirst(root, filepath.Join("cpu", "cpu.cfs_period_us")); periodOK {
			limits.CPUCount = quotaCPUCount(quota, period)
		}
	}

	if raw, ok := readFirst(root, "cpuset.cpus.effective", filepath.Join("cpuset", "cpuset.cpus")); ok {
		limits.CPUSet = parseCPUSet(raw)
		if count := len(limits.CPUSet); count > 0 && (limits.CPUCount == 0 || count < limits.CPUCount) {
			limits.CPUCount = count
		}
	}

	if raw, ok := readFirst(root, "memory.max"); ok && strings.TrimSpace(raw) != "max" {
		limits.MemoryLimitBytes = parsePositiveUint(raw)
		if current, currentOK := readFirst(root, "memory.current"); currentOK {
			limits.MemoryUsedBytes = parsePositiveUint(current)
		}
	} else if raw, ok := readFirst(root, filepath.Join("memory", "memory.limit_in_bytes")); ok {
		limits.MemoryLimitBytes = parsePositiveUint(raw)
		if current, currentOK := readFirst(root, filepath.Join("memory", "memory.usage_in_bytes")); currentOK {
			limits.MemoryUsedBytes = parsePositiveUint(current)
		}
	}
	if limits.MemoryLimitBytes >= unlimitedMemoryThreshold {
		limits.MemoryLimitBytes, limits.MemoryUsedBytes = 0, 0
	}
	return limits
}

func readFirst(root string, names ...string) (string, bool) {
	for _, name := range names {
		value, err := os.ReadFile(filepath.Join(root, name))
		if err == nil {
			return strings.TrimSpace(string(value)), true
		}
	}
	return "", false
}

func quotaCPUCount(quotaValue, periodValue string) int {
	quota, quotaErr := strconv.ParseInt(strings.TrimSpace(quotaValue), 10, 64)
	period, periodErr := strconv.ParseInt(strings.TrimSpace(periodValue), 10, 64)
	if quotaErr != nil || periodErr != nil || quota <= 0 || period <= 0 {
		return 0
	}
	count := int(quota / period)
	if count < 1 {
		return 1
	}
	return count
}

func parsePositiveUint(value string) uint64 {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

func parseCPUSet(value string) []int {
	seen := map[int]bool{}
	for _, part := range strings.Split(strings.TrimSpace(value), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		bounds := strings.Split(part, "-")
		start, err := strconv.Atoi(bounds[0])
		if err != nil || start < 0 {
			return nil
		}
		end := start
		if len(bounds) == 2 {
			end, err = strconv.Atoi(bounds[1])
			if err != nil || end < start {
				return nil
			}
		} else if len(bounds) != 1 {
			return nil
		}
		for cpuID := start; cpuID <= end; cpuID++ {
			seen[cpuID] = true
		}
	}
	values := make([]int, 0, len(seen))
	for cpuID := range seen {
		values = append(values, cpuID)
	}
	sort.Ints(values)
	return values
}
