package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type localLimitExecutor struct {
	execute func(context.Context, *Auth) (cliproxyexecutor.Response, error)
	stream  func(context.Context, *Auth) (*cliproxyexecutor.StreamResult, error)
}

func (*localLimitExecutor) Identifier() string { return "codex" }
func (e *localLimitExecutor) Execute(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e.execute != nil {
		return e.execute(ctx, a)
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}
func (e *localLimitExecutor) CountTokens(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, a, req, opts)
}
func (e *localLimitExecutor) ExecuteStream(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if e.stream != nil {
		return e.stream(ctx, a)
	}
	return successStreamResult(), nil
}
func (*localLimitExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) { return a, nil }
func (*localLimitExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func localLimitFixture(t *testing.T, limits ...int64) (*Manager, *localLimitExecutor, []string, cliproxyexecutor.Request) {
	t.Helper()
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, 1)
	e := &localLimitExecutor{}
	m.RegisterExecutor(e)
	model := "local-concurrency-model"
	var ids []string
	for i, limit := range limits {
		id := fmt.Sprintf("%s-%d", t.Name(), i)
		a := &Auth{ID: id, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": fmt.Sprint(100 - i)}, Metadata: map[string]any{MaxInFlightMetadataKey: limit}}
		if _, errRegister := m.Register(context.Background(), a); errRegister != nil {
			t.Fatal(errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
		ids = append(ids, id)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			registry.GetGlobalRegistry().UnregisterClient(id)
		}
	})
	return m, e, ids, cliproxyexecutor.Request{Model: model}
}

func receiveLocal[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for test execution")
		var zero T
		return zero
	}
}

func waitLocalCount(t *testing.T, m *Manager, id string, want int64) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for m.LocalConcurrency(id).AdmittedInFlight != want {
		select {
		case <-timer.C:
			t.Fatalf("admitted = %d, want %d", m.LocalConcurrency(id).AdmittedInFlight, want)
		case <-tick.C:
		}
	}
}

func TestLocalConcurrencyAtomicAdmission(t *testing.T) {
	m, e, ids, req := localLimitFixture(t, 3)
	entered := make(chan struct{}, 64)
	finish := make(chan struct{})
	var finishOnce sync.Once
	t.Cleanup(func() { finishOnce.Do(func() { close(finish) }) })
	e.execute = func(context.Context, *Auth) (cliproxyexecutor.Response, error) {
		entered <- struct{}{}
		<-finish
		return cliproxyexecutor.Response{}, nil
	}
	results := make(chan error, 64)
	for range 64 {
		go func() {
			_, errExecute := m.Execute(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{})
			results <- errExecute
		}()
	}
	for range 3 {
		receiveLocal(t, entered)
	}
	for range 61 {
		errExecute := receiveLocal(t, results)
		if !isLocalConcurrencyBusy(errExecute) || statusCodeFromError(errExecute) != 429 {
			t.Fatalf("expected local 429, got %v", errExecute)
		}
		if got := SafeResponseHeaders(errExecute).Get("Retry-After"); got != "1" {
			t.Fatalf("Retry-After = %q", got)
		}
	}
	if state := m.LocalConcurrency(ids[0]); state.AdmittedInFlight != 3 {
		t.Fatalf("admission exceeded limit: %+v", state)
	}
	a, _ := m.GetByID(ids[0])
	if a.Failed != 0 || a.Quota.Exceeded || a.Unavailable {
		t.Fatal("local saturation must not count as upstream failure or cool down the credential")
	}
	finishOnce.Do(func() { close(finish) })
	for range 3 {
		if errExecute := receiveLocal(t, results); errExecute != nil {
			t.Fatal(errExecute)
		}
	}
	waitLocalCount(t, m, ids[0], 0)
}

func TestLocalConcurrencyRotatesWithoutConsumingRetryBudget(t *testing.T) {
	m, e, ids, req := localLimitFixture(t, 1, 1)
	first, _ := m.GetByID(ids[0])
	lease, errAdmission := m.acquireLocalConcurrency(first)
	if errAdmission != nil {
		t.Fatal(errAdmission)
	}
	defer lease.releaseOwner()
	e.execute = func(_ context.Context, a *Auth) (cliproxyexecutor.Response, error) {
		if a.ID != ids[1] {
			t.Errorf("selected saturated credential %s", a.ID)
		}
		return cliproxyexecutor.Response{}, nil
	}
	if _, errExecute := m.Execute(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{}); errExecute != nil {
		t.Fatal(errExecute)
	}
	if _, errCount := m.ExecuteCount(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{}); errCount != nil {
		t.Fatal(errCount)
	}
	stream, errStream := m.ExecuteStream(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{})
	if errStream != nil {
		t.Fatal(errStream)
	}
	for range stream.Chunks {
	}
	waitLocalCount(t, m, ids[1], 0)
}

func TestLocalConcurrencyLiveLimitAndReload(t *testing.T) {
	m, _, ids, _ := localLimitFixture(t, 0)
	a, _ := m.GetByID(ids[0])
	first, _ := m.acquireLocalConcurrency(a)
	second, _ := m.acquireLocalConcurrency(a)
	defer first.releaseOwner()
	defer second.releaseOwner()
	a.Metadata[MaxInFlightMetadataKey] = int64(1)
	if _, errUpdate := m.Update(context.Background(), a); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if _, errAdmission := m.acquireLocalConcurrency(a); !isLocalConcurrencyBusy(errAdmission) {
		t.Fatalf("lowering a limit must include existing unlimited requests: %v", errAdmission)
	}
	if _, errRegister := m.Register(context.Background(), a.Clone()); errRegister != nil {
		t.Fatal(errRegister)
	}
	first.releaseOwner()
	first.releaseOwner()
	if _, errAdmission := m.acquireLocalConcurrency(a); !isLocalConcurrencyBusy(errAdmission) {
		t.Fatalf("re-registering and double release must not reset counts: %v", errAdmission)
	}
	second.releaseOwner()
	third, errAdmission := m.acquireLocalConcurrency(a)
	if errAdmission != nil {
		t.Fatal(errAdmission)
	}
	third.releaseOwner()
	waitLocalCount(t, m, ids[0], 0)
}

