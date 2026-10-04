package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestConcurrencyPageHonorsStandaloneAndPanelSettings(t *testing.T) {
	for _, test := range []struct {
		name     string
		home     bool
		disabled bool
		want     int
	}{
		{"standalone", false, false, http.StatusOK},
		{"home", true, false, http.StatusNotFound},
		{"disabled", false, true, http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Home.Enabled = test.home
			cfg.RemoteManagement.DisableControlPanel = test.disabled
			s := &Server{cfg: cfg}
			router := gin.New()
			router.GET("/credential-concurrency.html", s.serveCredentialConcurrency)
			router.GET("/credential-concurrency-assets/:asset", s.serveCredentialConcurrency)
			for _, path := range []string{"/credential-concurrency.html", "/credential-concurrency-assets/concurrency.js", "/credential-concurrency-assets/concurrency.css"} {
				r := httptest.NewRecorder()
				router.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
				if r.Code != test.want {
					t.Fatalf("%s status = %d, want %d", path, r.Code, test.want)
				}
				if test.want == 200 && (!strings.Contains(r.Header().Get("Content-Security-Policy"), "connect-src 'self'") || r.Body.Len() == 0) {
					t.Fatal("missing bundled content or security policy")
				}
			}
			r := httptest.NewRecorder()
			router.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/credential-concurrency-assets/config.yaml", nil))
			if r.Code != 404 {
				t.Fatal("unexpected asset access")
			}
		})
	}
}

func TestManagementV8PreservesCredentialConcurrencyAndStatus(t *testing.T) {
	dir := t.TempDir()
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	name := "v8-concurrency.json"
	credential := &coreauth.Auth{ID: name, FileName: name, Provider: "codex", Attributes: map[string]string{"path": filepath.Join(dir, name)}, Metadata: map[string]any{"type": "codex"}}
	if _, errRegister := manager.Register(context.Background(), credential); errRegister != nil {
		t.Fatal(errRegister)
	}
	cfg, errConfig := config.ParseConfigBytes([]byte("remote-management: {secret-key: test-password}\n"))
	if errConfig != nil {
		t.Fatal(errConfig)
	}
	cfg.AuthDir = dir
	h := management.NewHandlerWithoutConfigFilePath(cfg, manager)
	h.SetLocalPassword("test-password")
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementRoutes()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/v8/management/"+path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Management-Key", "test-password")
		response := httptest.NewRecorder()
		s.engine.ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("%s %s: status=%d body=%s", method, path, response.Code, response.Body.String())
		}
		return response
	}
	request(http.MethodPatch, "credentials/fields", `{"name":"v8-concurrency.json","max_in_flight":2}`)
	request(http.MethodPatch, "credentials/status", `{"name":"v8-concurrency.json","disabled":true}`)
	var listing struct {
		Files []struct {
			Disabled    bool  `json:"disabled"`
			MaxInFlight int64 `json:"max_in_flight"`
		} `json:"files"`
	}
	if err := json.Unmarshal(request(http.MethodGet, "credentials", "").Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Files) != 1 || !listing.Files[0].Disabled || listing.Files[0].MaxInFlight != 2 {
		t.Fatalf("v8 status update lost credential policy: %+v", listing)
	}
	reloaded := coreauth.NewManager(store, nil, nil)
	if err := reloaded.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if saved, ok := reloaded.GetByID(name); !ok || !saved.Disabled || reloaded.LocalConcurrency(name).MaxInFlight != 2 {
		t.Fatal("v8 status and concurrency changes did not survive reload")
	}
}
