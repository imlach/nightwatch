package api

import "sync"

// actuatorLocks is a same-process guard for mutating node actuator calls. It is
// intentionally local to this HTTP server; it does not try to coordinate across
// Nightwatch processes.
type actuatorLocks struct {
	mu       sync.Mutex
	inFlight map[string]struct{}
}

func (l *actuatorLocks) tryAcquire(node string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.inFlight == nil {
		l.inFlight = make(map[string]struct{})
	}
	if _, ok := l.inFlight[node]; ok {
		return nil, false
	}
	l.inFlight[node] = struct{}{}
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.inFlight, node)
	}, true
}
