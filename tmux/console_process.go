package tmux

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/process"
)

// tmux may report the shell/flock wrapper as its foreground command. Follow only
// that pane's bounded launcher chain, and require the actual DST executable and
// matching room/shard arguments before accepting console input.
func (s *DSTServer) consoleGameDescendant(pid int32) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type candidate struct {
		pid   int32
		depth int
	}
	queue := []candidate{{pid: pid}}
	seen := make(map[int32]bool)
	for len(queue) > 0 && len(seen) < 32 && ctx.Err() == nil {
		next := queue[0]
		queue = queue[1:]
		if seen[next.pid] || next.depth > 8 {
			continue
		}
		seen[next.pid] = true
		p, err := process.NewProcessWithContext(ctx, next.pid)
		if err != nil {
			continue
		}
		executable, err := p.ExeWithContext(ctx)
		if err != nil {
			continue
		}
		name := strings.ToLower(filepath.Base(executable))
		if strings.HasPrefix(name, "dontstarve_dedicated_server") {
			args, err := p.CmdlineSliceWithContext(ctx)
			if err == nil && consoleWorldArgumentsMatch(args, s.ArchiveName, s.WorldName) {
				return true
			}
			continue
		}
		switch name {
		case "sh", "dash", "bash", "zsh", "flock", "env", "taskpolicy":
		default:
			continue
		}
		children, err := p.ChildrenWithContext(ctx)
		if err != nil || len(children)+len(queue)+len(seen) > 32 {
			continue
		}
		for _, child := range children {
			queue = append(queue, candidate{pid: child.Pid, depth: next.depth + 1})
		}
	}
	return false
}

func consoleWorldArgumentsMatch(args []string, cluster, shard string) bool {
	var gotCluster, gotShard string
	for index := 1; index+1 < len(args); index++ {
		switch args[index] {
		case "-cluster":
			gotCluster = args[index+1]
		case "-shard":
			gotShard = args[index+1]
		}
	}
	return cluster != "" && shard != "" && gotCluster == cluster && gotShard == shard
}
