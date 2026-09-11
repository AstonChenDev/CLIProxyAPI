package config

import "testing"

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
