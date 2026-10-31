package softwareupdate

import (
	"context"
	"errors"
	"testing"
)

func TestCancelDownloadPersistsAndAllowsExplicitNewDownload(t *testing.T) {
	s, client := testService(t, testBundle(t, "v1.1.0", "linux-amd64", nil))
	client.block = make(chan struct{})
	op, err := s.Start("v1.1.0", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CancelDownload(context.Background(), "wrong-id"); !errors.Is(err, ErrNotPrepared) {
		t.Fatal(err)
	}
	canceled, err := s.CancelDownload(context.Background(), op.ID)
	if err != nil || canceled.Phase != "canceled" {
		t.Fatal(canceled, err)
	}
	snapshot := finishDownload(t, s)
	if snapshot.Operation.Phase != "canceled" {
		t.Fatal(snapshot.Operation)
	}
	state, err := readState(s.config.Root)
	if err != nil || state.Pending != nil || state.Current.Version != "v1.0.0" {
		t.Fatal(state, err)
	}
	if err = s.phase(op.ID, "prepared", 100, nil, "release"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation resurrected", err)
	}
	if _, _, err = s.PrepareApply(context.Background(), op.ID); err == nil {
		t.Fatal("canceled download can apply")
	}
	client.block = nil
	next, err := s.Start("v1.1.0", "auto")
	if err != nil || next.ID == op.ID {
		t.Fatal(next, err)
	}
	if _, err = s.CancelDownload(context.Background(), op.ID); !errors.Is(err, ErrNotPrepared) {
		t.Fatal("old id canceled new operation", err)
	}
	snapshot = finishDownload(t, s)
	if snapshot.Operation.Phase != "prepared" {
		t.Fatal(snapshot.Operation)
	}
}
func TestCancelPreparedAndRejectCommittedSwitch(t *testing.T) {
	s, _ := testService(t, testBundle(t, "v1.1.0", "linux-amd64", nil))
	op, err := s.Start("v1.1.0", "auto")
	if err != nil {
		t.Fatal(err)
	}
	finishDownload(t, s)
	state, err := readState(s.config.Root)
	if err != nil {
		t.Fatal(err)
	}
	state.Pending = &installedRelease{Version: "v1.1.0"}
	if err = writeState(s.config.Root, state); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CancelDownload(context.Background(), op.ID); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	state.Pending = nil
	if err = writeState(s.config.Root, state); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CancelDownload(context.Background(), op.ID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Snapshot()
	if err != nil || snapshot.Operation.Phase != "canceled" {
		t.Fatal(snapshot, err)
	}
}
