package config

import (
	"fmt"
	"os"
	"strings"
)

// ImageStorageConfig mirrors chatgpt2api's image storage contract while keeping
// credentials out of tracked configuration files. COS is the remote persistence
// backend implemented by CLIProxyAPI; other accepted modes retain inline URLs.
type ImageStorageConfig struct {
	Enabled       bool   `yaml:"enabled" json:"enabled"`
	Mode          string `yaml:"mode" json:"mode"`
	COSSecretID   string `yaml:"cos-secret-id,omitempty" json:"-"`
	COSSecretKey  string `yaml:"cos-secret-key,omitempty" json:"-"`
	COSRegion     string `yaml:"cos-region,omitempty" json:"cos-region,omitempty"`
	COSBucket     string `yaml:"cos-bucket,omitempty" json:"cos-bucket,omitempty"`
	COSPathPrefix string `yaml:"cos-path-prefix,omitempty" json:"cos-path-prefix,omitempty"`
	PublicBaseURL string `yaml:"public-base-url,omitempty" json:"public-base-url,omitempty"`
}

const (
	imageStorageDefaultMode       = "local"
	imageStorageDefaultPathPrefix = "generated/"
)

var errInvalidImageStorageCOSConfig = fmt.Errorf("image-storage COS requires secret id, secret key, region, and bucket")

// Normalize applies the same mode and prefix defaults as chatgpt2api.
func (c *ImageStorageConfig) Normalize() {
	if c == nil {
		return
	}
	c.Mode = strings.ToLower(strings.TrimSpace(c.Mode))
	if c.Mode != "local" && c.Mode != "webdav" && c.Mode != "both" && c.Mode != "cos" {
		c.Mode = imageStorageDefaultMode
	}
	if !c.Enabled {
		c.Mode = imageStorageDefaultMode
	}
	c.COSSecretID = strings.TrimSpace(c.COSSecretID)
	c.COSSecretKey = strings.TrimSpace(c.COSSecretKey)
	c.COSRegion = strings.TrimSpace(c.COSRegion)
	c.COSBucket = strings.TrimSpace(c.COSBucket)
	c.COSPathPrefix = strings.Trim(strings.TrimSpace(c.COSPathPrefix), "/")
	if c.COSPathPrefix == "" {
		c.COSPathPrefix = strings.Trim(imageStorageDefaultPathPrefix, "/")
	}
	c.PublicBaseURL = strings.TrimRight(strings.TrimSpace(c.PublicBaseURL), "/")
}

// ApplyEnvironmentOverrides uses chatgpt2api's production variable names. A
// non-empty variable wins over YAML, while an absent/empty variable preserves
// the configured value just like chatgpt2api.
func (c *ImageStorageConfig) ApplyEnvironmentOverrides() {
	if c == nil {
		return
	}
	applyBoolEnv(&c.Enabled, "CHATGPT2API_IMAGE_STORAGE_ENABLED", "CLIPROXY_IMAGE_STORAGE_ENABLED")
	applyStringEnv(&c.Mode, "CHATGPT2API_IMAGE_STORAGE_MODE", "CLIPROXY_IMAGE_STORAGE_MODE")
	applyStringEnv(&c.COSSecretID, "CHATGPT2API_COS_SECRET_ID", "CLIPROXY_COS_SECRET_ID")
	applyStringEnv(&c.COSSecretKey, "CHATGPT2API_COS_SECRET_KEY", "CLIPROXY_COS_SECRET_KEY")
	applyStringEnv(&c.COSRegion, "CHATGPT2API_COS_REGION", "CLIPROXY_COS_REGION")
	applyStringEnv(&c.COSBucket, "CHATGPT2API_COS_BUCKET", "CLIPROXY_COS_BUCKET")
	applyStringEnv(&c.COSPathPrefix, "CHATGPT2API_COS_PATH_PREFIX", "CLIPROXY_COS_PATH_PREFIX")
	applyStringEnv(&c.PublicBaseURL, "CHATGPT2API_IMAGE_PUBLIC_BASE_URL", "CLIPROXY_IMAGE_PUBLIC_BASE_URL")
	c.Normalize()
}

func applyStringEnv(target *string, names ...string) {
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok && strings.TrimSpace(value) != "" {
			*target = value
			return
		}
	}
}

func applyBoolEnv(target *bool, names ...string) {
	for _, name := range names {
		value, ok := os.LookupEnv(name)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes", "y", "on":
			*target = true
		case "0", "false", "no", "n", "off":
			*target = false
		}
		return
	}
}

// Validate checks the required fields for the selected remote storage mode.
func (c ImageStorageConfig) Validate() error {
	if !c.Enabled || c.Mode != "cos" {
		return nil
	}
	if c.COSSecretID == "" || c.COSSecretKey == "" || c.COSRegion == "" || c.COSBucket == "" {
		return errInvalidImageStorageCOSConfig
	}
	return nil
}
