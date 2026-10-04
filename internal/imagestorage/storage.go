// Package imagestorage persists generated image bytes and returns an
// OpenAI-compatible URL for the response payload.
package imagestorage

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
	"github.com/tencentyun/cos-go-sdk-v5"
)

const maxImageBytes = 25 * 1024 * 1024

// StoreBase64 uploads one base64-encoded image when COS storage is enabled and
// returns a data URL when it is not. The returned URL never contains credentials.
func StoreBase64(ctx context.Context, cfg *config.Config, encoded, outputFormat string) (string, error) {
	data, errDecode := decodeBase64(encoded)
	if errDecode != nil {
		return "", errDecode
	}
	return StoreBytes(ctx, cfg, data, mimeType(outputFormat))
}

func decodeBase64(encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, fmt.Errorf("image result is empty")
	}
	if strings.HasPrefix(encoded, "data:image/") {
		if _, value, ok := strings.Cut(encoded, ","); ok {
			encoded = value
		}
	}
	data, errDecode := base64.StdEncoding.DecodeString(encoded)
	if errDecode != nil {
		return nil, fmt.Errorf("decode image result: %w", errDecode)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("image result is empty")
	}
	if len(data) > maxImageBytes {
		return nil, fmt.Errorf("image result exceeds %d byte limit", maxImageBytes)
	}
	return data, nil
}

// RewriteOpenAIResponse adds the URL field expected by chatgpt2api to an
// OpenAI images response. With response_format=b64_json both b64_json and url
// are retained; with response_format=url only url is emitted. A nil config
// leaves the payload untouched so low-level executor tests and non-server
// callers retain the historical behavior.
func RewriteOpenAIResponse(ctx context.Context, cfg *config.Config, payload []byte, responseFormat string) ([]byte, error) {
	if cfg == nil {
		return payload, nil
	}
	settings := cfg.ImageStorage
	settings.Normalize()
	if !settings.Enabled {
		return payload, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, fmt.Errorf("decode image response: %w", err)
	}
	var items []map[string]json.RawMessage
	dataRaw, ok := root["data"]
	if !ok || json.Unmarshal(dataRaw, &items) != nil {
		return payload, nil
	}
	format := strings.ToLower(strings.TrimSpace(responseFormat))
	if format == "" {
		format = "b64_json"
	} else if format != "b64_json" {
		format = "url"
	}
	for _, item := range items {
		if item == nil {
			continue
		}
		var encoded string
		if raw, exists := item["b64_json"]; exists {
			_ = json.Unmarshal(raw, &encoded)
		}
		if strings.TrimSpace(encoded) == "" {
			var imageURL string
			if raw, exists := item["url"]; exists {
				_ = json.Unmarshal(raw, &imageURL)
			}
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(imageURL)), "data:image/") {
				encoded = imageURL
			}
		}
		if strings.TrimSpace(encoded) == "" {
			continue
		}
		data, errDecode := decodeBase64(encoded)
		if errDecode != nil {
			return nil, errDecode
		}
		contentType := mimeType("")
		if raw, exists := root["output_format"]; exists {
			var outputFormat string
			_ = json.Unmarshal(raw, &outputFormat)
			contentType = mimeType(outputFormat)
		}
		if raw, exists := item["output_format"]; exists {
			var outputFormat string
			_ = json.Unmarshal(raw, &outputFormat)
			contentType = mimeType(outputFormat)
		}
		if dataURLType := imageDataURLMIMEType(encoded); dataURLType != "" {
			contentType = dataURLType
		}
		storedURL, errStore := StoreBytes(ctx, cfg, data, contentType)
		if errStore != nil {
			// chatgpt2api falls back to local storage on COS failures. Keep the
			// response available as an inline URL when no local web server is
			// configured, while logging only non-sensitive error metadata.
			log.WithError(errStore).Warn("image storage failed; returning inline image URL")
			storedURL = dataURL(data, contentType)
		}
		if format == "url" {
			item["url"], _ = json.Marshal(storedURL)
			delete(item, "b64_json")
		} else {
			item["url"], _ = json.Marshal(storedURL)
		}
	}
	root["data"], _ = json.Marshal(items)
	return json.Marshal(root)
}

