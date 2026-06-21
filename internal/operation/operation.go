package operation

import "time"

type Type string

const (
	PowerOn       Type = "power-on"
	DrainShutdown Type = "drain-shutdown"
	Cordon        Type = "cordon"
	Uncordon      Type = "uncordon"
	Reserve       Type = "reserve"
	Release       Type = "release"
)

type State string

const (
	Pending   State = "pending"
	Running   State = "running"
	Succeeded State = "succeeded"
	Failed    State = "failed"
	Cancelled State = "cancelled"
)

type Step struct {
	Name      string         `json:"name"`
	Succeeded bool           `json:"succeeded"`
	Message   string         `json:"message,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
	At        time.Time      `json:"at"`
}

type Operation struct {
	ID        string    `json:"id"`
	Type      Type      `json:"type"`
	Node      string    `json:"node"`
	State     State     `json:"state"`
	Actor     string    `json:"actor"`
	LockID    string    `json:"lock_id,omitempty"`
	Steps     []Step    `json:"steps,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func New(typ Type, node, actor string, now time.Time) Operation {
	return Operation{
		ID:        "op-" + now.UTC().Format("20060102-150405") + "-" + node + "-" + string(typ),
		Type:      typ,
		Node:      node,
		State:     Pending,
		Actor:     actor,
		CreatedAt: now.UTC(),
		UpdatedAt: now.UTC(),
	}
}
