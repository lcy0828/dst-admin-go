// Package worldquery exposes only compiled-in, bounded game observations.
package worldquery

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/rooms"
)

var (
	ErrInvalid  = errors.New("WORLD_QUERY_INVALID")
	ErrDenied   = errors.New("WORLD_QUERY_DENIED")
	ErrLimited  = errors.New("WORLD_QUERY_RATE_LIMITED")
	ErrBusy     = errors.New("WORLD_QUERY_BUSY")
	ErrNotFound = errors.New("WORLD_QUERY_NOT_FOUND")
)

type Principal struct{ ID, RoomID string }
type Request struct{ RoomID, WorldID, QueryID, Prefab string }
type RoomReader interface {
	Room(string) (rooms.Room, error)
	World(string, string) (rooms.World, error)
}
type Runtime interface {
	QueryIdentity(context.Context, string, string) (string, error)
	ExecuteWorldQuery(context.Context, string, string, dstruntime.WorldQueryRequest) (dstruntime.CommandReceipt, error)
	ReadCommandReceipt(context.Context, string, string, dstruntime.CommandRequest, time.Time) (dstruntime.CommandReceipt, error)
	ReadCurrentWorldState(context.Context, string, string) (dstruntime.CurrentWorldState, error)
	ReadPlayers(context.Context, string, string) (dstruntime.Snapshot, error)
}

type Result struct {
	QueryRunID       string          `json:"queryRunId,omitempty"`
	QueryID          string          `json:"queryId"`
	RoomID           string          `json:"roomId"`
	WorldID          string          `json:"worldId"`
	Status           string          `json:"status"`
	Source           string          `json:"source"`
	Cached           bool            `json:"cached"`
	Complete         bool            `json:"complete"`
	TransportOutcome string          `json:"transportOutcome"`
	ExecutionOutcome string          `json:"executionOutcome"`
	ResultState      string          `json:"resultState"`
	MayHaveExecuted  bool            `json:"mayHaveExecuted"`
	ObservedAt       *time.Time      `json:"observedAt,omitempty"`
	Data             json.RawMessage `json:"data"`
	Error            *QueryError     `json:"error"`
	ScannedEntities  *int            `json:"scannedEntities,omitempty"`
	ScanLimit        int             `json:"scanLimit,omitempty"`
	ReceiptCheck     string          `json:"receiptCheck,omitempty"`
	RuntimeCode      string          `json:"runtimeCode,omitempty"`
}
type QueryError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type entityData struct {
	Prefab     string `json:"prefab"`
	Count      *int   `json:"count"`
	LowerBound *int   `json:"lowerBound,omitempty"`
}
type player struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Prefab string `json:"prefab"`
}
type playerData struct {
	Players []player `json:"players"`
	Count   int      `json:"count"`
}
type worldData struct {
	Cycles      *int     `json:"cycles"`
	Season      *string  `json:"season"`
	Phase       *string  `json:"phase"`
	Temperature *float64 `json:"temperature"`
	IsRaining   *bool    `json:"israining"`
	IsSnowing   *bool    `json:"issnowing"`
}
