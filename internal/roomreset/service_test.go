package roomreset

import (
	"context"
	"errors"
	"testing"

	backupapi "dont/internal/backups"
	consoleapi "dont/internal/console"
	"dont/internal/jobs"
	"dont/internal/rooms"
)

type fakeRooms struct{}

func (fakeRooms) Room(id string) (rooms.Room, error) {
	return rooms.Room{ID: id, Name: "测试服"}, nil
}
func (fakeRooms) Worlds(id string) ([]rooms.World, error) {
	return []rooms.World{{ID: "caves", RoomID: id}, {ID: "master", RoomID: id, IsMaster: true}}, nil
}

type fakeBackups struct {
	calls int
	err   error
}

func (b *fakeBackups) Create(_ context.Context, room, name string, kind backupapi.Kind, job string) (backupapi.Backup, error) {
	b.calls++
	if kind != backupapi.KindProtection || job == "" {
		panic("unexpected backup kind or source job")
	}
	return backupapi.Backup{Name: name}, b.err
}

type fakeConsole struct {
	calls  []string
	result consoleapi.WorkbenchResult
	err    error
}

func (c *fakeConsole) ExecuteWorkbench(_ context.Context, room, world, id string, _ map[string]interface{}, confirmed bool) (consoleapi.WorkbenchResult, error) {
	if !confirmed {
		panic("reset must be confirmed")
	}
	c.calls = append(c.calls, room+"/"+world+"/"+id)
	return c.result, c.err
}

type fakeJobs struct {
	targets []jobs.TargetSpec
	results map[string]jobs.TargetResult
}

func (j *fakeJobs) SubmitFactory(kind, room, world string, targets []jobs.TargetSpec, factory func(jobs.Job) jobs.Runner) (jobs.Job, error) {
	j.targets, j.results = targets, map[string]jobs.TargetResult{}
	job := jobs.Job{ID: "job-1", Kind: kind, RoomID: room, WorldID: world}
	err := factory(job)(context.Background(), func(r jobs.TargetResult) { j.results[r.TargetID] = r })
	return job, err
}

func service(b *fakeBackups, c *fakeConsole, j *fakeJobs) *Service {
	return New(fakeRooms{}, b, c, j)
}

func TestBackupThenReset(t *testing.T) {
	b, c, j := &fakeBackups{}, &fakeConsole{result: consoleapi.WorkbenchResult{Status: consoleapi.RunSucceeded, Complete: true}}, &fakeJobs{}
	job, err := service(b, c, j).Submit("room-1", Request{Confirmation: "测试服"})
	if err != nil || job.Kind != JobKind || job.WorldID != "master" {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if b.calls != 1 || len(c.calls) != 1 || c.calls[0] != "room-1/master/regenerate" {
		t.Fatalf("backup=%d console=%v", b.calls, c.calls)
	}
	if len(j.targets) != 2 || j.results[backupTarget].Status != jobs.StatusSucceeded || j.results[resetTarget].Status != jobs.StatusSucceeded {
		t.Fatalf("results=%+v", j.results)
	}
}

func TestSkipBackupWhenExplicitlyDisabled(t *testing.T) {
	off := false
	b, c, j := &fakeBackups{}, &fakeConsole{result: consoleapi.WorkbenchResult{Status: consoleapi.RunSucceeded, Complete: true}}, &fakeJobs{}
	if _, err := service(b, c, j).Submit("room-1", Request{Backup: &off, Confirmation: "测试服"}); err != nil {
		t.Fatal(err)
	}
	if b.calls != 0 || len(j.targets) != 1 || j.results[resetTarget].Status != jobs.StatusSucceeded {
		t.Fatalf("backup=%d targets=%v results=%+v", b.calls, j.targets, j.results)
	}
}

func TestBackupFailureCancelsReset(t *testing.T) {
	b, c, j := &fakeBackups{err: errors.New("disk full")}, &fakeConsole{}, &fakeJobs{}
	if _, err := service(b, c, j).Submit("room-1", Request{Confirmation: "测试服"}); err != nil {
		t.Fatal(err)
	}
	if len(c.calls) != 0 {
		t.Fatal("reset must not run after a failed backup")
	}
	if r := j.results[resetTarget]; r.Status != jobs.StatusFailed || r.Error.Code != "RESET_SKIPPED_BACKUP_FAILED" {
		t.Fatalf("reset result=%+v", r)
	}
}

func TestWrongConfirmationCreatesNoJob(t *testing.T) {
	b, c, j := &fakeBackups{}, &fakeConsole{}, &fakeJobs{}
	if _, err := service(b, c, j).Submit("room-1", Request{Confirmation: "确认"}); !errors.Is(err, ErrConfirmation) {
		t.Fatalf("err=%v", err)
	}
	if j.targets != nil || b.calls != 0 || len(c.calls) != 0 {
		t.Fatal("nothing may run without the room-name confirmation")
	}
}

func TestUnconfirmedResetIsNeverSuccess(t *testing.T) {
	b, c, j := &fakeBackups{}, &fakeConsole{result: consoleapi.WorkbenchResult{RunID: "run-9", Status: consoleapi.RunUncertain}}, &fakeJobs{}
	if _, err := service(b, c, j).Submit("room-1", Request{Confirmation: "测试服"}); err != nil {
		t.Fatal(err)
	}
	if r := j.results[resetTarget]; r.Status != jobs.StatusFailed || r.Error.Code != "RESET_UNCONFIRMED" {
		t.Fatalf("reset result=%+v", r)
	}
}
