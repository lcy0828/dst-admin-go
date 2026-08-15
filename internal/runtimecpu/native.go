package runtimecpu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"dont/internal/runtimeinventory"
	"dont/shared"
)

var (
	ErrUnsupportedPlatform = errors.New("CPU enforcement is unsupported on this platform")
	ErrCgroupUnavailable   = errors.New("cgroup v2 cpu/cpuset controllers are unavailable")
	ErrInstanceNotRunning  = errors.New("managed DST shard process is not running")
	ErrInstanceChanged     = errors.New("managed DST shard instance changed during CPU enforcement")
	ErrCPUNotApplied       = errors.New("CPU policy was not applied to the managed instance")
)

const cgroupPeriodMicros int64 = 100000

type ProcessIdentity struct {
	PID        int32
	InstanceID string
	Executable string
}

type ProcessResolver func(context.Context, string, string, string) (ProcessIdentity, error)

type NativeConfig struct {
	Platform          string
	ServerRoot        string
	CgroupRoot        string
	Resolve           ProcessResolver
	RegularFilesystem bool
	Now               func() time.Time
}

type Native struct {
	platform          string
	serverRoot        string
	cgroupRoot        string
	resolve           ProcessResolver
	regularFilesystem bool
	now               func() time.Time
}

func NewNative(config NativeConfig) (*Native, error) {
	platform := strings.ToLower(strings.TrimSpace(config.Platform))
	if platform == "" {
		platform = runtime.GOOS
	}
	serverRoot := filepath.Clean(strings.TrimSpace(config.ServerRoot))
	if serverRoot == "" || serverRoot == "." || !filepath.IsAbs(serverRoot) {
		return nil, errors.New("trusted DST server root is required")
	}
	root := filepath.Clean(strings.TrimSpace(config.CgroupRoot))
	if root == "" || root == "." {
		root = "/sys/fs/cgroup"
	}
	if !filepath.IsAbs(root) {
		return nil, errors.New("cgroup root must be absolute")
	}
	resolver := config.Resolve
	if resolver == nil {
		resolver = resolveManagedProcess
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Native{platform: platform, serverRoot: serverRoot, cgroupRoot: root, resolve: resolver, regularFilesystem: config.RegularFilesystem, now: now}, nil
}

func (n *Native) Prepare(ctx context.Context, installationID, cluster, shard string, request shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	request.LogicalCPUIds = SortedUnique(request.LogicalCPUIds)
	if err := validateRequest(request); err != nil {
		return shared.RuntimeCPUResult{}, err
	}
	if request.Policy == shared.RuntimeCPUPolicyNone {
		return n.result(request, shared.RuntimeCPUStatePrepared, nil), nil
	}
	if n.platform != "linux" {
		return shared.RuntimeCPUResult{}, fmt.Errorf("%w: %s does not provide a verifiable cpuset boundary", ErrUnsupportedPlatform, n.platform)
	}
	if err := ctx.Err(); err != nil {
		return shared.RuntimeCPUResult{}, err
	}
	path := n.cgroupPath(installationID, cluster, shard)
	if err := n.prepareCgroup(path, request.LogicalCPUIds); err != nil {
		return shared.RuntimeCPUResult{}, err
	}
	result, err := n.observeCgroup(path, request, nil)
	result.State, result.Enforced = shared.RuntimeCPUStatePrepared, false
	return result, err
}

func (n *Native) Apply(ctx context.Context, installationID, cluster, shard string, request shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	request.LogicalCPUIds = SortedUnique(request.LogicalCPUIds)
	if err := validateRequest(request); err != nil {
		return shared.RuntimeCPUResult{}, err
	}
	if request.Policy == shared.RuntimeCPUPolicyNone && n.platform != "linux" {
		result := n.result(request, shared.RuntimeCPUStateReleased, nil)
		result.Enforced = true
		return result, nil
	}
	if request.Policy != shared.RuntimeCPUPolicyNone {
		if _, err := n.Prepare(ctx, installationID, cluster, shard, request); err != nil {
			return shared.RuntimeCPUResult{}, err
		}
	} else if n.platform != "linux" {
		return shared.RuntimeCPUResult{}, fmt.Errorf("%w: %s does not provide a verifiable CPU release boundary", ErrUnsupportedPlatform, n.platform)
	}
	identity, err := n.resolveWaiting(ctx, cluster, shard)
	if err != nil {
		return shared.RuntimeCPUResult{}, err
	}
	path := n.cgroupPath(installationID, cluster, shard)
	if request.Policy == shared.RuntimeCPUPolicyNone {
		// A process that was never attached to our cgroup is already released.
		// Moving it to the cgroup root would incorrectly escape an enclosing
		// service boundary (for example a delegated systemd unit).
		if containsPID(path, identity.PID) {
			if err := os.WriteFile(filepath.Join(n.cgroupRoot, "cgroup.procs"), []byte(strconv.FormatInt(int64(identity.PID), 10)), 0o644); err != nil {
				return shared.RuntimeCPUResult{}, fmt.Errorf("release DST process from managed cgroup: %w", err)
			}
		}
		if err := n.removeCgroup(path); err != nil && !os.IsNotExist(err) {
			return shared.RuntimeCPUResult{}, fmt.Errorf("remove released DST cgroup: %w", err)
		}
		if err := n.verifyIdentity(ctx, cluster, shard, identity); err != nil {
			return shared.RuntimeCPUResult{}, err
		}
		result := n.result(request, shared.RuntimeCPUStateReleased, &identity)
		result.Enforced, result.InstanceRunning = true, true
		return result, nil
	}
	if err := os.WriteFile(filepath.Join(path, "cgroup.procs"), []byte(strconv.FormatInt(int64(identity.PID), 10)), 0o644); err != nil {
		return shared.RuntimeCPUResult{}, fmt.Errorf("attach DST process to managed cgroup: %w", err)
	}
	if err := n.verifyIdentity(ctx, cluster, shard, identity); err != nil {
		return shared.RuntimeCPUResult{}, err
	}
	result, err := n.observeCgroup(path, request, &identity)
	if err != nil {
		return shared.RuntimeCPUResult{}, err
	}
	if !containsPID(path, identity.PID) {
		return shared.RuntimeCPUResult{}, ErrCPUNotApplied
	}
	result.State, result.Enforced, result.InstanceRunning = shared.RuntimeCPUStateApplied, true, true
	return result, nil
}

func (n *Native) resolveWaiting(ctx context.Context, cluster, shard string) (ProcessIdentity, error) {
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		identity, err := n.resolve(ctx, n.serverRoot, cluster, shard)
		if err == nil || !errors.Is(err, ErrInstanceNotRunning) {
			return identity, err
		}
		select {
		case <-ctx.Done():
			return ProcessIdentity{}, ctx.Err()
		case <-deadline.C:
			return ProcessIdentity{}, err
		case <-ticker.C:
		}
	}
}

