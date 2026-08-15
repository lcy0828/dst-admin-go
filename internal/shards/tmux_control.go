package shards

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"dont/internal/consoledispatch"
	dsttmux "dont/tmux"
)

type TmuxConfig struct {
	SaveRoot      string
	UGCDirectory  string
	ServerPath    string
	ServerMode    string
	ConsoleSocket string
}

type TmuxControl struct {
	config     TmuxConfig
	dispatcher *consoledispatch.Dispatcher
}

func NewTmuxControl(config TmuxConfig) (*TmuxControl, error) {
	config.SaveRoot = filepath.Clean(strings.TrimSpace(config.SaveRoot))
	config.ServerMode = strings.TrimSpace(config.ServerMode)
	config.ConsoleSocket = strings.TrimSpace(config.ConsoleSocket)
	if config.SaveRoot == "" || config.SaveRoot == "." {
		return nil, fmt.Errorf("DST save root is required")
	}
	if config.ServerMode != "32" && config.ServerMode != "64" {
		config.ServerMode = "64"
	}
	if config.ConsoleSocket != "" && (!filepath.IsAbs(config.ConsoleSocket) || strings.ContainsAny(config.ConsoleSocket, "\x00\r\n")) {
		return nil, fmt.Errorf("tmux console socket must be an absolute safe path")
	}
	return &TmuxControl{config: config, dispatcher: consoledispatch.New()}, nil
}

func (c *TmuxControl) IsRunning(ctx context.Context, roomName, worldName string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	server, err := c.server(roomName, worldName)
	if err != nil {
		return false, err
	}
	return server.IsRunning()
}

func (c *TmuxControl) Status(ctx context.Context, roomName, worldName string) (RuntimeStatus, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeStatus{State: RuntimeUnknown}, err
	}
	server, err := c.server(roomName, worldName)
	if err != nil {
		return RuntimeStatus{State: RuntimeUnknown}, err
	}
	status, err := server.RuntimeStatus()
	if err != nil {
		return RuntimeStatus{State: RuntimeUnknown}, err
	}
	if status.State == dsttmux.RuntimeStopped && c.config.ConsoleSocket != "" {
		if legacy, legacyErr := server.DefaultSocketSessionExists(); legacyErr != nil {
			return RuntimeStatus{State: RuntimeUnknown}, legacyErr
		} else if legacy {
			return RuntimeStatus{
				State: RuntimeFailed, Code: "LEGACY_TMUX_SOCKET_CONFLICT", SessionExists: true,
				Message: "同名 DST 会话仍在默认 tmux socket 运行；为防止双实例，请先用旧版工具停止该分片再重新启动",
			}, nil
		}
	}
	return RuntimeStatus{
		State: RuntimeState(status.State), Code: status.Code, Message: status.Message, SessionExists: status.SessionExists,
	}, nil
}

func (c *TmuxControl) Start(ctx context.Context, roomName, worldName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := c.shardKey(roomName, worldName)
	server, err := c.server(roomName, worldName)
	if err != nil {
		return err
	}
	if c.config.ConsoleSocket != "" {
		if legacy, legacyErr := server.DefaultSocketSessionExists(); legacyErr != nil {
			return legacyErr
		} else if legacy {
			return errors.New("同名 DST 会话仍在默认 tmux socket 运行，已阻止启动第二个实例")
		}
	}
	if err := server.Start(); err != nil {
		_ = c.dispatcher.Pause(context.Background(), key)
		return err
	}
	instanceID, err := server.RuntimeInstanceID()
	if err != nil {
		_ = c.dispatcher.Pause(context.Background(), key)
		return fmt.Errorf("读取新启动的 tmux 实例身份: %w", err)
	}
	if err := c.dispatcher.BindInstance(key, instanceID); err != nil {
		return err
	}
	return c.dispatcher.Resume(key)
}

