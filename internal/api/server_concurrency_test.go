package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
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
