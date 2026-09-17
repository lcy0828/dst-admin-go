package mods

import (
	"context"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"dont/internal/operationprogress"

	"github.com/hpcloud/tail"
)

var (
	steamDownloadStartPattern = regexp.MustCompile(`AppID\s+([0-9]+)\s+update started\s*:\s*download\s+([0-9]+)/([0-9]+)`)
	steamStageProgressPattern = regexp.MustCompile(`\bstage\s+([0-9]+)/([0-9]+)`)
	steamCurrentRatePattern   = regexp.MustCompile(`Current download rate:\s*([0-9]+(?:\.[0-9]+)?)\s+Mbps`)
	steamTargetRatePattern    = regexp.MustCompile(`target number of download connections.*\bnow\s+([0-9]+(?:\.[0-9]+)?)\b`)
)

type steamDownloadProgressTracker struct {
	appID          string
	active         bool
	currentBytes   int64
	totalBytes     int64
	bytesPerSecond int64
	sampledBytes   int64
	sampledAt      time.Time
}

func (r *SteamCMDRunner) followDownloadProgress(ctx context.Context, processID int, workshopIDs []string) func() {
	if !operationprogress.Enabled(ctx) {
		return func() {}
	}
	tracker := &steamDownloadProgressTracker{appID: r.AppID, sampledAt: time.Now()}
	operationprogress.Report(ctx, tracker.update())
	baselineBytes := steamWorkshopBytes(r.DownloadRoot, r.AppID, workshopIDs)
	path := strings.TrimSpace(r.ContentLogPath)
	if path == "" {
		path = findSteamContentLog(r.Executable)
	}
	if path == "" {
		path = findSteamProcessContentLog(ctx, processID)
	}
	var stream *tail.Tail
	var lines <-chan *tail.Line
	if path != "" {
		_, statErr := os.Stat(path)
		mustExist := statErr == nil
		whence := os.SEEK_END
		if !mustExist {
			whence = os.SEEK_SET
		}
		opened, err := tail.TailFile(path, tail.Config{
			ReOpen: true, Follow: true, MustExist: mustExist, Poll: false,
			Location: &tail.SeekInfo{Offset: 0, Whence: whence},
			Logger:   tail.DiscardingLogger,
		})
		if err == nil {
			stream = opened
			lines = opened.Lines
		}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case line, ok := <-lines:
				if !ok {
					lines = nil
					continue
				}
				if line != nil && line.Err == nil {
					if update, changed := tracker.consume(line.Text); changed {
						operationprogress.Report(ctx, update)
					}
				}
			case now := <-ticker.C:
				currentBytes := steamWorkshopBytes(r.DownloadRoot, r.AppID, workshopIDs) - baselineBytes
				if currentBytes < 0 {
					currentBytes = 0
				}
				if update, changed := tracker.sample(now, currentBytes); changed {
					operationprogress.Report(ctx, update)
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			if stream != nil {
				_ = stream.Stop()
			}
			<-done
		})
	}
}

func findSteamProcessContentLog(ctx context.Context, processID int) string {
	if runtime.GOOS != "linux" || processID <= 0 {
		return ""
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if executable, err := filepath.EvalSymlinks(fmt.Sprintf("/proc/%d/exe", processID)); err == nil {
			if path := steamProcessContentLogPath(executable); path != "" {
				return path
			}
		}
		select {
		case <-ctx.Done():
			return ""
		case <-timer.C:
			return ""
		case <-ticker.C:
		}
	}
}

func steamProcessContentLogPath(executable string) string {
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return ""
	}
	directory := filepath.Join(filepath.Dir(executable), "logs")
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		return ""
	}
	return filepath.Join(directory, "content_log.txt")
}

