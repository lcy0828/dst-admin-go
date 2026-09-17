package modcontrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"dont/internal/modpublication"
	"dont/internal/operationprogress"
	"dont/shared"
)

const installationDownloadConcurrency = 3

type installationModDownload struct {
	placement modpublication.AppliedPlacement
	modIDs    []string
}

type modDownloadOutput struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *modDownloadOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(data)
}

func (s *Service) downloadRoomInstallations(ctx context.Context, targets map[string]modpublication.AppliedPlacement, modIDs []string, output io.Writer) (int, int, error) {
	groups := make(map[string]installationModDownload, len(targets))
	for key, placement := range targets {
		groups[key] = installationModDownload{placement: placement, modIDs: modIDs}
	}
	return s.downloadInstallationGroups(ctx, groups, output)
}

func (s *Service) downloadInstallationGroups(ctx context.Context, targets map[string]installationModDownload, output io.Writer) (int, int, error) {
	keys := make([]string, 0, len(targets))
	for key := range targets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > 1 {
		output = &modDownloadOutput{writer: output}
	}
	progress := make([]operationprogress.Update, len(keys))
	for index, key := range keys {
		group := targets[key]
		for _, id := range group.modIDs {
			progress[index].Items = append(progress[index].Items, shared.ModDownloadProgress{WorkshopID: id, TargetID: group.placement.TargetID, InstallationID: group.placement.InstallationID, Status: "queued"})
		}
	}
	ready := make([]bool, len(keys))
	errorsByTarget := make([]error, len(keys))
	var mu sync.Mutex
	download := func(index int) {
		group := targets[keys[index]]
		placement, modIDs := group.placement, group.modIDs
		report := func(update operationprogress.Update) {
			mu.Lock()
			defer mu.Unlock()
			update.Percent = max(progress[index].Percent, min(100, max(0, update.Percent)))
			if len(update.Items) == 0 {
				update.Items = progress[index].Items
			}
			progress[index] = update
			// Bytes and item ordinal describe this installation's current mod, not
			// an aggregate across differently sized downloads on other machines.
			combined := update
			combined.Stage = operationprogress.StageModCache
			combined.TargetID, combined.InstallationID = placement.TargetID, placement.InstallationID
			combined.Message = fmt.Sprintf("%s/%s: %s", placement.TargetID, placement.InstallationID, update.Message)
			combined.Percent = 0
			combined.Items = nil
			for _, item := range progress {
				combined.Percent += item.Percent
				combined.Items = append(combined.Items, item.Items...)
			}
			combined.Percent /= len(keys)
			operationprogress.Report(ctx, combined)
		}
		childCtx := operationprogress.WithReporter(ctx, report)
		if err := ctx.Err(); err != nil {
			errorsByTarget[index] = err
			return
		}
		report(operationprogress.Update{Message: "正在检查已有模组"})
		current, inspectErr := s.installationModsCurrent(childCtx, placement.TargetID, placement.InstallationID, modIDs)
		if inspectErr != nil {
			_, _ = fmt.Fprintf(output, "无法确认 %s/%s 的现有模组版本，将继续下载校验: %v\n", placement.TargetID, placement.InstallationID, inspectErr)
		}
		if current {
			if err := s.installer.LinkInstallationMods(childCtx, placement.TargetID, placement.InstallationID, modIDs); err != nil {
				errorsByTarget[index] = fmt.Errorf("%s/%s 本地模组准备失败 (Workshop %s): %w", placement.TargetID, placement.InstallationID, strings.Join(modIDs, ", "), err)
				report(operationprogress.Update{Message: err.Error()})
				return
			}
			ready[index] = true
			items := make([]shared.ModDownloadProgress, len(modIDs))
			for i, id := range modIDs {
				items[i] = shared.ModDownloadProgress{WorkshopID: id, TargetID: placement.TargetID, InstallationID: placement.InstallationID, Status: "succeeded", Message: "已有最新文件"}
			}
			report(operationprogress.Update{Percent: 100, Message: "已有最新模组，跳过下载", Items: items})
			return
		}
		_, err := s.installer.UpdateInstallationMods(childCtx, placement.TargetID, placement.InstallationID, modIDs, output)
		if err != nil {
			errorsByTarget[index] = fmt.Errorf("%s/%s 下载模组失败 (Workshop %s): %w", placement.TargetID, placement.InstallationID, strings.Join(modIDs, ", "), err)
			report(operationprogress.Update{Message: err.Error()})
			return
		}
		report(operationprogress.Update{Percent: 100, Message: "下载完成"})
	}
	if len(keys) == 1 {
		download(0)
	} else {
		var workers sync.WaitGroup
		count := min(installationDownloadConcurrency, len(keys))
		for worker := 0; worker < count; worker++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for index := worker; index < len(keys); index += count {
					download(index)
				}
			}()
		}
		workers.Wait()
	}
	downloaded, current := 0, 0
	for index, err := range errorsByTarget {
		if err != nil {
			continue
		}
		if ready[index] {
			current++
		} else {
			downloaded++
		}
	}
	if err := errors.Join(errorsByTarget...); err != nil {
		return downloaded, current, fmt.Errorf("%d/%d 个安装实例已就绪，未修改房间配置: %w", downloaded+current, len(keys), err)
	}
	return downloaded, current, nil
}
