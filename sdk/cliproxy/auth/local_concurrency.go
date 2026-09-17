package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// MaxInFlightMetadataKey is the persisted per-credential limit for standalone CPA.
const MaxInFlightMetadataKey = "max_in_flight"

// ParseMaxInFlight accepts a non-negative integer; zero or null disables the limit.
func ParseMaxInFlight(value any) (int64, error) {
	if value == nil {
		return 0, nil
	}
	var raw string
	if text, ok := value.(string); ok {
		raw = strings.TrimSpace(text)
		if raw == "" {
			return 0, nil
		}
	} else {
		data, errMarshal := json.Marshal(value)
		if errMarshal != nil {
			return 0, fmt.Errorf("max_in_flight must be an integer between 0 and 1000000")
		}
		raw = string(data)
	}
	limit, errParse := strconv.ParseInt(raw, 10, 64)
	if errParse != nil || limit < 0 || limit > 1_000_000 {
		return 0, fmt.Errorf("max_in_flight must be an integer between 0 and 1000000")
	}
	return limit, nil
}

// ValidateAuthConcurrency rejects invalid persisted limits instead of silently disabling them.
func ValidateAuthConcurrency(auth *Auth) error {
	if auth == nil {
		return nil
	}
	_, errParse := ParseMaxInFlight(auth.Metadata[MaxInFlightMetadataKey])
	return errParse
}

// LocalConcurrencyState is a process-local admission snapshot, shared across all models.
type LocalConcurrencyState struct {
	MaxInFlight      int64 `json:"max_in_flight"`
	AdmittedInFlight int64 `json:"admitted_in_flight"`
}

// LocalConcurrency returns the current policy and admitted count without exposing credentials.
func (m *Manager) LocalConcurrency(authID string) LocalConcurrencyState {
	if m == nil {
		return LocalConcurrencyState{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	state := LocalConcurrencyState{AdmittedInFlight: m.localInFlight[authID]}
	if auth := m.auths[authID]; auth != nil {
		state.MaxInFlight, _ = ParseMaxInFlight(auth.Metadata[MaxInFlightMetadataKey])
	}
	return state
}

type localConcurrencyBusyError struct{}

func (*localConcurrencyBusyError) Error() string {
	return "credential_concurrency_exceeded: credential concurrency limit reached"
}
func (*localConcurrencyBusyError) StatusCode() int { return http.StatusTooManyRequests }
func (*localConcurrencyBusyError) RetryAfter() *time.Duration {
	delay := time.Second
	return &delay
}
func isLocalConcurrencyBusy(err error) bool {
	var busy *localConcurrencyBusyError
	return errors.As(err, &busy)
}

// A lease retains admission until both its caller and every upstream stream finish.
// Cancellation may end a downstream wrapper before an executor closes its channel.
type localConcurrencyLease struct {
	mu      sync.Mutex
	refs    int
	once    sync.Once
	manager *Manager
	authID  string
}

func (m *Manager) acquireLocalConcurrency(auth *Auth) (*localConcurrencyLease, error) {
	if m == nil || m.HomeEnabled() {
		return nil, nil
	}
	if auth == nil {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.auths[auth.ID]
	if current == nil || current.Disabled || current.Status == StatusDisabled {
		return nil, &Error{Code: "auth_unavailable", Message: "credential is no longer available"}
	}
	limit, errParse := ParseMaxInFlight(current.Metadata[MaxInFlightMetadataKey])
	if errParse != nil {
		return nil, &Error{Code: "invalid_concurrency_limit", Message: errParse.Error(), HTTPStatus: http.StatusServiceUnavailable}
	}
	if limit > 0 && m.localInFlight[auth.ID] >= limit {
		return nil, &localConcurrencyBusyError{}
	}
	if m.localInFlight == nil {
		m.localInFlight = make(map[string]int64)
	}
	// Count unlimited requests too so lowering a live policy cannot over-admit.
	m.localInFlight[auth.ID]++
	return &localConcurrencyLease{refs: 1, manager: m, authID: auth.ID}, nil
}

func (l *localConcurrencyLease) releaseOwner() {
	if l != nil {
		l.once.Do(l.releaseRef)
	}
}

func (l *localConcurrencyLease) releaseRef() {
	l.mu.Lock()
	l.refs--
	finished := l.refs == 0
	l.mu.Unlock()
	if finished {
		l.manager.mu.Lock()
		l.manager.localInFlight[l.authID]--
		if l.manager.localInFlight[l.authID] == 0 {
			delete(l.manager.localInFlight, l.authID)
		}
		l.manager.mu.Unlock()
	}
}

type localConcurrencyContextKey struct{}

func withLocalConcurrency(ctx context.Context, lease *localConcurrencyLease) context.Context {
	if lease == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, localConcurrencyContextKey{}, lease)
}

// trackLocalUpstreamStream must wrap raw executor output, before bootstrap processing.
func trackLocalUpstreamStream(ctx context.Context, stream *cliproxyexecutor.StreamResult, executionErr error) *cliproxyexecutor.StreamResult {
	if ctx == nil || stream == nil || stream.Chunks == nil {
		return stream
	}
	lease, _ := ctx.Value(localConcurrencyContextKey{}).(*localConcurrencyLease)
	if lease == nil {
		return stream
	}
	lease.mu.Lock()
	lease.refs++
	lease.mu.Unlock()
	out := make(chan cliproxyexecutor.StreamChunk)
	result := *stream
	result.Chunks = out
	go func() {
		defer close(out)
		defer lease.releaseRef()
		for chunk := range stream.Chunks {
			select {
			case out <- chunk:
			case <-ctx.Done():
				// Drain without releasing admission until upstream acknowledges cancellation.
				for range stream.Chunks {
				}
				return
			}
		}
	}()
	if executionErr != nil {
		discardStreamChunks(result.Chunks)
	}
	return &result
}

func finishLocalConcurrencyStream(ctx context.Context, stream *cliproxyexecutor.StreamResult, lease *localConcurrencyLease) *cliproxyexecutor.StreamResult {
	if lease == nil {
		return stream
	}
	if stream == nil || stream.Chunks == nil {
		lease.releaseOwner()
		return stream
	}
	if ctx == nil {
		ctx = context.Background()
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	result := *stream
	result.Chunks = out
	go func() {
		defer close(out)
		defer lease.releaseOwner()
		for chunk := range stream.Chunks {
			select {
			case out <- chunk:
			case <-ctx.Done():
				for range stream.Chunks {
				}
				return
			}
		}
	}()
	return &result
}
