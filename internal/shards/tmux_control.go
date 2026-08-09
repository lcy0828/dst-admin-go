package shards

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	dsttmux "dont/tmux"
)

type TmuxConfig struct {
	SaveRoot     string
	UGCDirectory string
	ServerPath   string
	ServerMode   string
}

type TmuxControl struct {
	config TmuxConfig
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
	return &TmuxControl{config: config}, nil
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
	server, err := c.server(roomName, worldName)
	if err != nil {
		return err
	}
	return server.Start()
}

func (c *TmuxControl) Stop(ctx context.Context, roomName, worldName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	server, err := c.server(roomName, worldName)
	if err != nil {
		return err
	}
	return server.Stop()
}

func (c *TmuxControl) Send(ctx context.Context, roomName, worldName, command string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	server, err := c.server(roomName, worldName)
	if err != nil {
		return err
	}
	return server.SendCommand(command)
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
