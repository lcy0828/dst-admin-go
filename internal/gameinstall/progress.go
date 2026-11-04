package gameinstall

import (
	"context"
	"dont/internal/steamupdate"
)

// Installation and updates share one bounded SteamCMD progress parser.
type installOutput struct {
	tail     limitedOutput
	progress *steamupdate.ProgressWriter
}

func newInstallOutput(ctx context.Context) *installOutput {
	out := &installOutput{}
	out.progress = steamupdate.NewProgressWriter(ctx, &out.tail)
	return out
}
func (w *installOutput) Write(p []byte) (int, error) { return w.progress.Write(p) }
func (w *installOutput) finish() string              { w.progress.Flush(); return w.tail.text }
