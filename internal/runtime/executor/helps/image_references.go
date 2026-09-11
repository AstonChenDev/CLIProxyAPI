package helps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	maxInputImageReferenceBytes = 20 << 20
	maxInputImageTotalBytes     = 50 << 20
)

// ResolveResponsesInputImages converts URL and base64 image references in a
// Responses request into validated data URLs. The Codex Responses backend only
// accepts inline image data for image editing, even though the public compatible
// endpoint also accepts remote image URLs.
func ResolveResponsesInputImages(ctx context.Context, cfg *config.Config, payload []byte) ([]byte, error) {
	if !json.Valid(payload) {
		return nil, fmt.Errorf("invalid Responses request JSON")
	}

	out := payload
	totalBytes := 0
	for inputIndex, input := range gjson.GetBytes(payload, "input").Array() {
		for contentIndex, content := range input.Get("content").Array() {
			if content.Get("type").String() != "input_image" {
				continue
			}
			reference := strings.TrimSpace(content.Get("image_url").String())
			if reference == "" {
				continue
			}

			dataURL, imageBytes, errResolve := resolveInputImageReference(ctx, cfg, reference)
			if errResolve != nil {
				return nil, errResolve
			}
			totalBytes += imageBytes
			if totalBytes > maxInputImageTotalBytes {
				return nil, fmt.Errorf("input images exceed the 50MB total limit")
			}

			path := fmt.Sprintf("input.%d.content.%d.image_url", inputIndex, contentIndex)
			out, _ = sjson.SetBytes(out, path, dataURL)
		}
	}
	return out, nil
}

// RedactResponsesInputImages removes inline image data before request logging.
// This prevents large base64 payloads and user-supplied images from being kept
// in deferred request logs while preserving the rest of the request structure.
func RedactResponsesInputImages(payload []byte) []byte {
	if !json.Valid(payload) {
		return payload
	}
	out := payload
	for inputIndex, input := range gjson.GetBytes(payload, "input").Array() {
		for contentIndex, content := range input.Get("content").Array() {
			if content.Get("type").String() != "input_image" || !content.Get("image_url").Exists() {
				continue
			}
			path := fmt.Sprintf("input.%d.content.%d.image_url", inputIndex, contentIndex)
			out, _ = sjson.SetBytes(out, path, "[REDACTED_IMAGE_DATA]")
		}
	}
	return out
}

func resolveInputImageReference(ctx context.Context, cfg *config.Config, reference string) (string, int, error) {
	reference = strings.TrimSpace(reference)
	switch {
	case strings.HasPrefix(strings.ToLower(reference), "data:"):
		data, errDecode := decodeInputImageDataURL(reference)
		if errDecode != nil {
			return "", 0, errDecode
		}
		return inputImageDataURL(data)
	case strings.HasPrefix(strings.ToLower(reference), "http://"), strings.HasPrefix(strings.ToLower(reference), "https://"):
		data, errDownload := downloadInputImageReference(ctx, cfg, reference)
		if errDownload != nil {
			return "", 0, errDownload
		}
		return inputImageDataURL(data)
	default:
		data, errDecode := base64.StdEncoding.DecodeString(reference)
		if errDecode != nil {
			return "", 0, fmt.Errorf("input image is not valid base64")
		}
		return inputImageDataURL(data)
	}
}

func decodeInputImageDataURL(reference string) ([]byte, error) {
	header, encoded, found := strings.Cut(reference, ",")
	if !found || !strings.Contains(strings.ToLower(header), ";base64") {
		return nil, fmt.Errorf("input image data URL must be base64 encoded")
	}
	if len(encoded) > base64.StdEncoding.EncodedLen(maxInputImageReferenceBytes)+4 {
		return nil, fmt.Errorf("input image exceeds the 20MB limit")
	}
	data, errDecode := base64.StdEncoding.DecodeString(encoded)
	if errDecode != nil {
		return nil, fmt.Errorf("input image data URL contains invalid base64")
	}
	return data, nil
}

func downloadInputImageReference(ctx context.Context, cfg *config.Config, reference string) ([]byte, error) {
	parsed, errParse := url.Parse(reference)
	if errParse != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("input image URL must use http or https")
	}

	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, reference, nil)
	if errRequest != nil {
		return nil, fmt.Errorf("create input image request failed: %w", errRequest)
	}
	req.Header.Set("Accept", "image/*,*/*;q=0.8")
	req.Header.Set("User-Agent", "CLIProxyAPI image fetcher")

	// Reference images are client inputs, not provider traffic. Do not inherit
	// an account-specific upstream proxy, which may only permit OpenAI hosts and
	// would make image downloads fail depending on the selected account.
	resp, errDo := NewProxyAwareHTTPClient(ctx, cfg, nil, 0).Do(req)
	if errDo != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("fetch input image failed")
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("close input image response body failed: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch input image failed with HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxInputImageReferenceBytes {
		return nil, fmt.Errorf("input image exceeds the 20MB limit")
	}

	data, errRead := io.ReadAll(io.LimitReader(resp.Body, maxInputImageReferenceBytes+1))
	if errRead != nil {
		return nil, fmt.Errorf("read input image failed: %w", errRead)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("input image is empty")
	}
	if len(data) > maxInputImageReferenceBytes {
		return nil, fmt.Errorf("input image exceeds the 20MB limit")
	}
	return data, nil
}

func inputImageDataURL(data []byte) (string, int, error) {
	if len(data) == 0 {
		return "", 0, fmt.Errorf("input image is empty")
	}
	if len(data) > maxInputImageReferenceBytes {
		return "", 0, fmt.Errorf("input image exceeds the 20MB limit")
	}
	mediaType := http.DetectContentType(data)
	if !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		return "", 0, fmt.Errorf("input image reference does not contain an image")
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data), len(data), nil
}
