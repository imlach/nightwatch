package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/imlach/nightwatch/internal/operation"
)

type FileStore struct {
	root string
}

func NewFileStore(root string) (*FileStore, error) {
	for _, dir := range []string{"desired", "observed", "locks", "operations", "last-success"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return nil, err
		}
	}
	return &FileStore{root: root}, nil
}

func (s *FileStore) GetDesiredPower(ctx context.Context, node string) (PowerState, bool, error) {
	var doc struct {
		State PowerState `json:"state"`
	}
	found, err := s.readJSON("desired", node, &doc)
	return doc.State, found, err
}

func (s *FileStore) SetDesiredPower(ctx context.Context, node string, state PowerState, actor string) error {
	return s.writeJSON("desired", node, map[string]any{
		"state": state,
		"actor": actor,
		"ts":    time.Now().UTC(),
	})
}

func (s *FileStore) GetObservedState(ctx context.Context, node string) (map[string]any, bool, error) {
	var doc map[string]any
	found, err := s.readJSON("observed", node, &doc)
	return doc, found, err
}

func (s *FileStore) UpdateObservedState(ctx context.Context, node string, state map[string]any) error {
	state["updated_at"] = time.Now().UTC()
	return s.writeJSON("observed", node, state)
}

func (s *FileStore) AcquireLock(ctx context.Context, node, actor string, ttl time.Duration) (string, bool, error) {
	lock, found, err := s.GetLock(ctx, node)
	if err != nil {
		return "", false, err
	}
	if found && lock.ExpiresAt.After(time.Now()) {
		return "", false, nil
	}
	lockID, err := randomID()
	if err != nil {
		return "", false, err
	}
	lease := Lease{Node: node, Actor: actor, LockID: lockID, ExpiresAt: time.Now().Add(ttl).UTC()}
	if err := s.writeJSON("locks", node, lease); err != nil {
		return "", false, err
	}
	return lockID, true, nil
}

func (s *FileStore) RefreshLock(ctx context.Context, node, lockID string, ttl time.Duration) (bool, error) {
	lease, found, err := s.GetLock(ctx, node)
	if err != nil || !found {
		return false, err
	}
	if lease.LockID != lockID || lease.ExpiresAt.Before(time.Now()) {
		return false, nil
	}
	lease.ExpiresAt = time.Now().Add(ttl).UTC()
	return true, s.writeJSON("locks", node, lease)
}

func (s *FileStore) ReleaseLock(ctx context.Context, node, lockID string) error {
	lease, found, err := s.GetLock(ctx, node)
	if err != nil || !found {
		return err
	}
	if lease.LockID != lockID {
		return fmt.Errorf("cannot release lock %s on %s: held by %s", lockID, node, lease.LockID)
	}
	return os.Remove(s.path("locks", node))
}

func (s *FileStore) GetLock(ctx context.Context, node string) (*Lease, bool, error) {
	var lease Lease
	found, err := s.readJSON("locks", node, &lease)
	if err != nil || !found {
		return nil, found, err
	}
	if lease.ExpiresAt.Before(time.Now()) {
		_ = os.Remove(s.path("locks", node))
		return nil, false, nil
	}
	return &lease, true, nil
}

func (s *FileStore) CreateOperation(ctx context.Context, op operation.Operation) (string, error) {
	if op.ID == "" {
		return "", errors.New("operation ID is required")
	}
	return op.ID, s.writeJSON("operations", op.ID, op)
}

func (s *FileStore) GetOperation(ctx context.Context, id string) (*operation.Operation, bool, error) {
	var op operation.Operation
	found, err := s.readJSON("operations", id, &op)
	if err != nil || !found {
		return nil, found, err
	}
	return &op, true, nil
}

func (s *FileStore) ListOperations(ctx context.Context, state operation.State) ([]operation.Operation, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "operations"))
	if err != nil {
		return nil, err
	}
	var ops []operation.Operation
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var op operation.Operation
		key := strings.TrimSuffix(entry.Name(), ".json")
		found, err := s.readJSON("operations", key, &op)
		if err != nil {
			slog.Warn("skip unreadable operation state", "file", entry.Name(), "err", err)
			continue
		}
		if !found {
			continue
		}
		if state == "" || op.State == state {
			ops = append(ops, op)
		}
	}
	return ops, nil
}

func (s *FileStore) UpdateOperation(ctx context.Context, id string, mutate func(*operation.Operation) error) error {
	op, found, err := s.GetOperation(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return os.ErrNotExist
	}
	if err := mutate(op); err != nil {
		return err
	}
	op.UpdatedAt = time.Now().UTC()
	return s.writeJSON("operations", id, op)
}

func (s *FileStore) SetLastSuccess(ctx context.Context, node, op string, ts time.Time) error {
	return s.writeJSON("last-success", node+"--"+op, map[string]any{"ts": ts.UTC()})
}

func (s *FileStore) ListDesiredPower(ctx context.Context) (map[string]PowerState, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "desired"))
	if err != nil {
		return nil, err
	}
	out := map[string]PowerState{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		node := strings.TrimSuffix(entry.Name(), ".json")
		state, found, err := s.GetDesiredPower(ctx, node)
		if err != nil {
			return nil, err
		}
		if found {
			out[node] = state
		}
	}
	return out, nil
}

func (s *FileStore) readJSON(kind, key string, out any) (bool, error) {
	b, err := os.ReadFile(s.path(kind, key))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, out)
}

func (s *FileStore) writeJSON(kind, key string, value any) error {
	path := s.path(kind, key)
	lockPath := path + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *FileStore) path(kind, key string) string {
	return filepath.Join(s.root, kind, safeKey(key)+".json")
}

func safeKey(key string) string {
	key = strings.ReplaceAll(key, "/", "--")
	return strings.ReplaceAll(key, "..", "__")
}

func randomID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