func (n *Native) Observe(ctx context.Context, installationID, cluster, shard string, request shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	request.LogicalCPUIds = SortedUnique(request.LogicalCPUIds)
	if err := validateRequest(request); err != nil {
		return shared.RuntimeCPUResult{}, err
	}
	if request.Policy != shared.RuntimeCPUPolicyNone && n.platform != "linux" {
		return shared.RuntimeCPUResult{}, fmt.Errorf("%w: %s does not provide a verifiable cpuset boundary", ErrUnsupportedPlatform, n.platform)
	}
	identity, err := n.resolve(ctx, n.serverRoot, cluster, shard)
	if errors.Is(err, ErrInstanceNotRunning) {
		if request.Policy == shared.RuntimeCPUPolicyNone {
			return n.result(request, shared.RuntimeCPUStateReleased, nil), nil
		}
		result, observeErr := n.observeCgroup(n.cgroupPath(installationID, cluster, shard), request, nil)
		result.State, result.Enforced = shared.RuntimeCPUStatePrepared, false
		return result, observeErr
	}
	if err != nil {
		return shared.RuntimeCPUResult{}, err
	}
	if request.Policy == shared.RuntimeCPUPolicyNone {
		result := n.result(request, shared.RuntimeCPUStateReleased, &identity)
		result.InstanceRunning = true
		return result, nil
	}
	result, err := n.observeCgroup(n.cgroupPath(installationID, cluster, shard), request, &identity)
	if err != nil {
		return shared.RuntimeCPUResult{}, err
	}
	result.InstanceRunning = true
	if containsPID(n.cgroupPath(installationID, cluster, shard), identity.PID) {
		result.State, result.Enforced = shared.RuntimeCPUStateApplied, true
	} else {
		result.State = shared.RuntimeCPUStatePrepared
	}
	return result, nil
}

