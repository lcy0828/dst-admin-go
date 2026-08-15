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
	SaveRoot     string
	UGCDirectory string
	ServerPath   string
	ServerMode   string
}

type TmuxControl struct {
	config     TmuxConfig
	dispatcher *consoledispatch.Dispatcher
}

func NewTmuxControl(config TmuxConfig) (*TmuxControl, error) {
	config.SaveRoot = filepath.Clean(strings.TrimSpace(config.SaveRoot))
	config.ServerMode = strings.TrimSpace(config.ServerMode)
	if config.SaveRoot == "" || config.SaveRoot == "." {
		return nil, fmt.Errorf("DST save root is required")
	}
	if config.ServerMode != "32" && config.ServerMode != "64" {
		config.ServerMode = "64"
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
	instanceID, err := server.RuntimeInstanceID()
	if err != nil {
		return err
	}
	key := c.shardKey(roomName, worldName)
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
	instanceID, err := server.RuntimeInstanceID()
	if err != nil {
		return err
	}
	key := c.shardKey(roomName, worldName)
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
	return c.dispatcher.Health(c.shardKey(roomName, worldName))
}

func (c *TmuxControl) shardKey(roomName, worldName string) string {
	return c.config.SaveRoot + "\x00" + roomName + "\x00" + worldName
}

func (c *TmuxControl) server(roomName, worldName string) (*dsttmux.DSTServer, error) {
	return dsttmux.NewDSTServerWithSessionName(
		roomName,
		worldName,
		dsttmux.V2SessionName(roomName, worldName),
		c.config.UGCDirectory,
		filepath.Dir(c.config.SaveRoot),
		filepath.Base(c.config.SaveRoot),
		c.config.ServerPath,
		c.config.ServerMode,
	)
}
