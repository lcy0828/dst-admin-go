package moddiagnostics

import (
	"regexp"
	"slices"
	"strings"
	"time"

	"dont/internal/logstream"
)

type Input struct {
	Observation Observation
	Logs        *logstream.Snapshot
	LatestStart *time.Time
	IncidentAt  *time.Time
	Baseline    *Baseline
	IncludeLogs bool
}

var (
	errorPattern      = regexp.MustCompile(`(?i)(\b(lua error|mod error|error loading|error during|assertion failed|stack traceback|segmentation fault|failed to load|failed to save file)\b|\[critical\]|\battempt to (call|index|perform|concatenate)\b)`)
	stackFramePattern = regexp.MustCompile(`\.lua["\]]*:[0-9]+`)
	modPathPattern    = regexp.MustCompile(`(?i)(?:^|[\\/"\s])mods[\\/]([^\\/"\r\n]+)[\\/][^"\r\n]*\.lua["\]]*:[0-9]+`)
	secretPattern     = regexp.MustCompile(`(?i)(token|password|passwd|authorization|cookie|secret|security_key|steam_login)\s*[=:]|\+login\s|https?://[^\s/@]+:[^\s/@]+@`)
	playerPattern     = regexp.MustCompile(`KU_[A-Za-z0-9_-]+|\b7656119[0-9]{10}\b|\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)
)

func Analyze(in Input) Report {
	o := in.Observation
	r := Report{Status: "no_known_issue", Observation: o, Baseline: in.Baseline,
		Changes: []Change{}, Findings: []Finding{}, Evidence: []Evidence{}, LogCorrelation: "not_requested",
		Limitations:     append([]string{"NOT_A_COMPATIBILITY_GUARANTEE", "DECLARED_METADATA_ONLY"}, o.Limitations...),
		Recommendations: []string{}}
	enabled := map[string]bool{}
	for _, m := range o.Mods {
		enabled[m.ID] = true
	}
	for _, m := range o.Mods {
		if m.DSTCompatible != nil && !*m.DSTCompatible {
			r.Findings = append(r.Findings, Finding{Code: "DECLARED_NOT_DST", Kind: "observed", ModIDs: []string{m.ID}})
		}
		if m.FileStatus == "missing" || m.FileStatus == "invalid" || m.FileStatus == "corrupt" {
			r.Findings = append(r.Findings, Finding{Code: "MOD_FILES_UNAVAILABLE", Kind: "observed", ModIDs: []string{m.ID}})
		}
		for _, dep := range m.Dependencies {
			if slices.Contains(o.Limitations, "MOD_LIMIT_REACHED") {
				break
			}
			if !enabled[dep] {
				r.Findings = append(r.Findings, Finding{Code: "DECLARED_DEPENDENCY_NOT_ENABLED", Kind: "suspected", ModIDs: []string{m.ID, dep}})
			}
		}
	}
	compareBaseline(&r)
	if in.IncludeLogs {
		r.LogCorrelation = "unknown"
		r.Limitations = append(r.Limitations, "CURRENT_FILES_NOT_START_SNAPSHOT")
		r.IncidentAt = in.IncidentAt
		if in.Logs == nil {
			r.Limitations = append(r.Limitations, "LOG_UNAVAILABLE")
		} else {
			log := *in.Logs
			if !log.StartedAt.IsZero() {
				t := log.StartedAt.UTC()
				r.LogStartedAt = &t
				if in.LatestStart != nil {
					switch {
					case log.StartedAt.Before(in.LatestStart.Add(-time.Second)):
						r.LogCorrelation = "historical"
					case !log.StartedAt.After(in.LatestStart.Add(3 * time.Minute)):
						r.LogCorrelation = "matched"
					}
				}
			}
			if r.LogCorrelation == "historical" {
				r.Limitations = append(r.Limitations, "LOG_FROM_PREVIOUS_START")
			} else {
				r.Evidence = errorBlocks(log.Lines)
				for _, e := range r.Evidence {
					r.Findings = append(r.Findings, Finding{Code: e.Code, Kind: "observed"})
					if len(e.ModIDs) > 0 {
						r.Findings = append(r.Findings, Finding{Code: "MOD_IN_ERROR_STACK", Kind: "suspected", ModIDs: e.ModIDs})
					}
				}
			}
			if log.Truncated {
				r.Limitations = append(r.Limitations, "LOG_TAIL_ONLY")
			}
			if r.LogCorrelation == "unknown" {
				r.Limitations = append(r.Limitations, "START_CORRELATION_UNKNOWN")
			}
		}
	}
	if len(r.Limitations) > 2 {
		r.Status = "insufficient_evidence"
	}
	for _, f := range r.Findings {
		if f.Kind == "suspected" && r.Status != "observed_problem" {
			r.Status = "suspected"
		}
		if f.Kind == "observed" {
			r.Status = "observed_problem"
		}
	}
	if len(r.Evidence) > 0 {
		r.Recommendations = append(r.Recommendations, "REVIEW_FIRST_ERROR_AND_STACK")
	}
	if slices.ContainsFunc(r.Findings, func(f Finding) bool { return f.Code == "MOD_IN_ERROR_STACK" }) {
		r.Limitations = append(r.Limitations, "STACK_REFERENCE_IS_NOT_CAUSATION")
		r.Recommendations = append(r.Recommendations, "CHECK_MOD_AUTHOR_COMPATIBILITY")
	}
	if r.LogCorrelation == "historical" || in.IncludeLogs && len(r.Evidence) == 0 {
		r.Recommendations = append(r.Recommendations, "COLLECT_MATCHING_START_LOG")
	}
	if len(o.Mods) > 0 {
		r.Recommendations = append(r.Recommendations, "KEEP_BACKUP_BEFORE_GAME_UPDATE")
	}
	r.Limitations = unique(r.Limitations)
	return r
}

func errorBlocks(lines []logstream.Line) []Evidence {
	result := []Evidence{}
	lastEnd := -1
	for i, line := range lines {
		if i <= lastEnd || strings.Contains(line.Text, "RemoteCommandInput:") || !errorPattern.MatchString(line.Text) {
			continue
		}
		if len(result) == MaxEvidence {
			break
		}
		first := max(0, i-3)
		end := min(len(lines), first+MaxContextLines)
		// A later error belongs to its own block; keep intervening stack lines.
		for j := i + 1; j < end; j++ {
			if errorPattern.MatchString(lines[j].Text) && !strings.Contains(strings.ToLower(lines[j].Text), "stack traceback") && !stackFramePattern.MatchString(lines[j].Text) {
				end = j
				break
			}
		}
		e := Evidence{Code: "GAME_LOG_ERROR", FirstCursor: lines[first].Cursor, ErrorCursor: line.Cursor, Lines: []string{}, ModIDs: []string{}, Truncated: end < len(lines) && end-first >= MaxContextLines}
		if strings.Contains(strings.ToLower(line.Text), "failed to save file") {
			e.Code = "SAVE_WRITE_ERROR"
		}
		for j := first; j < end; j++ {
			// Only the error and following stack can implicate a mod. Preceding
			// routine mod-load messages are context, not attribution evidence.
			if j >= i && !strings.Contains(lines[j].Text, "RemoteCommandInput:") {
				for _, match := range modPathPattern.FindAllStringSubmatch(lines[j].Text, -1) {
					id := strings.TrimPrefix(match[1], "workshop-")
					if id == match[1] {
						id = "local:" + id
					}
					e.ModIDs = append(e.ModIDs, id)
				}
			}
			if len(lines[j].Text) > 1024 {
				e.Truncated = true
			}
			e.Lines = append(e.Lines, redact(lines[j].Text))
		}
		e.ModIDs = unique(e.ModIDs)
		result = append(result, e)
		lastEnd = end - 1
	}
	return result
}

func redact(line string) string {
	if secretPattern.MatchString(line) {
		return "[redacted: possible credential]"
	}
	line = playerPattern.ReplaceAllString(line, "[redacted]")
	if len(line) > 1024 {
		line = strings.ToValidUTF8(line[:1024], "") + "…"
	}
	return line
}

func compareBaseline(r *Report) {
	if r.Baseline == nil {
		r.Limitations = append(r.Limitations, "NO_PRE_UPDATE_BASELINE")
		return
	}
	b, o := r.Baseline.Observation, r.Observation
	if b.TargetID != o.TargetID || b.InstallationID != o.InstallationID {
		r.Limitations = append(r.Limitations, "BASELINE_PLACEMENT_CHANGED")
		return
	}
	add := func(field, id, before, after string) {
		if before != after {
			r.Changes = append(r.Changes, Change{Field: field, ModID: id, Before: before, After: after})
		}
	}
	if b.Version.AppID != "" && b.Version.AppID == o.Version.AppID {
		if b.Version.SteamBuild != "" && o.Version.SteamBuild != "" {
			add("steamBuild", "", b.Version.SteamBuild, o.Version.SteamBuild)
		}
		if b.Version.GameVersion != "" && o.Version.GameVersion != "" {
			add("gameVersion", "", b.Version.GameVersion, o.Version.GameVersion)
		}
	} else {
		r.Limitations = append(r.Limitations, "VERSION_NOT_COMPARABLE")
	}
	if b.ConfigurationRevision != "" && o.ConfigurationRevision != "" {
		add("configurationRevision", "", b.ConfigurationRevision, o.ConfigurationRevision)
	}
	for _, code := range []string{"RUNTIME_UNAVAILABLE", "PLACEMENT_CHANGED"} {
		if slices.Contains(b.Limitations, code) || slices.Contains(o.Limitations, code) {
			r.Limitations = append(r.Limitations, "MOD_COMPARISON_INCOMPLETE")
			return
		}
	}
	before := map[string]Mod{}
	for _, m := range b.Mods {
		before[m.ID] = m
	}
	if slices.Contains(b.Limitations, "MOD_FACTS_UNAVAILABLE") || slices.Contains(o.Limitations, "MOD_FACTS_UNAVAILABLE") || slices.Contains(b.Limitations, "MOD_LIMIT_REACHED") || slices.Contains(o.Limitations, "MOD_LIMIT_REACHED") {
		r.Limitations = append(r.Limitations, "MOD_COMPARISON_INCOMPLETE")
		return
	}
	for _, m := range o.Mods {
		if old, ok := before[m.ID]; ok {
			if old.Version != "" && m.Version != "" {
				add("modVersion", m.ID, old.Version, m.Version)
			}
			if old.Manifest != "" && m.Manifest != "" {
				add("modManifest", m.ID, old.Manifest, m.Manifest)
			}
			delete(before, m.ID)
		} else {
			add("modEnabled", m.ID, "false", "true")
		}
	}
	for _, id := range sortedKeys(before) {
		add("modEnabled", id, "true", "false")
	}
}

func unique(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, item := range in {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