func (c *TmuxControl) Stop(ctx context.Context, roomName, worldName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := c.shardKey(roomName, worldName)
	if err := c.dispatcher.Pause(ctx, key); err != nil {
		return err
	}
	server, err := c.server(roomName, worldName)
	if err != nil {
		_ = c.dispatcher.Resume(key)
		return err
	}
	if err := server.Stop(); err != nil {
		_ = c.dispatcher.Resume(key)
		return err
	}
	return nil
}

func (c *TmuxControl) Cleanup(ctx context.Context, roomName, worldName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := c.shardKey(roomName, worldName)
	if err := c.dispatcher.Pause(ctx, key); err != nil {
		return err
	}
	server, err := c.server(roomName, worldName)
	if err != nil {
		_ = c.dispatcher.Resume(key)
		return err
	}
	if err := server.KillSession(); err != nil {
		_ = c.dispatcher.Resume(key)
		return err
	}
	return nil
}

func (c *TmuxControl) Send(ctx context.Context, roomName, worldName, command string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	server, err := c.server(roomName, worldName)
	if err != nil {
		return err
	}
	key := c.shardKey(roomName, worldName)
	if err := c.guardConsoleTransport(server, key); err != nil {
		return err
	}
	instanceID, err := server.RuntimeInstanceID()
	if err != nil {
		return err
	}
	if err := c.dispatcher.BindInstance(key, instanceID); err != nil {
		return err
	}
	writeAttempted := false
	err = c.dispatcher.Dispatch(ctx, key, consoledispatch.Request{InstanceID: instanceID, Execute: func(sendContext context.Context) error {
		if err := sendContext.Err(); err != nil {
			return err
		}
		current, err := c.server(roomName, worldName)
		if err != nil {
			return err
		}
		currentID, err := current.RuntimeInstanceID()
		if err != nil {
			return err
		}
		if currentID != instanceID {
			return consoledispatch.ErrInstanceChanged
		}
		writeAttempted = true
		return current.SendCommand(command)
	}})
	if err != nil && writeAttempted && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		c.dispatcher.MarkInputDirty(key)
	}
	return err
}

func (c *TmuxControl) SendBackground(ctx context.Context, roomName, worldName, coalesceKey, command string) error {
	server, err := c.server(roomName, worldName)
	if err != nil {
		return err
	}
	key := c.shardKey(roomName, worldName)
	if err := c.guardConsoleTransport(server, key); err != nil {
		return err
	}
	instanceID, err := server.RuntimeInstanceID()
	if err != nil {
		return err
	}
	if err := c.dispatcher.BindInstance(key, instanceID); err != nil {
		return err
	}
	writeAttempted := false
	err = c.dispatcher.Dispatch(ctx, key, consoledispatch.Request{
		Class: consoledispatch.ClassBackground, CoalesceKey: coalesceKey, InstanceID: instanceID,
		Execute: func(sendContext context.Context) error {
			if err := sendContext.Err(); err != nil {
				return err
			}
			current, err := c.server(roomName, worldName)
			if err != nil {
				return err
			}
			currentID, err := current.RuntimeInstanceID()
			if err != nil {
				return err
			}
			if currentID != instanceID {
				return consoledispatch.ErrInstanceChanged
			}
			writeAttempted = true
			return current.SendCommand(command)
		},
	})
	if err != nil && writeAttempted && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		c.dispatcher.MarkInputDirty(key)
	}
	return err
}

func (c *TmuxControl) ConsoleHealth(roomName, worldName string) consoledispatch.Health {
	key := c.shardKey(roomName, worldName)
	current := c.dispatcher.Health(key)
	server, err := c.server(roomName, worldName)
	if err != nil {
		current.Status, current.Accepting = "not_found", false
		return current
	}
	status, externalWriter, probeErr := server.ConsoleTransportHealth()
	if probeErr != nil {
		current.Status, current.Accepting = "socket_unavailable", false
		return current
	}
	if externalWriter && !current.Maintenance {
		c.dispatcher.MarkExternalWriter(key)
		return c.dispatcher.Health(key)
	}
	if status != "ready" {
		current.Status, current.Accepting = status, false
	}
	return current
}

