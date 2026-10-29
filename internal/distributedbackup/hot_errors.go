package distributedbackup

import (
	"errors"
	"strings"
)

// The save and cleanup paths can encounter the same unreadable receipt. Keep
// every cause for errors.Is/As, but do not repeat identical lines in job output.
func joinHotBackupErrors(causes ...error) error {
	joined := errors.Join(causes...)
	if joined == nil {
		return nil
	}
	seen := make(map[string]bool)
	var lines []string
	for _, line := range strings.Split(joined.Error(), "\n") {
		if !seen[line] {
			seen[line] = true
			lines = append(lines, line)
		}
	}
	return &hotBackupError{cause: joined, message: strings.Join(lines, "\n")}
}

type hotBackupError struct {
	cause   error
	message string
}

func (e *hotBackupError) Error() string { return e.message }
func (e *hotBackupError) Unwrap() error { return e.cause }
