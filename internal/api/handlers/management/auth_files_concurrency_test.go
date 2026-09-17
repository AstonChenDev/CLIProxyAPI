package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestAuthFileConcurrencyPersistenceAndValidation(t *testing.T) {
	dir := t.TempDir()
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	name := "limit-test.json"
	record := &coreauth.Auth{ID: name, FileName: name, Provider: "codex", Attributes: map[string]string{"path": filepath.Join(dir, name)}, Metadata: map[string]any{"type": "codex", "note": "preserve me"}}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatal(errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
	patch := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(body))
		h.PatchAuthFileFields(c)
		return r
	}
	if r := patch(`{"name":"limit-test.json","max_in_flight":2}`); r.Code != 200 {
		t.Fatalf("save failed: %d %s", r.Code, r.Body.String())
	}
	for _, value := range []string{"-1", "1.5", "true", "1000001", `{"nested":1}`} {
		if r := patch(`{"name":"limit-test.json","max_in_flight":` + value + `}`); r.Code != 400 {
			t.Fatalf("accepted invalid limit %s: %d %s", value, r.Code, r.Body.String())
		}
	}
	if r := patch(`{"name":"limit-test.json","max_in_flight.nested":1}`); r.Code != 400 {
		t.Fatalf("accepted nested limit: %d", r.Code)
	}
	reloaded := coreauth.NewManager(store, nil, nil)
	if errLoad := reloaded.Load(context.Background()); errLoad != nil {
		t.Fatal(errLoad)
	}
	if got := reloaded.LocalConcurrency(name).MaxInFlight; got != 2 {
		t.Fatalf("persisted limit = %d, want 2", got)
	}
	auth, ok := reloaded.GetByID(name)
	if !ok || auth.Metadata["note"] != "preserve me" {
		t.Fatal("editing the limit lost unrelated metadata")
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	h.ListAuthFiles(c)
	var listing struct {
		Files []struct {
			MaxInFlight int64 `json:"max_in_flight"`
			InFlight    int64 `json:"admitted_in_flight"`
		} `json:"files"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &listing); errDecode != nil || len(listing.Files) != 1 || listing.Files[0].MaxInFlight != 2 || listing.Files[0].InFlight != 0 {
		t.Fatalf("invalid limit listing: %s", rec.Body.String())
	}
	if r := patch(`{"name":"limit-test.json","max_in_flight":null}`); r.Code != 200 {
		t.Fatalf("clear failed: %s", r.Body.String())
	}
	if errLoad := reloaded.Load(context.Background()); errLoad != nil {
		t.Fatal(errLoad)
	}
	if reloaded.LocalConcurrency(name).MaxInFlight != 0 {
		t.Fatal("clearing the limit was not persisted")
	}
}
