package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageStorageApplyEnvironmentOverridesMatchesChatGPT2API(t *testing.T) {
	t.Setenv("CHATGPT2API_IMAGE_STORAGE_ENABLED", "true")
	t.Setenv("CHATGPT2API_IMAGE_STORAGE_MODE", "cos")
	t.Setenv("CHATGPT2API_COS_SECRET_ID", "secret-id")
	t.Setenv("CHATGPT2API_COS_SECRET_KEY", "secret-key")
	t.Setenv("CHATGPT2API_COS_REGION", "ap-shanghai")
	t.Setenv("CHATGPT2API_COS_BUCKET", "bucket-123456")
	t.Setenv("CHATGPT2API_COS_PATH_PREFIX", "generated/")
	t.Setenv("CHATGPT2API_IMAGE_PUBLIC_BASE_URL", "https://cdn.example.test/images/")

	var got ImageStorageConfig
	got.ApplyEnvironmentOverrides()
	if !got.Enabled || got.Mode != "cos" {
		t.Fatalf("enabled/mode = %v/%q, want true/cos", got.Enabled, got.Mode)
	}
	if got.COSSecretID != "secret-id" || got.COSSecretKey != "secret-key" {
		t.Fatalf("COS credentials were not loaded from environment")
	}
	if got.COSRegion != "ap-shanghai" || got.COSBucket != "bucket-123456" {
		t.Fatalf("COS location = %q/%q", got.COSRegion, got.COSBucket)
	}
	if got.COSPathPrefix != "generated" || got.PublicBaseURL != "https://cdn.example.test/images" {
		t.Fatalf("normalized path/base URL = %q/%q", got.COSPathPrefix, got.PublicBaseURL)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestImageStorageValidateRejectsIncompleteCOSConfig(t *testing.T) {
	got := ImageStorageConfig{Enabled: true, Mode: "cos"}
	if err := got.Validate(); err == nil {
		t.Fatal("Validate() accepted incomplete COS configuration")
	}
}

func TestImageStorageV8MigrationAndSavePreserveCOSSettings(t *testing.T) {
	legacy := []byte("port: 8317\nimage-storage:\n  enabled: true\n  mode: cos\n  cos-secret-id: test-id\n  cos-secret-key: test-secret\n  cos-region: ap-beijing\n  cos-bucket: test-bucket\n  cos-path-prefix: generated\n  public-base-url: https://cdn.example.test\n")
	cfg, err := ParseConfigBytes(legacy)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Debug = true
	if err = SaveConfigPreserveComments(path, cfg, true); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateV8Config(saved); err != nil {
		t.Fatalf("migrated image storage is not valid v8 config: %v", err)
	}
	if !strings.Contains(string(saved), "multimedia:") || !strings.Contains(string(saved), "  image-storage:") {
		t.Fatalf("image storage did not migrate under multimedia: %s", saved)
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ImageStorage != cfg.ImageStorage || !reloaded.Debug {
		t.Fatal("v8 config save lost COS settings or the edited setting")
	}
	public, err := json.Marshal(reloaded)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), "test-secret") || strings.Contains(string(public), "test-id") {
		t.Fatal("JSON configuration exposed COS credentials")
	}
}
