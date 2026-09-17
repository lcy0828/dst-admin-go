package savehealth

import "bytes"

const SaveWriteFailedCode = "SAVE_WRITE_FAILED"

type LogIncident struct {
	Code    string
	Message string
}

// InspectLogTail recognizes an unresolved DST save failure. Callers must pass
// a bounded tail from the current Runtime log rather than the complete file.
func InspectLogTail(content []byte) *LogIncident {
	failure := bytes.LastIndex(content, []byte("[CRITICAL] Failed to save file"))
	if failure < 0 {
		return nil
	}
	serialization := bytes.LastIndex(content, []byte("Serializing world:"))
	if serialization > failure {
		return nil
	}
	return &LogIncident{
		Code:    SaveWriteFailedCode,
		Message: "DST 正在运行，但最近一次存档写入失败；玩家掉线、重连或进程异常后可能回档。请立即检查存档目录的所有者、权限和磁盘空间。",
	}
}
