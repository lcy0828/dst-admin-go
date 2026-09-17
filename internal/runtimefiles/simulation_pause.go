package runtimefiles

import (
	"bytes"
	"io"
	"os"
	"regexp"
	"sync"
)

// Only complete engine log records count; echoed console commands do not.
var simulationRecord = regexp.MustCompile(`(?m)^\[(?:[0-9]{2,9}:[0-5][0-9]:[0-5][0-9]|[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-5][0-9]:[0-5][0-9])\]:[ \t]*(Sim paused|Sim unpaused|Starting Up|Current time: [^\r\n]{1,64})[ \t]*\r?\n`)

const simulationScanBytes = 64 * 1024
const simulationRecordOverlap = 256

// SimulationPauseLog retains only the read position and last observation for
// one log/process. Callers must first establish that the log is from the live
// process, and include its startup identity in instanceID.
type SimulationPauseLog struct {
	mu         sync.Mutex
	instanceID string
	info       os.FileInfo
	paused     *bool
}

func (s *SimulationPauseLog) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.instanceID, s.info, s.paused = "", nil, nil
}

// Read reuses the status reader's tail. Only a first read or a gap preceding
// that tail needs additional disk reads; unchanged logs need no history scan.
// Missing or replaced evidence never defaults to unpaused.
func (s *SimulationPauseLog) Read(path, instanceID string, info os.FileInfo, tail []byte) *bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	previousInfo, previousID, previousPause := s.info, s.instanceID, s.paused
	s.instanceID, s.info, s.paused = "", nil, nil
	if instanceID == "" || info == nil || !info.Mode().IsRegular() {
		return nil
	}
	var lower int64
	if previousID == instanceID && previousInfo != nil && os.SameFile(previousInfo, info) &&
		info.Size() >= previousInfo.Size() && !info.ModTime().Before(previousInfo.ModTime()) {
		if info.Size() == previousInfo.Size() && info.ModTime().Equal(previousInfo.ModTime()) {
			s.instanceID, s.info, s.paused = instanceID, info, previousPause
			return previousPause
		}
		if info.Size() > previousInfo.Size() {
			lower = max(0, previousInfo.Size()-simulationRecordOverlap)
			s.paused = previousPause
		}
	}
	if pause, found := latestSimulationPause(tail, info.Size() <= int64(len(tail))); found {
		s.paused = pause
	} else {
		var end int64
		if info.Size() > int64(len(tail)) {
			end = min(info.Size(), info.Size()-int64(len(tail))+simulationRecordOverlap)
		}
		if end > lower {
			file, err := os.Open(path)
			if err != nil {
				s.paused = nil
				return nil
			}
			defer file.Close()
			opened, err := file.Stat()
			if err != nil || !os.SameFile(info, opened) || opened.Size() < info.Size() {
				s.paused = nil
				return nil
			}
			pause, found, err := scanSimulationPause(file, lower, end)
			if err != nil {
				s.paused = nil
				return nil
			}
			if found {
				s.paused = pause
			}
		}
	}
	s.instanceID, s.info = instanceID, info
	return s.paused
}

func latestSimulationPause(content []byte, completeStart bool) (*bool, bool) {
	if !completeStart {
		if newline := bytes.IndexByte(content, '\n'); newline >= 0 {
			content = content[newline+1:]
		} else {
			return nil, false
		}
	}
	matches := simulationRecord.FindAllSubmatch(content, -1)
	if len(matches) == 0 {
		return nil, false
	}
	switch string(matches[len(matches)-1][1]) {
	case "Sim paused":
		paused := true
		return &paused, true
	case "Sim unpaused":
		paused := false
		return &paused, true
	default:
		// A newer startup record invalidates all earlier pause records.
		return nil, true
	}
}

func scanSimulationPause(file io.ReaderAt, lower, end int64) (*bool, bool, error) {
	buffer := make([]byte, simulationScanBytes)
	for end > lower {
		start := max(lower, end-simulationScanBytes)
		data := buffer[:end-start]
		if _, err := file.ReadAt(data, start); err != nil {
			return nil, false, err
		}
		if pause, found := latestSimulationPause(data, start == 0); found {
			return pause, true, nil
		}
		if start == lower {
			break
		}
		end = start + simulationRecordOverlap
	}
	return nil, false, nil
}