func imageDataURLMIMEType(value string) string {
	header, _, ok := strings.Cut(strings.TrimSpace(value), ",")
	if !ok {
		return ""
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(header, ";", 2)[0]))
	mediaType = strings.TrimPrefix(mediaType, "data:")
	if !strings.HasPrefix(mediaType, "image/") {
		return ""
	}
	return mediaType
}

// StoreBytes uploads image data to Tencent COS when configured. If storage is
// disabled, the image is represented as an inline data URL for backward
// compatibility with CLIProxyAPI's existing image response contract.
func StoreBytes(ctx context.Context, cfg *config.Config, data []byte, contentType string) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("image result is empty")
	}
	if len(data) > maxImageBytes {
		return "", fmt.Errorf("image result exceeds %d byte limit", maxImageBytes)
	}
	if cfg == nil {
		return dataURL(data, contentType), nil
	}
	settings := cfg.ImageStorage
	settings.Normalize()
	if !settings.Enabled || settings.Mode != "cos" {
		return dataURL(data, contentType), nil
	}
	if errValidate := settings.Validate(); errValidate != nil {
		return "", errValidate
	}

	relativePath := imageRelativePath(data, contentType)
	key := relativePath
	if settings.COSPathPrefix != "" {
		key = path.Join(settings.COSPathPrefix, relativePath)
	}

	client, endpoint, errClient := newCOSClient(settings)
	if errClient != nil {
		return "", errClient
	}
	response, errPut := client.Object.Put(ctx, key, bytes.NewReader(data), &cos.ObjectPutOptions{
		ObjectPutHeaderOptions: &cos.ObjectPutHeaderOptions{ContentType: contentType},
	})
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if errPut != nil {
		return "", fmt.Errorf("upload image to COS: %w", errPut)
	}
	if response != nil && response.Body != nil {
		_, _ = io.Copy(io.Discard, response.Body)
	}
	if settings.PublicBaseURL != "" {
		return strings.TrimRight(settings.PublicBaseURL, "/") + "/" + key, nil
	}
	return endpoint + "/" + key, nil
}

// Probe performs a bounded, read-only bucket request to verify that the
// configured COS credentials and endpoint are usable. It never creates or
// deletes an object.
func Probe(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("configuration is required")
	}
	settings := cfg.ImageStorage
	settings.Normalize()
	if errValidate := settings.Validate(); errValidate != nil {
		return errValidate
	}
	client, _, errClient := newCOSClient(settings)
	if errClient != nil {
		return errClient
	}
	prefix := settings.COSPathPrefix
	_, response, errGet := client.Bucket.Get(ctx, &cos.BucketGetOptions{Prefix: prefix, MaxKeys: 1})
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if errGet != nil {
		return fmt.Errorf("probe COS bucket: %w", errGet)
	}
	return nil
}

func newCOSClient(settings config.ImageStorageConfig) (*cos.Client, string, error) {
	endpoint := fmt.Sprintf("https://%s.cos.%s.myqcloud.com", settings.COSBucket, settings.COSRegion)
	endpointURL, errParse := url.Parse(endpoint)
	if errParse != nil {
		return nil, "", fmt.Errorf("parse COS endpoint: %w", errParse)
	}
	client := cos.NewClient(&cos.BaseURL{BucketURL: endpointURL}, &http.Client{
		Transport: &cos.AuthorizationTransport{
			SecretID:  settings.COSSecretID,
			SecretKey: settings.COSSecretKey,
		},
	})
	return client, endpoint, nil
}

func imageRelativePath(data []byte, contentType string) string {
	digest := md5.Sum(data)
	extension := extensionForMIME(contentType)
	now := time.Now()
	return fmt.Sprintf("%04d/%02d/%02d/%d_%s.%s", now.Year(), now.Month(), now.Day(), now.Unix(), hex.EncodeToString(digest[:]), extension)
}

func dataURL(data []byte, contentType string) string {
	return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func mimeType(outputFormat string) string {
	switch strings.ToLower(strings.TrimSpace(outputFormat)) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func extensionForMIME(contentType string) string {
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/webp":
		return "webp"
	default:
		return "png"
	}
}
