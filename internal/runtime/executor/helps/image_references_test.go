package helps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResolveResponsesInputImagesConvertsRemoteImageToDataURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'})
	}))
	defer server.Close()

	payload := []byte(`{"input":[{"role":"user","content":[{"type":"input_text","text":"keep me"},{"type":"input_image","image_url":"` + server.URL + `/reference.png"}]}]}`)
	out, errResolve := ResolveResponsesInputImages(context.Background(), nil, nil, payload)
	if errResolve != nil {
		t.Fatalf("ResolveResponsesInputImages() error = %v", errResolve)
	}
	if got := gjson.GetBytes(out, "input.0.content.0.text").String(); got != "keep me" {
		t.Fatalf("text = %q, want keep me", got)
	}
	if got := gjson.GetBytes(out, "input.0.content.1.image_url").String(); !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("image URL = %q, want PNG data URL", got)
	}
}

func TestResolveResponsesInputImagesRejectsNonImageResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("not an image"))
	}))
	defer server.Close()

	payload := []byte(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"` + server.URL + `/reference.txt"}]}]}`)
	if _, errResolve := ResolveResponsesInputImages(context.Background(), nil, nil, payload); errResolve == nil || !strings.Contains(errResolve.Error(), "does not contain an image") {
		t.Fatalf("ResolveResponsesInputImages() error = %v, want non-image rejection", errResolve)
	}
}

func TestResolveResponsesInputImagesAcceptsChatGPT2APIBase64(t *testing.T) {
	payload := []byte(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"iVBORw0KGgo="}]}]}`)
	out, errResolve := ResolveResponsesInputImages(context.Background(), nil, nil, payload)
	if errResolve != nil {
		t.Fatalf("ResolveResponsesInputImages() error = %v", errResolve)
	}
	if got := gjson.GetBytes(out, "input.0.content.0.image_url").String(); !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("image URL = %q, want PNG data URL", got)
	}
}

func TestRedactResponsesInputImagesRemovesInlineImageData(t *testing.T) {
	payload := []byte(`{"input":[{"role":"user","content":[{"type":"input_text","text":"keep me"},{"type":"input_image","image_url":"data:image/png;base64,secret-image"}]}]}`)
	out := RedactResponsesInputImages(payload)
	if strings.Contains(string(out), "secret-image") {
		t.Fatalf("redacted payload still contains image data: %s", string(out))
	}
	if got := gjson.GetBytes(out, "input.0.content.0.text").String(); got != "keep me" {
		t.Fatalf("text = %q, want keep me", got)
	}
	if got := gjson.GetBytes(out, "input.0.content.1.image_url").String(); got != "[REDACTED_IMAGE_DATA]" {
		t.Fatalf("image URL = %q, want redaction marker", got)
	}
}
