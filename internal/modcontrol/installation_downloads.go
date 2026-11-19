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
	"dont/internal/mods"
	"dont/internal/operationprogress"
	"dont/internal/runtimedriver"
	"dont/shared"
)

const installationDownloadConcurrency = 3

type installationModDownload struct {
	placement      modpublication.AppliedPlacement
	modIDs         []string
	reuseInstalled bool
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
	return s.prepareRoomModFiles(ctx, targets, modIDs, output, false)
}

func (s *Service) prepareRoomModFiles(ctx context.Context, targets map[string]modpublication.AppliedPlacement, modIDs []string, output io.Writer, reuseInstalled bool) (int, int, error) {
	groups := make(map[string]installationModDownload, len(targets))
	for key, placement := range targets {
		groups[key] = installationModDownload{placement: placement, modIDs: modIDs, reuseInstalled: reuseInstalled}
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
		var reusedItems []shared.ModDownloadProgress
		checkpoint := func(status, message string) error {
			return mods.RecordUpdateTarget(ctx, mods.ModUpdateTarget{TargetID: placement.TargetID, InstallationID: placement.InstallationID, ModIDs: modIDs, Status: status, Message: message})
		}
		report := func(update operationprogress.Update) {
			mu.Lock()
			defer mu.Unlock()
			update.Percent = max(progress[index].Percent, min(100, max(0, update.Percent)))
			if len(update.Items) == 0 {
				update.Items = progress[index].Items
			} else if len(reusedItems) > 0 {
				update.Items = append(append([]shared.ModDownloadProgress(nil), reusedItems...), update.Items...)
			}
			progress[index] = update
			// Bytes and item ordinal describe this installation's current mod, not
			// an aggregate across differently sized downloads on other machines.
			combined := update
			if combined.Stage != operationprogress.StageModInspect {
				combined.Stage = operationprogress.StageModCache
			}
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
		report(operationprogress.Update{Stage: operationprogress.StageModInspect, Message: "正在检查已有模组"})
		current := false
		var inspectErr error
		if group.reuseInstalled {
			installed, missing, err := s.roomModFiles(childCtx, placement, modIDs)
			if err != nil {
				errorsByTarget[index] = fmt.Errorf("%s/%s 无法读取机器已有模组: %w", placement.TargetID, placement.InstallationID, err)
				return
			}
			if len(missing) == 0 {
				current = true
			} else {
				if len(installed) > 0 {
					if err := s.installer.LinkInstallationMods(childCtx, placement.TargetID, placement.InstallationID, installed); err != nil {
						errorsByTarget[index] = fmt.Errorf("%s/%s 本地模组准备失败: %w", placement.TargetID, placement.InstallationID, err)
						return
					}
					items := readyModProgress(placement, installed, "使用已有文件")
					for _, id := range missing {
						items = append(items, shared.ModDownloadProgress{WorkshopID: id, TargetID: placement.TargetID, InstallationID: placement.InstallationID, Status: "queued"})
					}
					report(operationprogress.Update{Stage: operationprogress.StageModInspect, Message: "已有文件已就绪，正在准备补下载", Items: items})
					reusedItems = items[:len(installed)]
				}
				modIDs = missing
			}
		} else {
			current, inspectErr = s.installationModsCurrent(childCtx, placement.TargetID, placement.InstallationID, modIDs)
		}
		if previous := mods.PreviousUpdateTarget(ctx, placement.TargetID, placement.InstallationID); previous != nil &&
			(previous.Status == "downloading" || previous.Status == "uncertain") {
			verified := false
			if current {
				verified, inspectErr = s.installationModsCurrent(childCtx, placement.TargetID, placement.InstallationID, modIDs, true)
			}
			if !verified {
				errorsByTarget[index] = fmt.Errorf("%s/%s 上次下载结果尚未确认，未重复下发，请核对原任务", placement.TargetID, placement.InstallationID)
				return
			}
		}
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
			if err := checkpoint("verified", "已确认安装版本"); err != nil {
				errorsByTarget[index] = err
				return
			}
			message := "已有最新模组，跳过下载"
			itemMessage := "已有最新文件"
			stage := operationprogress.StageModCache
			if group.reuseInstalled {
				message = "模组文件已就绪"
				itemMessage = "使用已有文件"
				stage = operationprogress.StageModInspect
			}
			report(operationprogress.Update{Stage: stage, Percent: 100, Message: message, Items: readyModProgress(placement, modIDs, itemMessage)})
			return
		}
		if mods.TracksUpdate(ctx) && inspectErr != nil {
			errorsByTarget[index] = inspectErr
			return
		}
		if err := checkpoint("downloading", "下载已开始，等待校验"); err != nil {
			errorsByTarget[index] = err
			return
		}
		_, err := s.installer.UpdateInstallationMods(childCtx, placement.TargetID, placement.InstallationID, modIDs, output)
		if err != nil {
			status := "failed"
			if errors.Is(err, runtimedriver.ErrModDownloadUncertain) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				status = "uncertain"
			}
			err = errors.Join(err, checkpoint(status, err.Error()))
			errorsByTarget[index] = fmt.Errorf("%s/%s 下载模组失败 (Workshop %s): %w", placement.TargetID, placement.InstallationID, strings.Join(modIDs, ", "), err)
			report(operationprogress.Update{Message: err.Error()})
			return
		}
		if mods.TracksUpdate(ctx) {
			verified, verifyErr := s.installationModsCurrent(childCtx, placement.TargetID, placement.InstallationID, modIDs)
			if verifyErr != nil || !verified {
				err := errors.Join(errors.New("下载已结束，但尚未确认已安装版本"), verifyErr)
				errorsByTarget[index] = errors.Join(err, checkpoint("uncertain", err.Error()))
				return
			}
		}
		if err := checkpoint("verified", "下载和版本校验完成"); err != nil {
			errorsByTarget[index] = err
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

func (s *Service) roomModFiles(ctx context.Context, placement modpublication.AppliedPlacement, modIDs []string) (installed, missing []string, err error) {
	if s.runtimeFiles == nil {
		return nil, nil, errors.New("运行机器模组清单不可用")
	}
	observed, err := s.runtimeFiles.ObserveRuntimeModFiles(ctx, RuntimeFileRequest{
		TargetID: placement.TargetID, InstallationID: placement.InstallationID,
		TopologyRevision: "room-mod-install-v1", WorkshopIDs: modIDs,
	})
	if err != nil {
		return nil, nil, err
	}
	if observed == nil || observed.InstallationID != placement.InstallationID {
		return nil, nil, errors.New("运行机器返回的 Mod 安装目录清单无效")
	}
	for _, id := range modIDs {
		switch observed.Mods[id].Status {
		case shared.RuntimeModFileReady:
			installed = append(installed, id)
		case shared.RuntimeModFileMissing:
			missing = append(missing, id)
		case shared.RuntimeModFileInvalid:
			return nil, nil, fmt.Errorf("模组 %s 的文件不完整，请先修复机器模组", id)
		default:
			return nil, nil, fmt.Errorf("无法确认模组 %s 的文件可用", id)
		}
	}
	return installed, missing, nil
}

func readyModProgress(placement modpublication.AppliedPlacement, ids []string, message string) []shared.ModDownloadProgress {
	items := make([]shared.ModDownloadProgress, len(ids))
	for i, id := range ids {
		items[i] = shared.ModDownloadProgress{WorkshopID: id, TargetID: placement.TargetID, InstallationID: placement.InstallationID, Status: "succeeded", Message: message}
	}
	return items
}