func (p *steamDownloadProgressTracker) consume(line string) (operationprogress.Update, bool) {
	if match := steamDownloadStartPattern.FindStringSubmatch(line); len(match) == 4 {
		if match[1] != p.appID {
			return operationprogress.Update{}, false
		}
		current, currentErr := strconv.ParseInt(match[2], 10, 64)
		total, totalErr := strconv.ParseInt(match[3], 10, 64)
		if currentErr != nil || totalErr != nil || current < 0 || total <= 0 {
			return operationprogress.Update{}, false
		}
		if current > total {
			current = total
		}
		if stage := steamStageProgressPattern.FindStringSubmatch(line); len(stage) == 3 {
			stageCurrent, currentErr := strconv.ParseInt(stage[1], 10, 64)
			stageTotal, totalErr := strconv.ParseInt(stage[2], 10, 64)
			if currentErr == nil && totalErr == nil && stageCurrent >= 0 && stageTotal > 0 {
				if stageCurrent > stageTotal {
					stageCurrent = stageTotal
				}
				current, total = stageCurrent, stageTotal
			}
		}
		p.active = true
		p.currentBytes = current
		p.totalBytes = total
		p.bytesPerSecond = 0
		return p.update(), true
	}
	if strings.Contains(line, "AppID "+p.appID+" scheduler finished") {
		p.active = false
		return operationprogress.Update{}, false
	}
	if !p.active {
		return operationprogress.Update{}, false
	}
	match := steamCurrentRatePattern.FindStringSubmatch(line)
	if len(match) != 2 {
		match = steamTargetRatePattern.FindStringSubmatch(line)
	}
	if len(match) != 2 {
		return operationprogress.Update{}, false
	}
	mbps, err := strconv.ParseFloat(match[1], 64)
	if err != nil || mbps < 0 {
		return operationprogress.Update{}, false
	}
	bytesPerSecond := int64(math.Round(mbps * 1_000_000 / 8))
	if bytesPerSecond == p.bytesPerSecond {
		return operationprogress.Update{}, false
	}
	p.bytesPerSecond = bytesPerSecond
	return p.update(), true
}

func (p *steamDownloadProgressTracker) sample(now time.Time, currentBytes int64) (operationprogress.Update, bool) {
	if currentBytes < 0 || currentBytes <= p.sampledBytes {
		return operationprogress.Update{}, false
	}
	elapsed := now.Sub(p.sampledAt)
	if p.sampledAt.IsZero() || elapsed <= 0 {
		p.sampledBytes = currentBytes
		p.sampledAt = now
		return operationprogress.Update{}, false
	}
	p.bytesPerSecond = int64(float64(currentBytes-p.sampledBytes) / elapsed.Seconds())
	p.sampledBytes = currentBytes
	p.sampledAt = now
	p.currentBytes = currentBytes
	if p.totalBytes > 0 && p.currentBytes > p.totalBytes {
		p.currentBytes = p.totalBytes
	}
	return p.update(), true
}

func (p *steamDownloadProgressTracker) update() operationprogress.Update {
	percent := 5
	if p.totalBytes > 0 {
		percent += int(p.currentBytes * 79 / p.totalBytes)
	}
	return operationprogress.Update{
		Stage:          operationprogress.StageModCache,
		Percent:        percent,
		Message:        "正在通过 SteamCMD 下载模组文件",
		CurrentBytes:   p.currentBytes,
		TotalBytes:     p.totalBytes,
		BytesPerSecond: p.bytesPerSecond,
	}
}

func steamWorkshopBytes(downloadRoot, appID string, workshopIDs []string) int64 {
	var total int64
	for _, workshopID := range uniqueModIDs(workshopIDs) {
		for _, phase := range []string{"downloads", "content"} {
			root := filepath.Join(downloadRoot, "steamapps", "workshop", phase, appID, workshopID)
			_ = filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
				if err != nil || entry == nil || !entry.Type().IsRegular() {
					return nil
				}
				if info, infoErr := entry.Info(); infoErr == nil && info.Size() > 0 {
					total += info.Size()
				}
				return nil
			})
		}
	}
	return total
}

func findSteamContentLog(executable string) string {
	home, _ := os.UserHomeDir()
	candidates := make([]string, 0, 8)
	if executable = strings.TrimSpace(executable); executable != "" {
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), "logs", "content_log.txt"))
	}
	if runtime.GOOS == "darwin" && home != "" {
		candidates = append(candidates, filepath.Join(home, "Library", "Application Support", "Steam", "logs", "content_log.txt"))
	}
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".local", "share", "Steam", "logs", "content_log.txt"),
			filepath.Join(home, ".steam", "steam", "logs", "content_log.txt"),
			filepath.Join(home, ".steam", "logs", "content_log.txt"),
			filepath.Join(home, "Steam", "logs", "content_log.txt"),
		)
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}
