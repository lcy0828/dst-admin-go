package savehealth

import "bytes"

const (
	SaveWriteFailedCode       = "SAVE_WRITE_FAILED"
	SnapshotBarrierFailedCode = "SNAPSHOT_BARRIER_FAILED"
)

type LogIncident struct {
	Code    string
	Message string
}

// InspectLogTail recognizes an unresolved DST save or snapshot barrier failure.
// Callers must pass a bounded tail from the current Runtime log rather than the
// complete file.
func InspectLogTail(content []byte) *LogIncident {
	failure := bytes.LastIndex(content, []byte("[CRITICAL] Failed to save file"))
	serialization := bytes.LastIndex(content, []byte("Serializing world:"))
	if failure >= 0 && serialization <= failure {
		return &LogIncident{
			Code:    SaveWriteFailedCode,
			Message: "DST 正在运行，但最近一次存档写入失败；玩家掉线、重连或进程异常后可能回档。请立即检查存档目录的所有者、权限和磁盘空间。",
		}
	}
	if barrierFailure := lastSnapshotBarrierErrorIndex(content); barrierFailure >= 0 {
		if !barrierFailureResolvedAfter(content[barrierFailure:]) {
			return &LogIncident{
				Code:    SnapshotBarrierFailedCode,
				Message: "DST 进程仍在运行，但最近一次快照屏障释放失败，世界可能停留在暂停或控制台无响应状态；建议一键重启恢复。",
			}
		}
	}
	return nil
}

func lastSnapshotBarrierErrorIndex(content []byte) int {
	const marker = "[DST-ADMIN-RUNTIME ERROR]"
	search := content
	offset := 0
	last := -1
	for len(search) > 0 {
		lineEnd := bytes.IndexByte(search, '\n')
		var line []byte
		nextOffset := offset + len(search)
		if lineEnd >= 0 {
			line = search[:lineEnd]
			nextOffset = offset + lineEnd + 1
		} else {
			line = search
		}
		if !bytes.Contains(line, []byte("RemoteCommandInput:")) &&
			bytes.Contains(line, []byte(marker)) &&
			bytes.Contains(bytes.ToUpper(line), []byte("CODE=SNAPSHOT_BARRIER_")) {
			last = offset
		}
		if lineEnd < 0 {
			break
		}
		search = search[lineEnd+1:]
		offset = nextOffset
	}
	return last
}

func barrierFailureResolvedAfter(tail []byte) bool {
	lines := bytes.Split(tail, []byte{'\n'})
	if len(lines) == 0 {
		return false
	}
	attempt := barrierAttempt(lines[0])
	for _, line := range lines[1:] {
		if bytes.Contains(line, []byte("RemoteCommandInput:")) {
			continue
		}
		message := bytes.TrimSpace(line)
		// Native console lines have a [HH:MM:SS]: prefix. Match actual output,
		// never a print statement embedded in echoed Lua or a transport message.
		if bytes.HasPrefix(message, []byte{'['}) {
			if end := bytes.Index(message, []byte("]:")); end >= 0 {
				message = bytes.TrimSpace(message[end+2:])
			}
		}
		if bytes.Equal(message, []byte("Sim unpaused")) || bytes.HasPrefix(message, []byte("Starting Up")) {
			return true
		}
		if bytes.HasPrefix(message, []byte("[DST-ADMIN-RUNTIME] snapshot barrier recovered")) &&
			(len(attempt) == 0 || bytes.Equal(attempt, barrierAttempt(message))) {
			return true
		}
	}
	return false
}

func barrierAttempt(line []byte) []byte {
	for _, field := range bytes.Fields(line) {
		for _, prefix := range [][]byte{[]byte("id="), []byte("barrier=")} {
			if bytes.HasPrefix(field, prefix) {
				return bytes.TrimPrefix(field, prefix)
			}
		}
	}
	return nil
}
