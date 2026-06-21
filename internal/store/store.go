package store

import (
	"context"
	"time"

	"github.com/imlach/nightwatch/internal/operation"
)

type PowerState string

const (
	PowerOn  PowerState = "on"
	PowerOff PowerState = "off"
)

type Lease struct {
	Node      string    `json:"node"`
	Actor     string    `json:"actor"`
	LockID    string    `json:"lock_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

type StateStore interface {
	GetDesiredPower(ctx context.Context, node string) (PowerState, bool, error)
	SetDesiredPower(ctx context.Context, node string, state PowerState, actor string) error
	GetObservedState(ctx context.Context, node string) (map[string]any, bool, error)
	UpdateObservedState(ctx context.Context, node string, state map[string]any) error
	AcquireLock(ctx context.Context, node, actor string, ttl time.Duration) (string, bool, error)
	RefreshLock(ctx context.Context, node, lockID string, ttl time.Duration) (bool, error)
	ReleaseLock(ctx context.Context, node, lockID string) error
	GetLock(ctx context.Context, node string) (*Lease, bool, error)
	CreateOperation(ctx context.Context, op operation.Operation) (string, error)
	GetOperation(ctx context.Context, id string) (*operation.Operation, bool, error)
	ListOperations(ctx context.Context, state operation.State) ([]operation.Operation, error)
	UpdateOperation(ctx context.Context, id string, mutate func(*operation.Operation) error) error
	SetLastSuccess(ctx context.Context, node, op string, ts time.Time) error
	ListDesiredPower(ctx context.Context) (map[string]PowerState, error)
}
