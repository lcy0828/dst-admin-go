package gameupdate

import (
	"context"
	"os/exec"
	"strconv"
	"time"
)

func configureVersionCommand(command *exec.Cmd) {
	command.WaitDelay = time.Second
	command.Cancel = func() error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "taskkill", "/PID", strconv.Itoa(command.Process.Pid), "/T", "/F").Run()
		return command.Process.Kill()
	}
}
