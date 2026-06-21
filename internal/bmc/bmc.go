package bmc

import "context"

type PowerState string

const (
	PowerOn      PowerState = "on"
	PowerOff     PowerState = "off"
	PowerUnknown PowerState = "unknown"
)

type Result struct {
	OK         bool
	PowerState PowerState
	Error      string
	Raw        string
}

type Adapter interface {
	GetPowerState(ctx context.Context) Result
	PowerOn(ctx context.Context) Result
	SoftOff(ctx context.Context) Result
	HardOff(ctx context.Context) Result
	Reset(ctx context.Context) Result
}

func ErrorResult(err error) Result {
	return Result{OK: false, PowerState: PowerUnknown, Error: err.Error()}
}
