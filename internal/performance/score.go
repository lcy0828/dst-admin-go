package performance

import (
	"math"
	"time"
)

func finite(v *float64) bool    { return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) }
func number(v float64) *float64 { return &v }

// Score measures simulated seconds per producer capture wall second. CPU is
// deliberately independent: 100% CPU means one logical core, not a bad score.
func score(previous *Input, in Input, interval time.Duration) Point {
	p := Point{RoomID: in.RoomID, WorldID: in.WorldID, WorldName: in.WorldName, TargetID: in.TargetID, InstallationID: in.InstallationID, InstanceID: in.InstanceID, At: in.At, State: in.State, RSSBytes: in.RSSBytes, NativePerformance: in.NativePerformance, Samples: 1, ScoreVersion: ScoreVersion}
	if in.State != "running" {
		p.Reason = "NOT_RUNNING"
		return p
	}
	// Resource measurements use the process clock and remain meaningful while
	// simulation time is paused. Keep their validity independent of scoring.
	if previous != nil {
		old := *previous
		if in.InstanceID != "" && in.InstanceID == old.InstanceID && in.TargetID == old.TargetID && in.InstallationID == old.InstallationID && old.State == "running" && in.ProcessID == old.ProcessID && in.ProcessStartedAt.Equal(old.ProcessStartedAt) && !in.ProcessStartedAt.IsZero() && finite(in.CPUSeconds) && finite(old.CPUSeconds) {
			wall := in.CPUSampledAt.Sub(old.CPUSampledAt).Seconds()
			used := *in.CPUSeconds - *old.CPUSeconds
			if wall > 0 && wall <= 3*interval.Seconds() && used >= 0 && used/wall <= 1024 {
				p.CPUPercent = number(100 * used / wall)
			}
		}
	}
	if in.Paused {
		p.State = "paused"
		p.Reason = "PAUSED"
		if previous != nil && previous.Paused && previous.InstanceID == in.InstanceID && in.InstanceID != "" && previous.TargetID == in.TargetID && previous.InstallationID == in.InstallationID {
			delta := in.At.Sub(previous.At).Seconds()
			if delta > 0 && delta <= 3*interval.Seconds() {
				p.WindowSeconds = delta
			}
		}
		return p
	}
	p.Reason = "WARMING_UP"
	if previous == nil {
		return p
	}
	old := *previous
	delta := in.At.Sub(old.At).Seconds()
	if delta <= 0 || delta > 3*interval.Seconds() || in.InstanceID == "" || in.InstanceID != old.InstanceID || in.TargetID != old.TargetID || in.InstallationID != old.InstallationID || old.State != "running" || old.Paused {
		return p
	}
	p.WindowSeconds = delta
	if !finite(in.SimulationSeconds) || !finite(in.CaptureSeconds) || !finite(in.TimeScale) || !finite(old.SimulationSeconds) || !finite(old.CaptureSeconds) || !finite(old.TimeScale) {
		p.Reason = "SIMULATION_CLOCK_UNAVAILABLE"
		return p
	}
	wall := *in.CaptureSeconds - *old.CaptureSeconds
	sim := *in.SimulationSeconds - *old.SimulationSeconds
	if *in.TimeScale <= 0 || *in.TimeScale > 100 || *in.TimeScale != *old.TimeScale {
		p.Reason = "TIME_SCALE_CHANGED"
		return p
	}
	if wall < 5 || wall > 3*interval.Seconds() || sim < 0 || math.Abs(wall-delta) > math.Max(5, delta*0.25) {
		p.Reason = "CLOCK_DISCONTINUITY"
		return p
	}
	if !in.ObservedAt.IsZero() && !old.ObservedAt.IsZero() {
		observed := in.ObservedAt.Sub(old.ObservedAt).Seconds()
		if observed <= 0 || math.Abs(wall-observed) > math.Max(5, observed*0.25) {
			p.Reason = "CLOCK_DISCONTINUITY"
			return p
		}
	}
	// Pause and native empty-server sleep must never become a fabricated zero.
	if sim == 0 {
		p.State = "idle"
		p.Reason = "SIMULATION_NOT_ADVANCING"
		return p
	}
	ratio := sim / (wall * (*in.TimeScale))
	if ratio > 1.5 {
		p.Reason = "CLOCK_DISCONTINUITY"
		return p
	}
	value := math.Round(math.Min(1, ratio)*1000) / 10
	p.Score = number(value)
	p.MinimumScore = number(value)
	p.SpeedRatio = number(ratio)
	p.ValidSeconds = math.Min(wall, delta)
	p.Reason = ""
	if value < 80 {
		p.LowSeconds = p.ValidSeconds
	}
	return p
}