func (n *Native) prepareCgroup(path string, cpuIDs []int) error {
	controllers, err := os.ReadFile(filepath.Join(n.cgroupRoot, "cgroup.controllers"))
	if err != nil || !containsWord(string(controllers), "cpu") || !containsWord(string(controllers), "cpuset") {
		return ErrCgroupUnavailable
	}
	subtreePath := filepath.Join(n.cgroupRoot, "cgroup.subtree_control")
	subtree, err := os.ReadFile(subtreePath)
	if err != nil {
		return fmt.Errorf("read cgroup subtree controllers: %w", err)
	}
	missing := make([]string, 0, 2)
	for _, controller := range []string{"cpu", "cpuset"} {
		if !containsWord(string(subtree), controller) {
			missing = append(missing, "+"+controller)
		}
	}
	if len(missing) > 0 {
		if err := os.WriteFile(subtreePath, []byte(strings.Join(missing, " ")), 0o644); err != nil {
			return fmt.Errorf("enable cgroup controllers: %w", err)
		}
	}
	if err := os.Mkdir(path, 0o755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create managed DST cgroup: %w", err)
	}
	mems, err := firstNonEmptyFile(filepath.Join(n.cgroupRoot, "cpuset.mems.effective"), filepath.Join(n.cgroupRoot, "cpuset.mems"))
	if err != nil || strings.TrimSpace(mems) == "" {
		return fmt.Errorf("%w: cpuset memory nodes unavailable", ErrCgroupUnavailable)
	}
	if err := os.WriteFile(filepath.Join(path, "cpuset.mems"), []byte(strings.TrimSpace(mems)), 0o644); err != nil {
		return fmt.Errorf("configure cgroup memory nodes: %w", err)
	}
	if err := os.WriteFile(filepath.Join(path, "cpuset.cpus"), []byte(FormatCPUSet(cpuIDs)), 0o644); err != nil {
		return fmt.Errorf("configure cgroup cpuset: %w", err)
	}
	quota := int64(len(cpuIDs)) * cgroupPeriodMicros
	if err := os.WriteFile(filepath.Join(path, "cpu.max"), []byte(fmt.Sprintf("%d %d", quota, cgroupPeriodMicros)), 0o644); err != nil {
		return fmt.Errorf("configure cgroup CPU quota: %w", err)
	}
	return nil
}

func (n *Native) observeCgroup(path string, request shared.RuntimeCPURequest, identity *ProcessIdentity) (shared.RuntimeCPUResult, error) {
	result := n.result(request, shared.RuntimeCPUStateUnknown, identity)
	effective, err := firstNonEmptyFile(filepath.Join(path, "cpuset.cpus.effective"), filepath.Join(path, "cpuset.cpus"))
	if err != nil {
		return result, fmt.Errorf("read effective cgroup cpuset: %w", err)
	}
	result.EffectiveCPUIds, err = ParseCPUSet(effective)
	if err != nil || !equalInts(result.EffectiveCPUIds, request.LogicalCPUIds) {
		return result, ErrCPUNotApplied
	}
	quota, err := os.ReadFile(filepath.Join(path, "cpu.max"))
	if err != nil {
		return result, fmt.Errorf("read effective cgroup quota: %w", err)
	}
	fields := strings.Fields(string(quota))
	if len(fields) != 2 {
		return result, ErrCPUNotApplied
	}
	result.QuotaMicros, err = strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return result, ErrCPUNotApplied
	}
	result.PeriodMicros, err = strconv.ParseInt(fields[1], 10, 64)
	if err != nil || result.QuotaMicros != int64(len(request.LogicalCPUIds))*cgroupPeriodMicros || result.PeriodMicros != cgroupPeriodMicros {
		return result, ErrCPUNotApplied
	}
	return result, nil
}

func (n *Native) verifyIdentity(ctx context.Context, cluster, shard string, expected ProcessIdentity) error {
	current, err := n.resolve(ctx, n.serverRoot, cluster, shard)
	if err != nil {
		return err
	}
	if current.PID != expected.PID || current.InstanceID != expected.InstanceID {
		return ErrInstanceChanged
	}
	return nil
}

func (n *Native) result(request shared.RuntimeCPURequest, state shared.RuntimeCPUState, identity *ProcessIdentity) shared.RuntimeCPUResult {
	result := shared.RuntimeCPUResult{Policy: request.Policy, LogicalCPUIds: append([]int(nil), request.LogicalCPUIds...), State: state, RuntimeKind: "native", ObservedAt: n.now().UTC()}
	if identity != nil {
		result.PID, result.InstanceID = identity.PID, identity.InstanceID
	}
	return result
}