func (c *TmuxControl) guardConsoleTransport(server *dsttmux.DSTServer, key string) error {
	status, externalWriter, err := server.ConsoleTransportHealth()
	if err != nil {
		return err
	}
	if externalWriter {
		c.dispatcher.MarkExternalWriter(key)
		return consoledispatch.ErrExternalWriter
	}
	if status != "ready" {
		return fmt.Errorf("console transport is %s", status)
	}
	return nil
}

type ConsoleAttachSpec struct {
	Command    []string
	InstanceID string
}

func (c *TmuxControl) ConsoleAttach(roomName, worldName string, readOnly bool) (ConsoleAttachSpec, error) {
	server, err := c.server(roomName, worldName)
	if err != nil {
		return ConsoleAttachSpec{}, err
	}
	command, err := server.ConsoleAttachCommand(readOnly)
	if err != nil {
		return ConsoleAttachSpec{}, err
	}
	instanceID, err := server.RuntimeInstanceID()
	if err != nil {
		return ConsoleAttachSpec{}, err
	}
	return ConsoleAttachSpec{Command: command, InstanceID: instanceID}, nil
}

func (c *TmuxControl) BeginConsoleMaintenance(ctx context.Context, roomName, worldName, owner string) (ConsoleAttachSpec, *consoledispatch.MaintenanceLease, error) {
	spec, err := c.ConsoleAttach(roomName, worldName, false)
	if err != nil {
		return ConsoleAttachSpec{}, nil, err
	}
	key := c.shardKey(roomName, worldName)
	if err := c.dispatcher.BindInstance(key, spec.InstanceID); err != nil {
		return ConsoleAttachSpec{}, nil, err
	}
	lease, err := c.dispatcher.BeginMaintenance(ctx, key, owner, spec.InstanceID)
	if err != nil {
		return ConsoleAttachSpec{}, nil, err
	}
	return spec, lease, nil
}

func (c *TmuxControl) EndConsoleMaintenance(roomName, worldName string, lease *consoledispatch.MaintenanceLease) error {
	if lease == nil {
		return consoledispatch.ErrInvalidRequest
	}
	key := c.shardKey(roomName, worldName)
	server, err := c.server(roomName, worldName)
	if err != nil {
		c.dispatcher.MarkInputDirty(key)
		_ = lease.Release()
		return err
	}
	status, externalWriter, probeErr := server.ConsoleTransportHealth()
	if probeErr != nil || status != "ready" {
		c.dispatcher.MarkInputDirty(key)
	} else if externalWriter {
		c.dispatcher.MarkExternalWriter(key)
	}
	return errors.Join(probeErr, lease.Release())
}

func (c *TmuxControl) RecoverConsoleHazard(ctx context.Context, roomName, worldName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	server, err := c.server(roomName, worldName)
	if err != nil {
		return err
	}
	exists, err := server.SessionExists()
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	instanceID, err := server.RuntimeInstanceID()
	if err != nil {
		return err
	}
	key := c.shardKey(roomName, worldName)
	if err := c.dispatcher.BindInstance(key, instanceID); err != nil {
		return err
	}
	c.dispatcher.MarkInputDirty(key)
	return nil
}

func (c *TmuxControl) shardKey(roomName, worldName string) string {
	return c.config.SaveRoot + "\x00" + roomName + "\x00" + worldName
}

func (c *TmuxControl) server(roomName, worldName string) (*dsttmux.DSTServer, error) {
	return dsttmux.NewDSTServerWithSocketAndSessionName(
		roomName,
		worldName,
		dsttmux.V2SessionName(roomName, worldName),
		c.config.ConsoleSocket,
		c.config.UGCDirectory,
		filepath.Dir(c.config.SaveRoot),
		filepath.Base(c.config.SaveRoot),
		c.config.ServerPath,
		c.config.ServerMode,
	)
}
