package bmc

import (
	"fmt"
	"sync"
)

// Factory builds an Adapter for one BMC endpoint.
type Factory func(host, username, password string) Adapter

// Driver registry, keyed by bmc.type. Drivers self-register via Register from
// their package init, so bmc stays free of backend imports (amtwsman imports
// bmc, so bmc importing it back would cycle).
var (
	driversMu sync.RWMutex
	drivers   = map[string]Factory{}
)

// Register makes a BMC driver available under typ. It panics on a nil factory
// or a duplicate type, mirroring database/sql.Register.
func Register(typ string, f Factory) {
	driversMu.Lock()
	defer driversMu.Unlock()
	if f == nil {
		panic("bmc: Register factory is nil")
	}
	if _, dup := drivers[typ]; dup {
		panic("bmc: Register called twice for type " + typ)
	}
	drivers[typ] = f
}

// New builds an Adapter for a registered bmc.type. Unknown types return a
// not-supported error rather than a nil adapter.
func New(typ, host, username, password string) (Adapter, error) {
	driversMu.RLock()
	f, ok := drivers[typ]
	driversMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("bmc: type %q not supported", typ)
	}
	return f(host, username, password), nil
}
