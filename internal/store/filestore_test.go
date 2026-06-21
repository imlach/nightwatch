package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/imlach/nightwatch/internal/operation"
)

func TestFileStoreDesiredPower(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.GetDesiredPower(ctx, "node-1"); err != nil || found {
		t.Fatalf("initial GetDesiredPower found=%v err=%v, want found=false err=nil", found, err)
	}
	if err := store.SetDesiredPower(ctx, "node-1", PowerOff, "test"); err != nil {
		t.Fatal(err)
	}
	state, found, err := store.GetDesiredPower(ctx, "node-1")
	if err != nil || !found || state != PowerOff {
		t.Fatalf("GetDesiredPower = %q found=%v err=%v, want off true nil", state, found, err)
	}
}

func TestFileStoreListOperationsSkipsCorruptFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	op := operation.New(operation.PowerOn, "node-1", "test", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))
	if _, err := store.CreateOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "operations", "corrupt.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	ops, err := store.ListOperations(ctx, operation.Pending)
	if err != nil {
		t.Fatalf("ListOperations() error = %v, want nil", err)
	}
	if len(ops) != 1 || ops[0].ID != op.ID {
		t.Fatalf("ListOperations() = %+v, want only valid operation %s", ops, op.ID)
	}
}

func TestFileStoreLockLifecycle(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lockID, ok, err := store.AcquireLock(ctx, "node-1", "test", time.Minute)
	if err != nil || !ok || lockID == "" {
		t.Fatalf("AcquireLock = %q %v %v, want id true nil", lockID, ok, err)
	}
	if _, ok, err := store.AcquireLock(ctx, "node-1", "other", time.Minute); err != nil || ok {
		t.Fatalf("second AcquireLock ok=%v err=%v, want false nil", ok, err)
	}
	lease, found, err := store.GetLock(ctx, "node-1")
	if err != nil || !found || lease.LockID != lockID {
		t.Fatalf("GetLock = %+v found=%v err=%v", lease, found, err)
	}
	if err := store.ReleaseLock(ctx, "node-1", lockID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.GetLock(ctx, "node-1"); err != nil || found {
		t.Fatalf("GetLock after release found=%v err=%v, want false nil", found, err)
	}
}

func TestFileStoreOperations(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	op := operation.New(operation.PowerOn, "node-1", "test", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))
	id, err := store.CreateOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateOperation(ctx, id, func(op *operation.Operation) error {
		op.State = operation.Running
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ops, err := store.ListOperations(ctx, operation.Running)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].ID != id {
		t.Fatalf("ListOperations = %+v, want one op %s", ops, id)
	}
}
