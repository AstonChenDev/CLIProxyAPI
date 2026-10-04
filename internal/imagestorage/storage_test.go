package imagestorage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestRewriteOpenAIResponseAddsURLAlongsideBase64(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ImageStorage: config.ImageStorageConfig{
		Enabled: true,
		Mode:    "local",
	}}}
	encoded := base64.StdEncoding.EncodeToString([]byte("image-bytes"))
	payload := []byte(`{"created":1,"data":[{"b64_json":"` + encoded + `"}]}`)

	out, errRewrite := RewriteOpenAIResponse(context.Background(), cfg, payload, "b64_json")
	if errRewrite != nil {
		t.Fatalf("RewriteOpenAIResponse() error = %v", errRewrite)
	}
	var got struct {
		Data []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode rewritten response: %v", err)
	}
	item := got.Data[0]
	if item["b64_json"] != encoded {
		t.Fatalf("b64_json = %q, want %q", item["b64_json"], encoded)
	}
	if !strings.HasPrefix(item["url"], "data:image/png;base64,") {
		t.Fatalf("url = %q, want inline image URL", item["url"])
	}
}

func TestRewriteOpenAIResponseURLFormatOmitsBase64(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ImageStorage: config.ImageStorageConfig{
		Enabled: true,
		Mode:    "local",
	}}}
	encoded := base64.StdEncoding.EncodeToString([]byte("image-bytes"))
	payload := []byte(`{"created":1,"data":[{"b64_json":"` + encoded + `"}]}`)

	out, errRewrite := RewriteOpenAIResponse(context.Background(), cfg, payload, "url")
	if errRewrite != nil {
		t.Fatalf("RewriteOpenAIResponse() error = %v", errRewrite)
	}
	var root struct {
		Data []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("decode rewritten response: %v", err)
	}
	item := root.Data[0]
	if _, ok := item["b64_json"]; ok {
		t.Fatalf("b64_json should be omitted for response_format=url")
	}
	if !strings.HasPrefix(item["url"], "data:image/png;base64,") {
		t.Fatalf("url = %q, want inline image URL", item["url"])
	}
}

func TestRewriteOpenAIResponseStoresInlineURLResult(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ImageStorage: config.ImageStorageConfig{
		Enabled: true,
		Mode:    "local",
	}}}
	encoded := base64.StdEncoding.EncodeToString([]byte("image-bytes"))
	payload := []byte(`{"created":1,"output_format":"png","data":[{"url":"data:image/webp;base64,` + encoded + `"}]}`)

	out, errRewrite := RewriteOpenAIResponse(context.Background(), cfg, payload, "url")
	if errRewrite != nil {
		t.Fatalf("RewriteOpenAIResponse() error = %v", errRewrite)
	}
	var root struct {
		Data []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("decode rewritten response: %v", err)
	}
	if got := root.Data[0]["url"]; !strings.HasPrefix(got, "data:image/webp;base64,") {
		t.Fatalf("url = %q, want stored WebP URL", got)
	}
}

func TestRewriteOpenAIResponseDisabledLeavesPayloadUntouched(t *testing.T) {
	cfg := &config.Config{}
	payload := []byte(`{"created":1,"data":[{"b64_json":"AA=="}]}`)
	out, errRewrite := RewriteOpenAIResponse(context.Background(), cfg, payload, "b64_json")
	if errRewrite != nil {
		t.Fatalf("RewriteOpenAIResponse() error = %v", errRewrite)
	}
	if string(out) != string(payload) {
		t.Fatalf("disabled storage changed payload: %s", out)
	}
}

func TestCOSProbeIntegration(t *testing.T) {
	if os.Getenv("CLIPROXY_RUN_COS_INTEGRATION") != "1" {
		t.Skip("set CLIPROXY_RUN_COS_INTEGRATION=1 to probe a real COS bucket")
	}
	cfg := &config.Config{}
	cfg.ImageStorage.ApplyEnvironmentOverrides()
	if !cfg.ImageStorage.Enabled || cfg.ImageStorage.Mode != "cos" {
		t.Skip("COS image storage is not enabled in the current environment")
	}
	if err := Probe(context.Background(), cfg); err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
}