func TestLocalConcurrencyStreamCancellationWaitsForUpstream(t *testing.T) {
	m, e, ids, req := localLimitFixture(t, 1)
	raw := make(chan cliproxyexecutor.StreamChunk, 1)
	raw <- cliproxyexecutor.StreamChunk{Payload: []byte("data: first\n\n")}
	var closeOnce sync.Once
	t.Cleanup(func() { closeOnce.Do(func() { close(raw) }) })
	e.stream = func(context.Context, *Auth) (*cliproxyexecutor.StreamResult, error) {
		return &cliproxyexecutor.StreamResult{Chunks: raw}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, errStream := m.ExecuteStream(ctx, []string{"codex"}, req, cliproxyexecutor.Options{})
	if errStream != nil {
		t.Fatal(errStream)
	}
	receiveLocal(t, stream.Chunks)
	cancel()
	if _, errExecute := m.Execute(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{}); !isLocalConcurrencyBusy(errExecute) {
		t.Fatalf("cancellation released upstream capacity prematurely: %v", errExecute)
	}
	closeOnce.Do(func() { close(raw) })
	for range stream.Chunks {
	}
	waitLocalCount(t, m, ids[0], 0)
}

func TestLocalConcurrencyFailedBootstrapDrainsBeforeRelease(t *testing.T) {
	m, e, ids, req := localLimitFixture(t, 1)
	raw := make(chan cliproxyexecutor.StreamChunk, 1)
	raw <- cliproxyexecutor.StreamChunk{Err: &Error{HTTPStatus: 400, Message: "bad request"}}
	e.stream = func(context.Context, *Auth) (*cliproxyexecutor.StreamResult, error) {
		return &cliproxyexecutor.StreamResult{Chunks: raw}, nil
	}
	_, errStream := m.ExecuteStream(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{})
	if errStream == nil {
		t.Fatal("expected bootstrap error")
	}
	if got := m.LocalConcurrency(ids[0]).AdmittedInFlight; got != 1 {
		t.Fatalf("stream still draining, admitted = %d", got)
	}
	close(raw)
	waitLocalCount(t, m, ids[0], 0)
}

func TestLocalConcurrencyInvalidMetadataAndRefresh(t *testing.T) {
	for _, raw := range []any{-1, 1.5, true, json.Number("1000001"), map[string]any{"x": 1}} {
		if _, errParse := ParseMaxInFlight(raw); errParse == nil {
			t.Errorf("accepted invalid limit %#v", raw)
		}
		m := NewManager(nil, nil, nil)
		if _, errRegister := m.Register(context.Background(), &Auth{ID: "invalid", Metadata: map[string]any{MaxInFlightMetadataKey: raw}}); errRegister == nil {
			t.Errorf("registered invalid limit %#v", raw)
		}
	}
	m, _, ids, _ := localLimitFixture(t, 2)
	base, _ := m.GetByID(ids[0])
	current := base.Clone()
	current.Metadata[MaxInFlightMetadataKey] = int64(5)
	if _, errUpdate := m.Update(context.Background(), current); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	refreshed := base.Clone()
	refreshed.Metadata["access_token"] = "new-token"
	if _, errRefresh := m.UpdateRefreshedAuth(context.Background(), base, refreshed); errRefresh != nil {
		t.Fatal(errRefresh)
	}
	if m.LocalConcurrency(ids[0]).MaxInFlight != 5 {
		t.Fatal("token refresh replaced the newer concurrency policy")
	}
}

func TestLocalConcurrencyExecuteFailureAndCancellationRelease(t *testing.T) {
	m, e, ids, req := localLimitFixture(t, 1)
	e.execute = func(context.Context, *Auth) (cliproxyexecutor.Response, error) {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: 400, Message: "invalid input"}
	}
	if _, errExecute := m.Execute(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{}); errExecute == nil {
		t.Fatal("expected execution error")
	}
	waitLocalCount(t, m, ids[0], 0)
	entered := make(chan struct{}, 1)
	e.execute = func(ctx context.Context, _ *Auth) (cliproxyexecutor.Response, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return cliproxyexecutor.Response{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, errExecute := m.Execute(ctx, []string{"codex"}, req, cliproxyexecutor.Options{})
		done <- errExecute
	}()
	receiveLocal(t, entered)
	cancel()
	if errExecute := receiveLocal(t, done); !errors.Is(errExecute, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", errExecute)
	}
	waitLocalCount(t, m, ids[0], 0)
}

type localConcurrencyFailingStore struct{ weightValidationStore }

func (*localConcurrencyFailingStore) Save(context.Context, *Auth) (string, error) {
	return "", errors.New("storage unavailable")
}

func TestLocalConcurrencyUpdateReportsPersistenceFailure(t *testing.T) {
	m := NewManager(&localConcurrencyFailingStore{}, nil, nil)
	a := &Auth{ID: "persist-limit", Metadata: map[string]any{MaxInFlightMetadataKey: 1}}
	if _, errRegister := m.Register(context.Background(), a); errRegister != nil {
		t.Fatal(errRegister)
	}
	a, _ = m.GetByID(a.ID)
	a.Metadata[MaxInFlightMetadataKey] = 2
	if _, errUpdate := m.Update(context.Background(), a); errUpdate == nil {
		t.Fatal("must not report a limit as saved when its store fails")
	}
}
