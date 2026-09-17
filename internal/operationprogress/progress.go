package operationprogress

import (
	"context"
	"dont/shared"
)

const (
	StageStartConfiguration = "start.configuration"
	StageStartRouting       = "start.routing"
	StageStartMods          = "start.mods"
	StageModInspect         = "mod.inspect"
	StageModCache           = "mod.cache"
	StageModPrepare         = "mod.prepare"
	StageModPublish         = "mod.publish"
	StageModComplete        = "mod.complete"
	StageModDone            = "mod.done"
)

type Update struct {
	Stage          string
	Percent        int
	Message        string
	WorkshopID     string
	CurrentItem    int
	TotalItems     int
	Items          []shared.ModDownloadProgress
	Worlds         []shared.WorldOperationProgress
	TargetID       string
	InstallationID string
	CurrentBytes   int64
	TotalBytes     int64
	BytesPerSecond int64
}

type Reporter func(Update)

type reporterContextKey struct{}

func WithReporter(ctx context.Context, reporter Reporter) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if reporter == nil {
		return ctx
	}
	return context.WithValue(ctx, reporterContextKey{}, reporter)
}

func Report(ctx context.Context, update Update) {
	if ctx == nil {
		return
	}
	reporter, _ := ctx.Value(reporterContextKey{}).(Reporter)
	if reporter != nil {
		reporter(update)
	}
}

func Enabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	reporter, _ := ctx.Value(reporterContextKey{}).(Reporter)
	return reporter != nil
}
