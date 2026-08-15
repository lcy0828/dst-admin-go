package moddistribution

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	managedSetupBegin = "-- BEGIN DST-ADMIN MANAGED MODS"
	managedSetupEnd   = "-- END DST-ADMIN MANAGED MODS"
)

func composeManagedSetup(current []byte, mods []ModVersion) ([]byte, error) {
	if len(current) > maxOverridesBytes || bytes.IndexByte(current, 0) >= 0 {
		return nil, ErrIntegrity
	}
	text := string(current)
	newline := "\n"
	if bytes.Contains(current, []byte("\r\n")) {
		newline = "\r\n"
	}
	begin := strings.Index(text, managedSetupBegin)
	end := strings.Index(text, managedSetupEnd)
	if begin >= 0 != (end >= 0) || begin >= 0 && end < begin || strings.Count(text, managedSetupBegin) > 1 || strings.Count(text, managedSetupEnd) > 1 {
		return nil, ErrConflict
	}
	prefix, suffix := text, ""
	if begin >= 0 {
		prefix = text[:begin]
		suffix = text[end+len(managedSetupEnd):]
	}
	prefix = strings.TrimRight(prefix, "\r\n")
	suffix = strings.TrimLeft(suffix, "\r\n")
	var builder strings.Builder
	if prefix != "" {
		builder.WriteString(prefix)
		builder.WriteString(newline)
		builder.WriteString(newline)
	}
	builder.WriteString(managedSetupBegin)
	builder.WriteString(newline)
	for _, mod := range mods {
		if !validWorkshopID(mod.WorkshopID) {
			return nil, ErrInvalidInput
		}
		fmt.Fprintf(&builder, "ServerModSetup(\"%s\")%s", mod.WorkshopID, newline)
	}
	builder.WriteString(managedSetupEnd)
	builder.WriteString(newline)
	if suffix != "" {
		builder.WriteString(newline)
		builder.WriteString(suffix)
		if !strings.HasSuffix(suffix, "\n") && !strings.HasSuffix(suffix, "\r") {
			builder.WriteString(newline)
		}
	}
	return []byte(builder.String()), nil
}

func readOptionalRegular(path string) ([]byte, error) {
	if err := rejectSymlinkComponents(filepath.Dir(path)); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxOverridesBytes {
		return nil, ErrUnsafePath
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(content) > maxOverridesBytes {
		return nil, errors.Join(ErrIntegrity, ErrInvalidInput)
	}
	return content, nil
}