func (n *Native) cgroupPath(installationID, cluster, shard string) string {
	digest := sha256.Sum256([]byte(installationID + "\x00" + cluster + "\x00" + shard))
	return filepath.Join(n.cgroupRoot, "dst-admin-"+hex.EncodeToString(digest[:12]))
}

func (n *Native) removeCgroup(path string) error {
	if n.regularFilesystem {
		return os.RemoveAll(path)
	}
	return os.Remove(path)
}

func resolveManagedProcess(ctx context.Context, serverRoot, cluster, shard string) (ProcessIdentity, error) {
	matches := make([]shared.ShardProcessReport, 0, 1)
	for _, process := range runtimeinventory.CollectDSTProcesses(ctx) {
		if process.Cluster == cluster && process.Shard == shard && pathInside(serverRoot, process.Executable) {
			matches = append(matches, process)
		}
	}
	if len(matches) == 0 {
		return ProcessIdentity{}, ErrInstanceNotRunning
	}
	if len(matches) != 1 || matches[0].StartedAt == nil {
		return ProcessIdentity{}, errors.New("managed DST process identity is ambiguous")
	}
	value := matches[0]
	return ProcessIdentity{PID: value.PID, InstanceID: fmt.Sprintf("%d:%d", value.PID, value.StartedAt.UnixMilli()), Executable: value.Executable}, nil
}

func pathInside(root, candidate string) bool {
	root, candidate = filepath.Clean(root), filepath.Clean(candidate)
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func validateRequest(request shared.RuntimeCPURequest) error {
	if !shared.IsRuntimeCPUPolicy(request.Policy) {
		return errors.New("invalid CPU policy")
	}
	if request.Policy == shared.RuntimeCPUPolicyNone && len(request.LogicalCPUIds) != 0 || request.Policy != shared.RuntimeCPUPolicyNone && len(request.LogicalCPUIds) == 0 {
		return errors.New("CPU policy and logical CPU selection do not match")
	}
	for _, id := range request.LogicalCPUIds {
		if id < 0 || id > 1048575 {
			return errors.New("logical CPU ID is out of range")
		}
	}
	return nil
}

func SortedUnique(values []int) []int {
	seen := make(map[int]bool, len(values))
	result := make([]int, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Ints(result)
	return result
}

func FormatCPUSet(values []int) string {
	values = SortedUnique(values)
	parts := make([]string, 0, len(values))
	for index := 0; index < len(values); {
		end := index
		for end+1 < len(values) && values[end+1] == values[end]+1 {
			end++
		}
		if end == index {
			parts = append(parts, strconv.Itoa(values[index]))
		} else {
			parts = append(parts, strconv.Itoa(values[index])+"-"+strconv.Itoa(values[end]))
		}
		index = end + 1
	}
	return strings.Join(parts, ",")
}

func ParseCPUSet(value string) ([]int, error) {
	result := make([]int, 0)
	for _, part := range strings.Split(strings.TrimSpace(value), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		left, right, ranged := strings.Cut(part, "-")
		start, err := strconv.Atoi(left)
		if err != nil || start < 0 {
			return nil, errors.New("invalid cpuset")
		}
		end := start
		if ranged {
			end, err = strconv.Atoi(right)
			if err != nil || end < start || end-start > 1048576 {
				return nil, errors.New("invalid cpuset range")
			}
		}
		for id := start; id <= end; id++ {
			result = append(result, id)
		}
	}
	return SortedUnique(result), nil
}

func containsWord(value, expected string) bool {
	for _, word := range strings.Fields(strings.ReplaceAll(value, "+", "")) {
		if word == expected {
			return true
		}
	}
	return false
}

func firstNonEmptyFile(paths ...string) (string, error) {
	var last error
	for _, path := range paths {
		value, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(value)) != "" {
			return string(value), nil
		}
		if err != nil {
			last = err
		}
	}
	if last == nil {
		last = errors.New("file is empty")
	}
	return "", last
}

func containsPID(path string, pid int32) bool {
	value, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
	if err != nil {
		return false
	}
	want := strconv.FormatInt(int64(pid), 10)
	for _, field := range strings.Fields(string(value)) {
		if field == want {
			return true
		}
	}
	return false
}

func equalInts(left, right []int) bool {
	left, right = SortedUnique(left), SortedUnique(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
