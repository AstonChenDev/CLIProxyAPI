package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/imagestorage"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexOpenAIImageSourceFormat = "openai-image"
	codexImagesGenerationsPath   = "/v1/images/generations"
	codexImagesEditsPath         = "/v1/images/edits"
	codexDirectImagesGenerations = "/images/generations"
	codexDirectImagesEdit        = "/images/edits"
	codexGPTImage15Model         = "gpt-image-1.5"
	codexGPTImage25FlareModel    = "gpt-image-2.5-flare"
	codexGPTImage25SunburstModel = "gpt-image-2.5-sunburst"
	codexOpenAIImagesMainModel   = "gpt-5.5"
	codexOpenAIImagesInstruction = "Use the image_generation tool to create exactly one image for the user request. Return the generated image result."
)

type codexOpenAIImagePreparedRequest struct {
	Body           []byte
	ResponseFormat string
	StreamPrefix   string
}

type codexImageCallResult struct {
	Result        string
	RevisedPrompt string
	OutputFormat  string
	Size          string
	Background    string
	Quality       string
}

func isCodexOpenAIImageRequest(opts cliproxyexecutor.Options) bool {
	if !strings.EqualFold(strings.TrimSpace(opts.SourceFormat.String()), codexOpenAIImageSourceFormat) {
		return false
	}
	return codexIsImagesEndpointPath(helps.PayloadRequestPath(opts))
}

func codexIsImagesEndpointPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == codexImagesGenerationsPath || path == codexImagesEditsPath {
		return true
	}
	return strings.HasSuffix(path, codexImagesGenerationsPath) || strings.HasSuffix(path, codexImagesEditsPath)
}

func (e *CodexExecutor) resolveGPTImage2BaseModel() string {
	if e == nil || e.cfg == nil {
		return codexOpenAIImagesMainModel
	}
	model := strings.TrimSpace(e.cfg.GPTImage2BaseModel)
	if model == "" {
		return codexOpenAIImagesMainModel
	}
	if strings.HasPrefix(strings.ToLower(model), "gpt-") {
		return model
	}
	return codexOpenAIImagesMainModel
}

func (e *CodexExecutor) executeOpenAIImage(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if directEndpoint := codexDirectOpenAIImageEndpoint(req, opts); directEndpoint != "" {
		return e.executeDirectOpenAIImage(ctx, auth, req, opts, directEndpoint)
	}

	prepared, errPrepare := codexPrepareOpenAIImageRequest(req, opts)
	if errPrepare != nil {
		return resp, errPrepare
	}
	prepared.Body, errPrepare = helps.ResolveResponsesInputImages(ctx, e.cfg, auth, prepared.Body)
	if errPrepare != nil {
		return resp, statusErr{code: http.StatusBadRequest, msg: errPrepare.Error()}
	}

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	mainModel := e.resolveGPTImage2BaseModel()
	reporter := helps.NewExecutorUsageReporter(ctx, e, mainModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	body, errBuild := e.prepareCodexOpenAIImageBody(prepared.Body, req, opts, mainModel)
	if errBuild != nil {
		return resp, errBuild
	}
	reporter.SetTranslatedReasoningEffort(body, "codex")

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	var identityState codexIdentityConfuseState
	httpReq, body, identityState, errCache := e.cacheHelper(ctx, sdktranslator.FromString(codexOpenAIImageSourceFormat), url, auth, req, req.Payload, body)
	if errCache != nil {
		return resp, errCache
	}
	applyCodexHeaders(httpReq, auth, apiKey, true, e.cfg, opts.Headers)
	applyModelHeaderOverrides(httpReq.Header, mainModel)
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
	recordCodexOpenAIImageRequest(ctx, e.cfg, e.Identifier(), auth, url, httpReq.Header.Clone(), body)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return resp, errDo
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
	}()

	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	data, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		return resp, errRead
	}
	data = applyCodexIdentityConfuseResponsePayload(data, identityState)
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		err = newCodexStatusErr(httpResp.StatusCode, data)
		return resp, err
	}

	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		if !bytes.HasPrefix(line, dataTag) {
			continue
		}
		eventData := bytes.TrimSpace(line[len(dataTag):])
		switch gjson.GetBytes(eventData, "type").String() {
		case "response.output_item.done":
			collectCodexOutputItemDone(eventData, outputItemsByIndex, &outputItemsFallback)
		case "response.completed":
			if detail, ok := helps.ParseCodexUsage(eventData); ok {
				reporter.Publish(ctx, detail)
			}
			publishCodexImageToolUsage(ctx, reporter, body, eventData)
			results, createdAt, usageRaw, firstMeta, errExtract := codexExtractImageResults(eventData, outputItemsByIndex, outputItemsFallback)
			if errExtract != nil {
				return resp, errExtract
			}
			if len(results) == 0 {
				return resp, statusErr{code: http.StatusBadGateway, msg: "upstream did not return image output"}
			}
			out, errOutput := codexBuildImagesAPIResponse(results, createdAt, usageRaw, firstMeta, prepared.ResponseFormat)
			if errOutput != nil {
				return resp, errOutput
			}
			out, errOutput = imagestorage.RewriteOpenAIResponse(ctx, e.cfg, out, prepared.ResponseFormat)
			if errOutput != nil {
				return resp, errOutput
			}
			return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
		}
	}

	err = statusErr{code: http.StatusGatewayTimeout, msg: "stream error: stream disconnected before completion"}
	return resp, err
}

func (e *CodexExecutor) executeOpenAIImageStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if directEndpoint := codexDirectOpenAIImageEndpoint(req, opts); directEndpoint != "" {
		return e.executeDirectOpenAIImageStream(ctx, auth, req, opts, directEndpoint)
	}

	prepared, errPrepare := codexPrepareOpenAIImageRequest(req, opts)
	if errPrepare != nil {
		return nil, errPrepare
	}
	prepared.Body, errPrepare = helps.ResolveResponsesInputImages(ctx, e.cfg, auth, prepared.Body)
	if errPrepare != nil {
		return nil, statusErr{code: http.StatusBadRequest, msg: errPrepare.Error()}
	}

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	mainModel := e.resolveGPTImage2BaseModel()
	reporter := helps.NewExecutorUsageReporter(ctx, e, mainModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	body, errBuild := e.prepareCodexOpenAIImageBody(prepared.Body, req, opts, mainModel)
	if errBuild != nil {
		return nil, errBuild
	}
	reporter.SetTranslatedReasoningEffort(body, "codex")

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	var identityState codexIdentityConfuseState
	httpReq, body, identityState, errCache := e.cacheHelper(ctx, sdktranslator.FromString(codexOpenAIImageSourceFormat), url, auth, req, req.Payload, body)
	if errCache != nil {
		return nil, errCache
	}
	applyCodexHeaders(httpReq, auth, apiKey, true, e.cfg, opts.Headers)
	applyModelHeaderOverrides(httpReq.Header, mainModel)
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
	recordCodexOpenAIImageRequest(ctx, e.cfg, e.Identifier(), auth, url, httpReq.Header.Clone(), body)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return nil, errDo
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}
		data = applyCodexIdentityConfuseResponsePayload(data, identityState)
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		err = newCodexStatusErr(httpResp.StatusCode, data)
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
		}()

		sendPayload := func(payload []byte) bool {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: payload}:
				return true
			case <-ctx.Done():
				return false
			}
		}
		sendError := func(errSend error) bool {
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errSend}:
				return true
			case <-ctx.Done():
				return false
			}
		}

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB
		outputItemsByIndex := make(map[int64][]byte)
		var outputItemsFallback [][]byte
		for scanner.Scan() {
			line := applyCodexIdentityConfuseResponsePayload(scanner.Bytes(), identityState)
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if !bytes.HasPrefix(line, dataTag) {
				continue
			}
			eventData := bytes.TrimSpace(line[len(dataTag):])
			switch gjson.GetBytes(eventData, "type").String() {
			case "response.output_item.done":
				collectCodexOutputItemDone(eventData, outputItemsByIndex, &outputItemsFallback)
			case "response.image_generation_call.partial_image":
				frame := codexBuildImagePartialFrame(eventData, prepared.ResponseFormat, prepared.StreamPrefix)
				if len(frame) > 0 && !sendPayload(frame) {
					return
				}
			case "response.completed":
				if detail, ok := helps.ParseCodexUsage(eventData); ok {
					reporter.Publish(ctx, detail)
				}
				publishCodexImageToolUsage(ctx, reporter, body, eventData)
				results, _, usageRaw, _, errExtract := codexExtractImageResults(eventData, outputItemsByIndex, outputItemsFallback)
				if errExtract != nil {
					sendError(errExtract)
					return
				}
				if len(results) == 0 {
					sendError(statusErr{code: http.StatusBadGateway, msg: "upstream did not return image output"})
					return
				}
				for _, img := range results {
					frame := codexBuildImageCompletedFrame(ctx, e.cfg, img, usageRaw, prepared.ResponseFormat, prepared.StreamPrefix)
					if len(frame) > 0 && !sendPayload(frame) {
						return
					}
				}
				return
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			sendError(errScan)
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *CodexExecutor) executeDirectOpenAIImage(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointPath string) (resp cliproxyexecutor.Response, err error) {
	body, contentType, model, errPrepare := codexPrepareDirectOpenAIImageBody(req, opts, false)
	if errPrepare != nil {
		return resp, errPrepare
	}

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, model, auth)
	defer reporter.TrackFailure(ctx, &err)
	reporter.SetTranslatedReasoningEffort(body, "openai")

	url := strings.TrimSuffix(baseURL, "/") + endpointPath
	var identityState codexIdentityConfuseState
	httpReq, body, identityState, errCache := e.cacheHelper(ctx, sdktranslator.FromString(codexOpenAIImageSourceFormat), url, auth, req, req.Payload, body)
	if errCache != nil {
		return resp, errCache
	}
	applyCodexDirectImageHeaders(httpReq, auth, apiKey, false, e.cfg)
	applyModelHeaderOverrides(httpReq.Header, model)
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
	recordCodexOpenAIImageRequest(ctx, e.cfg, e.Identifier(), auth, url, httpReq.Header.Clone(), body)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return resp, errDo
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
	}()

	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	data, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		return resp, errRead
	}
	data = applyCodexIdentityConfuseResponsePayload(data, identityState)
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		err = newCodexStatusErr(httpResp.StatusCode, data)
		return resp, err
	}

	reporter.Publish(ctx, helps.ParseOpenAIUsage(data))
	reporter.EnsurePublished(ctx)
	data, err = imagestorage.RewriteOpenAIResponse(ctx, e.cfg, data, codexOpenAIImageResponseFormatFromJSON(body))
	if err != nil {
		return resp, err
	}
	return cliproxyexecutor.Response{Payload: data, Headers: httpResp.Header.Clone()}, nil
}

func (e *CodexExecutor) executeDirectOpenAIImageStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointPath string) (_ *cliproxyexecutor.StreamResult, err error) {
	body, contentType, model, errPrepare := codexPrepareDirectOpenAIImageBody(req, opts, true)
	if errPrepare != nil {
		return nil, errPrepare
	}

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, model, auth)
	defer reporter.TrackFailure(ctx, &err)
	reporter.SetTranslatedReasoningEffort(body, "openai")

	url := strings.TrimSuffix(baseURL, "/") + endpointPath
	var identityState codexIdentityConfuseState
	httpReq, body, identityState, errCache := e.cacheHelper(ctx, sdktranslator.FromString(codexOpenAIImageSourceFormat), url, auth, req, req.Payload, body)
	if errCache != nil {
		return nil, errCache
	}
	applyCodexDirectImageHeaders(httpReq, auth, apiKey, true, e.cfg)
	applyModelHeaderOverrides(httpReq.Header, model)
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
	recordCodexOpenAIImageRequest(ctx, e.cfg, e.Identifier(), auth, url, httpReq.Header.Clone(), body)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return nil, errDo
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}
		data = applyCodexIdentityConfuseResponsePayload(data, identityState)
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		err = newCodexStatusErr(httpResp.StatusCode, data)
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		var streamUsage helps.StreamUsageBuffer
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
			streamUsage.Publish(ctx, reporter)
			reporter.EnsurePublished(ctx)
		}()

		buffer := make([]byte, 32*1024)
		for {
			n, errRead := httpResp.Body.Read(buffer)
			if n > 0 {
				chunk := bytes.Clone(buffer[:n])
				chunk = applyCodexIdentityConfuseResponsePayload(chunk, identityState)
				helps.AppendAPIResponseChunk(ctx, e.cfg, chunk)
				for _, line := range bytes.Split(chunk, []byte("\n")) {
					streamUsage.ObserveOpenAIStream(bytes.TrimSpace(line))
				}
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
				case <-ctx.Done():
					return
				}
			}
			if errRead != nil {
				if errRead != io.EOF {
					helps.RecordAPIResponseError(ctx, e.cfg, errRead)
					reporter.PublishFailure(ctx, errRead)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: errRead}:
					case <-ctx.Done():
					}
				}
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func codexDirectOpenAIImageEndpoint(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	if codexDirectOpenAIImageModel(req) == "" {
		return ""
	}
	path := helps.PayloadRequestPath(opts)
	if strings.HasSuffix(strings.TrimSpace(path), codexImagesGenerationsPath) {
		// The Image API generations endpoint creates images from text only. Keep
		// the public generations route for chatgpt2api compatibility, but route
		// requests containing reference images through Responses so the images
		// are delivered to the upstream model as input_image content parts.
		if json.Valid(req.Payload) && len(codexCollectOpenAIImageReferences(req.Payload)) > 0 {
			return ""
		}
		return codexDirectImagesGenerations
	}
	if strings.HasSuffix(strings.TrimSpace(path), codexImagesEditsPath) {
		return codexDirectImagesEdit
	}
	return ""
}

func codexPrepareDirectOpenAIImageBody(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) ([]byte, string, string, error) {
	model := codexDirectOpenAIImageModel(req)
	if model == "" {
		return nil, "", "", fmt.Errorf("unsupported direct OpenAI image model %q", req.Model)
	}
	body, contentType, errPrepare := codexPrepareDirectOpenAIImagePayload(req, opts, model, stream)
	if errPrepare != nil {
		return nil, "", "", errPrepare
	}
	return body, contentType, model, nil
}

func codexPrepareDirectOpenAIImagePayload(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, model string, stream bool) ([]byte, string, error) {
	contentType := opts.Headers.Get("Content-Type")
	path := strings.TrimSpace(helps.PayloadRequestPath(opts))
	if strings.HasSuffix(path, codexImagesEditsPath) {
		return codexPrepareDirectOpenAIImageEditPayload(req.Payload, model, contentType, stream)
	}
	return prepareOpenAICompatImagesPayload(req.Payload, model, contentType, stream)
}

func codexPrepareDirectOpenAIImageEditPayload(payload []byte, model string, contentType string, stream bool) ([]byte, string, error) {
	if json.Valid(payload) {
		payload = normalizeCodexImageEditJSONPayload(payload)
		return prepareOpenAICompatImagesPayload(payload, model, contentType, stream)
	}

	mediaType, params, errParse := mime.ParseMediaType(strings.TrimSpace(contentType))
	if errParse != nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mediaType)), "multipart/") {
		return nil, "", fmt.Errorf("unsupported OpenAI image edit Content-Type %q", contentType)
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return nil, "", fmt.Errorf("multipart boundary is missing")
	}
	return codexRewriteOpenAIImageEditMultipartToJSON(payload, model, boundary, stream)
}

// normalizeCodexImageEditJSONPayload accepts the same image reference aliases
// as chatgpt2api while retaining the native images[].image_url shape expected by
// Codex's direct image edit endpoint. Existing object references (including
// file_id values used by legacy callers) are preserved unchanged.
func normalizeCodexImageEditJSONPayload(payload []byte) []byte {
	if !json.Valid(payload) {
		return payload
	}
	images := make([][]byte, 0)
	var appendReference func(gjson.Result)
	appendReference = func(value gjson.Result) {
		if !value.Exists() {
			return
		}
		if value.IsArray() {
			for _, item := range value.Array() {
				appendReference(item)
			}
			return
		}
		if value.IsObject() {
			if imageURL := value.Get("image_url"); imageURL.Exists() {
				if imageURL.IsObject() {
					if ref := strings.TrimSpace(imageURL.Get("url").String()); ref != "" {
						item := []byte(`{"image_url":""}`)
						item, _ = sjson.SetBytes(item, "image_url", ref)
						images = append(images, item)
						return
					}
				}
				if ref := strings.TrimSpace(imageURL.String()); ref != "" {
					item := []byte(`{"image_url":""}`)
					item, _ = sjson.SetBytes(item, "image_url", ref)
					images = append(images, item)
					return
				}
			}
			if value.Get("file_id").Exists() || value.Get("b64_json").Exists() {
				images = append(images, []byte(value.Raw))
				return
			}
			if value.Get("url").Exists() {
				item := []byte(`{"image_url":""}`)
				item, _ = sjson.SetBytes(item, "image_url", value.Get("url").String())
				images = append(images, item)
			}
			return
		}
		ref := strings.TrimSpace(value.String())
		if ref == "" {
			return
		}
		item := []byte(`{"image_url":""}`)
		item, _ = sjson.SetBytes(item, "image_url", ref)
		images = append(images, item)
	}

	if existing := gjson.GetBytes(payload, "images"); existing.IsArray() {
		for _, item := range existing.Array() {
			appendReference(item)
		}
	} else {
		appendReference(gjson.GetBytes(payload, "images"))
		appendReference(gjson.GetBytes(payload, "image"))
		appendReference(gjson.GetBytes(payload, "image_url"))
	}
	if len(images) > 0 {
		payload, _ = sjson.SetRawBytes(payload, "images", helps.JoinRawJSONArray(images))
	}

	mask := gjson.GetBytes(payload, "mask")
	if mask.Exists() && mask.Type == gjson.String && strings.TrimSpace(mask.String()) != "" {
		payload, _ = sjson.SetBytes(payload, "mask.image_url", strings.TrimSpace(mask.String()))
	}
	return payload
}

func codexRewriteOpenAIImageEditMultipartToJSON(payload []byte, model string, boundary string, stream bool) ([]byte, string, error) {
	reader := multipart.NewReader(bytes.NewReader(payload), boundary)
	form, errRead := reader.ReadForm(openAICompatMultipartMemory)
	if errRead != nil {
		return nil, "", fmt.Errorf("read multipart form failed: %w", errRead)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			log.Errorf("codex openai images: remove multipart form files error: %v", errRemove)
		}
	}()

	out := []byte(`{}`)
	out, _ = sjson.SetBytes(out, "model", model)
	if stream {
		out, _ = sjson.SetBytes(out, "stream", true)
	}

	formImageRefs := make([]string, 0)
	formImageRawRefs := make([]string, 0)
	for key, values := range form.Value {
		key = strings.TrimSpace(key)
		if key == "" || key == "model" || key == "stream" {
			continue
		}
		if codexIsImageReferenceField(key) {
			for _, value := range values {
				if key == "images" && !strings.HasPrefix(strings.TrimSpace(value), "[") && !strings.HasPrefix(strings.TrimSpace(value), "{") {
					if strings.TrimSpace(value) != "" {
						formImageRawRefs = append(formImageRawRefs, strings.TrimSpace(value))
					}
					continue
				}
				formImageRefs = append(formImageRefs, codexExpandImageReferenceValue(value)...)
			}
			continue
		}
		if key == "mask" || key == "mask[]" {
			if len(values) > 0 && strings.TrimSpace(values[0]) != "" {
				out, _ = sjson.SetBytes(out, "mask.image_url", strings.TrimSpace(values[0]))
			}
			continue
		}
		out = codexSetOpenAIImageEditFormValues(out, key, values)
	}

	if maskFiles := form.File["mask"]; len(maskFiles) > 0 && maskFiles[0] != nil {
		dataURL, errData := codexMultipartFileToDataURL(maskFiles[0])
		if errData != nil {
			return nil, "", errData
		}
		out, _ = sjson.SetBytes(out, "mask.image_url", dataURL)
	}

	imageFiles := codexMultipartImageFiles(form)
	if existingImages := gjson.GetBytes(out, "images"); !existingImages.Exists() || existingImages.IsArray() {
		existingItems := existingImages.Array()
		imageItems := make([][]byte, 0, len(existingItems)+len(imageFiles)+len(formImageRefs)+len(formImageRawRefs))
		for _, image := range existingItems {
			imageItems = append(imageItems, []byte(image.Raw))
		}
		for _, ref := range formImageRawRefs {
			item, _ := json.Marshal(ref)
			imageItems = append(imageItems, item)
		}
		for _, ref := range formImageRefs {
			item := []byte(`{"image_url":""}`)
			item, _ = sjson.SetBytes(item, "image_url", ref)
			imageItems = append(imageItems, item)
		}
		for _, fileHeader := range imageFiles {
			dataURL, errData := codexMultipartFileToDataURL(fileHeader)
			if errData != nil {
				return nil, "", errData
			}
			item := []byte(`{"image_url":""}`)
			item, _ = sjson.SetBytes(item, "image_url", dataURL)
			imageItems = append(imageItems, item)
		}
		if len(imageFiles) > 0 || len(formImageRefs) > 0 || len(formImageRawRefs) > 0 {
			out, _ = sjson.SetRawBytes(out, "images", helps.JoinRawJSONArray(imageItems))
		}
	} else {
		for _, ref := range formImageRefs {
			out, _ = sjson.SetBytes(out, "images.-1.image_url", ref)
		}
		for _, fileHeader := range imageFiles {
			dataURL, errData := codexMultipartFileToDataURL(fileHeader)
			if errData != nil {
				return nil, "", errData
			}
			out, _ = sjson.SetBytes(out, "images.-1.image_url", dataURL)
		}
	}

	return out, "application/json", nil
}

func codexIsImageReferenceField(key string) bool {
	switch strings.TrimSpace(key) {
	case "image", "image[]", "images", "images[]", "image_url", "image_url[]":
		return true
	default:
		return false
	}
}

func codexExpandImageReferenceValue(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if strings.HasPrefix(value, "[") && json.Valid([]byte(value)) {
		var values []string
		if json.Unmarshal([]byte(value), &values) == nil {
			return values
		}
		var objects []map[string]any
		if json.Unmarshal([]byte(value), &objects) == nil {
			refs := make([]string, 0, len(objects))
			for _, item := range objects {
				if ref, ok := item["image_url"].(string); ok && strings.TrimSpace(ref) != "" {
					refs = append(refs, strings.TrimSpace(ref))
				} else if ref, ok := item["url"].(string); ok && strings.TrimSpace(ref) != "" {
					refs = append(refs, strings.TrimSpace(ref))
				}
			}
			return refs
		}
	}
	if strings.HasPrefix(value, "{") && json.Valid([]byte(value)) {
		var object map[string]any
		if json.Unmarshal([]byte(value), &object) == nil {
			if ref, ok := object["image_url"].(string); ok && strings.TrimSpace(ref) != "" {
				return []string{strings.TrimSpace(ref)}
			}
			if ref, ok := object["url"].(string); ok && strings.TrimSpace(ref) != "" {
				return []string{strings.TrimSpace(ref)}
			}
		}
	}
	return []string{value}
}

func codexSetOpenAIImageEditFormValues(out []byte, key string, values []string) []byte {
	if len(values) == 0 {
		return out
	}
	path := codexOpenAIImageEditFormJSONPath(key)
	if path == "" {
		return out
	}
	if len(values) == 1 {
		return codexSetOpenAIImageEditFormValue(out, path, values[0])
	}
	items := make([][]byte, 0, len(values))
	for _, value := range values {
		items = append(items, codexOpenAIImageEditFormJSONValue(key, value))
	}
	out, _ = sjson.SetRawBytes(out, path, helps.JoinRawJSONArray(items))
	return out
}

func codexSetOpenAIImageEditFormValue(out []byte, path string, value string) []byte {
	item := codexOpenAIImageEditFormJSONValue(path, value)
	out, _ = sjson.SetRawBytes(out, path, item)
	return out
}

func codexOpenAIImageEditFormJSONValue(key string, value string) []byte {
	value = strings.TrimSpace(value)
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "n", "output_compression", "partial_images":
		if parsed, errParse := strconv.ParseInt(value, 10, 64); errParse == nil {
			raw, _ := json.Marshal(parsed)
			return raw
		}
	}
	raw, _ := json.Marshal(value)
	return raw
}

func codexOpenAIImageEditFormJSONPath(key string) string {
	key = strings.TrimSpace(key)
	switch key {
	case "mask[file_id]":
		return "mask.file_id"
	case "mask[image_url]":
		return "mask.image_url"
	default:
		return key
	}
}

func codexDirectOpenAIImageModel(req cliproxyexecutor.Request) string {
	for _, model := range []string{gjson.GetBytes(req.Payload, "model").String(), req.Model} {
		baseModel := codexOpenAIImageBaseModel(model)
		if codexIsDirectOpenAIImageModel(baseModel) {
			return baseModel
		}
	}
	return ""
}

func codexOpenAIImageBaseModel(model string) string {
	model = strings.TrimSpace(thinking.ParseSuffix(model).ModelName)
	if idx := strings.LastIndex(model, "/"); idx >= 0 && idx < len(model)-1 {
		model = strings.TrimSpace(model[idx+1:])
	}
	return strings.ToLower(strings.TrimSpace(model))
}

func codexIsDirectOpenAIImageModel(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case codexGPTImage15Model, codexDefaultImageToolModel, codexGPTImage25FlareModel, codexGPTImage25SunburstModel:
		return true
	default:
		return false
	}
}

func (e *CodexExecutor) prepareCodexOpenAIImageBody(body []byte, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, mainModel string) ([]byte, error) {
	out := body
	mainModel = strings.TrimSpace(mainModel)
	if mainModel == "" {
		mainModel = codexOpenAIImagesMainModel
	}
	var errThinking error
	out, errThinking = helps.ApplyThinkingWithSourcePayload(out, body, body, mainModel, codexOpenAIImageSourceFormat, "codex", e.Identifier())
	if errThinking != nil {
		return nil, errThinking
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	out = helps.ApplyPayloadConfigWithRequest(e.cfg, mainModel, "codex", codexOpenAIImageSourceFormat, "", out, body, requestedModel, requestPath, opts.Headers)
	out = helps.SetStringIfDifferent(out, "model", mainModel)
	out = helps.SetBoolIfDifferent(out, "stream", true)
	out, _ = sjson.DeleteBytes(out, "previous_response_id")
	out, _ = sjson.DeleteBytes(out, "prompt_cache_retention")
	out, _ = sjson.DeleteBytes(out, "safety_identifier")
	out, _ = sjson.DeleteBytes(out, "stream_options")
	return normalizeCodexInstructions(out), nil
}

func recordCodexOpenAIImageRequest(ctx context.Context, cfg *config.Config, provider string, auth *cliproxyauth.Auth, url string, headers http.Header, body []byte) {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   headers,
		Body:      helps.RedactResponsesInputImages(body),
		Provider:  provider,
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
}

func codexPrepareOpenAIImageRequest(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (codexOpenAIImagePreparedRequest, error) {
	path := helps.PayloadRequestPath(opts)
	if strings.HasSuffix(path, codexImagesGenerationsPath) {
		return codexPrepareOpenAIImageGenerationJSON(req.Payload, req.Model)
	}
	if !strings.HasSuffix(path, codexImagesEditsPath) {
		return codexOpenAIImagePreparedRequest{}, fmt.Errorf("unsupported OpenAI image endpoint path %q", path)
	}

	contentType := codexImageContentType(opts.Headers)
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		return codexPrepareOpenAIImageEditMultipart(req.Payload, req.Model, contentType)
	}
	return codexPrepareOpenAIImageEditJSON(req.Payload, req.Model)
}

func codexPrepareOpenAIImageGenerationJSON(rawJSON []byte, routeModel string) (codexOpenAIImagePreparedRequest, error) {
	if !json.Valid(rawJSON) {
		return codexOpenAIImagePreparedRequest{}, fmt.Errorf("invalid OpenAI image generation request JSON")
	}
	prompt := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String())
	images := codexCollectOpenAIImageReferences(rawJSON)
	action := "generate"
	if len(images) > 0 {
		action = "edit"
	}
	tool := codexBuildOpenAIImageTool(rawJSON, routeModel, action, []string{"size", "quality", "background", "output_format", "moderation"}, []string{"output_compression", "partial_images"})
	body := codexBuildImagesResponsesRequest(prompt, images, tool)
	return codexOpenAIImagePreparedRequest{
		Body:           body,
		ResponseFormat: codexOpenAIImageResponseFormatFromJSON(rawJSON),
		StreamPrefix:   "image_generation",
	}, nil
}

func codexCollectOpenAIImageReferences(rawJSON []byte) []string {
	refs := make([]string, 0, 4)
	var appendValue func(gjson.Result)
	appendValue = func(value gjson.Result) {
		if !value.Exists() {
			return
		}
		if value.IsArray() {
			for _, item := range value.Array() {
				appendValue(item)
			}
			return
		}
		if value.IsObject() {
			if imageURL := strings.TrimSpace(value.Get("image_url.url").String()); imageURL != "" {
				refs = append(refs, imageURL)
				return
			}
			if imageURL := strings.TrimSpace(value.Get("image_url").String()); imageURL != "" {
				refs = append(refs, imageURL)
				return
			}
			if imageURL := strings.TrimSpace(value.Get("url").String()); imageURL != "" {
				refs = append(refs, imageURL)
				return
			}
			if encoded := strings.TrimSpace(value.Get("b64_json").String()); encoded != "" {
				refs = append(refs, encoded)
			}
			return
		}
		if ref := strings.TrimSpace(value.String()); ref != "" {
			refs = append(refs, ref)
		}
	}
	for _, key := range []string{"image", "images", "image_url"} {
		appendValue(gjson.GetBytes(rawJSON, key))
	}
	if len(refs) > 4 {
		return refs[:4]
	}
	return refs
}

func codexPrepareOpenAIImageEditJSON(rawJSON []byte, routeModel string) (codexOpenAIImagePreparedRequest, error) {
	if !json.Valid(rawJSON) {
		return codexOpenAIImagePreparedRequest{}, fmt.Errorf("invalid OpenAI image edit request JSON")
	}
	prompt := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String())
	images := codexCollectOpenAIImageReferences(rawJSON)
	tool := codexBuildOpenAIImageTool(rawJSON, routeModel, "edit", []string{"size", "quality", "background", "output_format", "input_fidelity", "moderation"}, []string{"output_compression", "partial_images"})
	mask := strings.TrimSpace(gjson.GetBytes(rawJSON, "mask.image_url").String())
	if mask == "" {
		mask = strings.TrimSpace(gjson.GetBytes(rawJSON, "mask.url").String())
	}
	if mask == "" {
		mask = strings.TrimSpace(gjson.GetBytes(rawJSON, "mask").String())
	}
	if mask != "" {
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", mask)
	}
	body := codexBuildImagesResponsesRequest(prompt, images, tool)
	return codexOpenAIImagePreparedRequest{
		Body:           body,
		ResponseFormat: codexOpenAIImageResponseFormatFromJSON(rawJSON),
		StreamPrefix:   "image_edit",
	}, nil
}

func codexPrepareOpenAIImageEditMultipart(rawBody []byte, routeModel string, contentType string) (codexOpenAIImagePreparedRequest, error) {
	_, params, errMedia := mime.ParseMediaType(contentType)
	if errMedia != nil {
		return codexOpenAIImagePreparedRequest{}, fmt.Errorf("parse multipart content type failed: %w", errMedia)
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return codexOpenAIImagePreparedRequest{}, fmt.Errorf("multipart boundary is required")
	}
	reader := multipart.NewReader(bytes.NewReader(rawBody), boundary)
	form, errForm := reader.ReadForm(32 << 20)
	if errForm != nil {
		return codexOpenAIImagePreparedRequest{}, fmt.Errorf("parse multipart form failed: %w", errForm)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			log.Errorf("codex openai images: remove multipart temp files error: %v", errRemove)
		}
	}()

	prompt := strings.TrimSpace(codexFormValue(form, "prompt"))
	responseFormat := codexNormalizeImageResponseFormat(codexFormValue(form, "response_format"))
	tool := []byte(`{"type":"image_generation","action":"edit"}`)
	tool, _ = sjson.SetBytes(tool, "model", codexOpenAIImageToolModel(codexFormValue(form, "model"), routeModel))
	for _, field := range []string{"size", "quality", "background", "output_format", "input_fidelity", "moderation"} {
		if value := strings.TrimSpace(codexFormValue(form, field)); value != "" {
			tool, _ = sjson.SetBytes(tool, field, value)
		}
	}
	for _, field := range []string{"output_compression", "partial_images"} {
		if value := strings.TrimSpace(codexFormValue(form, field)); value != "" {
			if parsed, errParse := strconv.ParseInt(value, 10, 64); errParse == nil {
				tool, _ = sjson.SetBytes(tool, field, parsed)
			}
		}
	}

	images := codexMultipartImageReferenceValues(form)
	for _, fh := range codexMultipartImageFiles(form) {
		dataURL, errData := codexMultipartFileToDataURL(fh)
		if errData != nil {
			return codexOpenAIImagePreparedRequest{}, errData
		}
		images = append(images, dataURL)
	}
	if mask := codexMultipartMaskReferenceValue(form); mask != "" {
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", mask)
	}
	if maskFiles := form.File["mask"]; len(maskFiles) > 0 && maskFiles[0] != nil {
		dataURL, errData := codexMultipartFileToDataURL(maskFiles[0])
		if errData != nil {
			return codexOpenAIImagePreparedRequest{}, errData
		}
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", dataURL)
	}

	body := codexBuildImagesResponsesRequest(prompt, images, tool)
	return codexOpenAIImagePreparedRequest{
		Body:           body,
		ResponseFormat: responseFormat,
		StreamPrefix:   "image_edit",
	}, nil
}

func codexImageContentType(headers http.Header) string {
	if headers == nil {
		return ""
	}
	return strings.TrimSpace(headers.Get("Content-Type"))
}

func codexOpenAIImageResponseFormatFromJSON(rawJSON []byte) string {
	return codexNormalizeImageResponseFormat(gjson.GetBytes(rawJSON, "response_format").String())
}

func codexNormalizeImageResponseFormat(responseFormat string) string {
	if strings.EqualFold(strings.TrimSpace(responseFormat), "b64_json") || strings.TrimSpace(responseFormat) == "" {
		return "b64_json"
	}
	return "url"
}

func codexOpenAIImageToolModel(requestModel string, routeModel string) string {
	model := strings.TrimSpace(requestModel)
	if model == "" {
		model = strings.TrimSpace(routeModel)
	}
	if model == "" {
		model = codexDefaultImageToolModel
	}
	return model
}

func codexBuildOpenAIImageTool(rawJSON []byte, routeModel string, action string, stringFields []string, numberFields []string) []byte {
	tool := []byte(`{"type":"image_generation","action":""}`)
	tool, _ = sjson.SetBytes(tool, "action", action)
	tool, _ = sjson.SetBytes(tool, "model", codexOpenAIImageToolModel(gjson.GetBytes(rawJSON, "model").String(), routeModel))
	for _, field := range stringFields {
		if value := strings.TrimSpace(gjson.GetBytes(rawJSON, field).String()); value != "" {
			tool, _ = sjson.SetBytes(tool, field, value)
		}
	}
	for _, field := range numberFields {
		if value := gjson.GetBytes(rawJSON, field); value.Exists() && value.Type == gjson.Number {
			tool, _ = sjson.SetBytes(tool, field, value.Int())
		}
	}
	return tool
}

func codexBuildImagesResponsesRequest(prompt string, images []string, toolJSON []byte) []byte {
	req := []byte(`{"instructions":"","stream":true,"reasoning":{"effort":"medium","summary":"auto"},"parallel_tool_calls":true,"include":["reasoning.encrypted_content"],"model":"","store":false,"tool_choice":{"type":"image_generation"},"tools":[]}`)
	req, _ = sjson.SetBytes(req, "model", codexOpenAIImagesMainModel)
	req, _ = sjson.SetBytes(req, "instructions", codexOpenAIImagesInstruction)
	if len(toolJSON) > 0 && json.Valid(toolJSON) {
		req, _ = sjson.SetRawBytes(req, "tools", helps.JoinRawJSONArray([][]byte{toolJSON}))
	}

	textPart := []byte(`{"type":"input_text","text":""}`)
	textPart, _ = sjson.SetBytes(textPart, "text", prompt)
	contentItems := make([][]byte, 0, len(images)+1)
	contentItems = append(contentItems, textPart)
	for _, img := range images {
		if strings.TrimSpace(img) == "" {
			continue
		}
		part := []byte(`{"type":"input_image","image_url":""}`)
		part, _ = sjson.SetBytes(part, "image_url", img)
		contentItems = append(contentItems, part)
	}
	inputSize := len(`[{"type":"message","role":"user","content":[]}]`) + len(contentItems)
	for _, item := range contentItems {
		inputSize += len(item)
	}
	input := make([]byte, 0, inputSize)
	input = append(input, `[{"type":"message","role":"user","content":[`...)
	for index, item := range contentItems {
		if index > 0 {
			input = append(input, ',')
		}
		input = append(input, item...)
	}
	input = append(input, ']', '}', ']')
	req, _ = sjson.SetRawBytes(req, "input", input)
	return req
}

func codexFormValue(form *multipart.Form, key string) string {
	if form == nil || len(form.Value[key]) == 0 {
		return ""
	}
	return strings.TrimSpace(form.Value[key][0])
}

func codexMultipartImageFiles(form *multipart.Form) []*multipart.FileHeader {
	if form == nil {
		return nil
	}
	files := make([]*multipart.FileHeader, 0)
	for _, key := range []string{"image", "image[]", "images", "images[]", "image_url", "image_url[]"} {
		files = append(files, form.File[key]...)
	}
	return files
}

func codexMultipartImageReferenceValues(form *multipart.Form) []string {
	if form == nil {
		return nil
	}
	refs := make([]string, 0)
	for _, key := range []string{"image", "image[]", "images", "images[]", "image_url", "image_url[]"} {
		for _, value := range form.Value[key] {
			refs = append(refs, codexExpandImageReferenceValue(value)...)
		}
	}
	return refs
}

func codexMultipartMaskReferenceValue(form *multipart.Form) string {
	if form == nil {
		return ""
	}
	for _, key := range []string{"mask", "mask[]"} {
		for _, value := range form.Value[key] {
			refs := codexExpandImageReferenceValue(value)
			if len(refs) > 0 && strings.TrimSpace(refs[0]) != "" {
				return strings.TrimSpace(refs[0])
			}
		}
	}
	return ""
}

func codexMultipartFileToDataURL(fileHeader *multipart.FileHeader) (string, error) {
	if fileHeader == nil {
		return "", fmt.Errorf("upload file is nil")
	}
	f, errOpen := fileHeader.Open()
	if errOpen != nil {
		return "", fmt.Errorf("open upload file failed: %w", errOpen)
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.Errorf("codex openai images: close upload file error: %v", errClose)
		}
	}()

	data, errRead := io.ReadAll(f)
	if errRead != nil {
		return "", fmt.Errorf("read upload file failed: %w", errRead)
	}
	mediaType := strings.TrimSpace(fileHeader.Header.Get("Content-Type"))
	if mediaType == "" {
		mediaType = http.DetectContentType(data)
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// codexExtractImageResults extracts image generation results directly from the
// completed event and the items collected from response.output_item.done events,
// without rebuilding the full completed JSON.
//
// It prefers image_generation_call items already present in the completed event's
// response.output and only falls back to the collected items when that output is
// empty, mirroring the semantics of patchCodexCompletedOutput + the previous
// extractor. Skipping the concatenate-and-reparse step avoids two large copies of
// the base64 payload, which matters for multi-megabyte generated images.
func codexExtractImageResults(completed []byte, itemsByIndex map[int64][]byte, fallback [][]byte) (results []codexImageCallResult, createdAt int64, usageRaw []byte, firstMeta codexImageCallResult, err error) {
	if gjson.GetBytes(completed, "type").String() != "response.completed" {
		return nil, 0, nil, codexImageCallResult{}, fmt.Errorf("unexpected event type")
	}
	createdAt = gjson.GetBytes(completed, "response.created_at").Int()
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}

	appendItem := func(item gjson.Result) {
		if item.Get("type").String() != "image_generation_call" {
			return
		}
		res := strings.TrimSpace(item.Get("result").String())
		if res == "" {
			return
		}
		entry := codexImageCallResult{
			Result:        res,
			RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
			OutputFormat:  strings.TrimSpace(item.Get("output_format").String()),
			Size:          strings.TrimSpace(item.Get("size").String()),
			Background:    strings.TrimSpace(item.Get("background").String()),
			Quality:       strings.TrimSpace(item.Get("quality").String()),
		}
		if len(results) == 0 {
			firstMeta = entry
		}
		results = append(results, entry)
	}

	var outputItems []gjson.Result
	if output := gjson.GetBytes(completed, "response.output"); output.Exists() && output.IsArray() {
		outputItems = output.Array()
	}
	if len(outputItems) > 0 {
		// Completed event already carries the output; extract from it in place.
		results = make([]codexImageCallResult, 0, len(outputItems))
		for _, item := range outputItems {
			appendItem(item)
		}
	} else if len(itemsByIndex) > 0 || len(fallback) > 0 {
		// Completed output was empty; extract directly from the collected items,
		// preserving their original output_index ordering.
		results = make([]codexImageCallResult, 0, len(itemsByIndex)+len(fallback))
		if len(itemsByIndex) > 0 {
			indexes := make([]int64, 0, len(itemsByIndex))
			for idx := range itemsByIndex {
				indexes = append(indexes, idx)
			}
			sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
			for _, idx := range indexes {
				appendItem(gjson.ParseBytes(itemsByIndex[idx]))
			}
		}
		for _, raw := range fallback {
			appendItem(gjson.ParseBytes(raw))
		}
	}

	if usage := gjson.GetBytes(completed, "response.tool_usage.image_gen"); usage.Exists() && usage.IsObject() {
		usageRaw = []byte(usage.Raw)
	}
	return results, createdAt, usageRaw, firstMeta, nil
}

func codexBuildImagesAPIResponse(results []codexImageCallResult, createdAt int64, usageRaw []byte, firstMeta codexImageCallResult, responseFormat string) ([]byte, error) {
	out := []byte(`{"created":0,"data":[]}`)
	out, _ = sjson.SetBytes(out, "created", createdAt)
	if firstMeta.Background != "" {
		out, _ = sjson.SetBytes(out, "background", firstMeta.Background)
	}
	if firstMeta.OutputFormat != "" {
		out, _ = sjson.SetBytes(out, "output_format", firstMeta.OutputFormat)
	}
	if firstMeta.Quality != "" {
		out, _ = sjson.SetBytes(out, "quality", firstMeta.Quality)
	}
	if firstMeta.Size != "" {
		out, _ = sjson.SetBytes(out, "size", firstMeta.Size)
	}
	if len(usageRaw) > 0 && json.Valid(usageRaw) {
		out, _ = sjson.SetRawBytes(out, "usage", usageRaw)
	}

	responseFormat = codexNormalizeImageResponseFormat(responseFormat)
	items := make([][]byte, 0, len(results))
	for _, img := range results {
		item := []byte(`{}`)
		if img.RevisedPrompt != "" {
			item, _ = sjson.SetBytes(item, "revised_prompt", img.RevisedPrompt)
		}
		if responseFormat == "url" {
			item, _ = sjson.SetBytes(item, "url", "data:"+codexMimeTypeFromOutputFormat(img.OutputFormat)+";base64,"+img.Result)
		} else {
			item, _ = sjson.SetBytes(item, "b64_json", img.Result)
		}
		items = append(items, item)
	}
	out, _ = sjson.SetRawBytes(out, "data", helps.JoinRawJSONArray(items))
	return out, nil
}

func codexBuildImagePartialFrame(payload []byte, responseFormat string, streamPrefix string) []byte {
	b64 := strings.TrimSpace(gjson.GetBytes(payload, "partial_image_b64").String())
	if b64 == "" {
		return nil
	}
	outputFormat := strings.TrimSpace(gjson.GetBytes(payload, "output_format").String())
	eventName := strings.TrimSpace(streamPrefix) + ".partial_image"
	data := []byte(`{"type":"","partial_image_index":0}`)
	data, _ = sjson.SetBytes(data, "type", eventName)
	data, _ = sjson.SetBytes(data, "partial_image_index", gjson.GetBytes(payload, "partial_image_index").Int())
	if codexNormalizeImageResponseFormat(responseFormat) == "url" {
		data, _ = sjson.SetBytes(data, "url", "data:"+codexMimeTypeFromOutputFormat(outputFormat)+";base64,"+b64)
	} else {
		data, _ = sjson.SetBytes(data, "b64_json", b64)
	}
	return codexBuildSSEFrame(eventName, data)
}

func codexBuildImageCompletedFrame(ctx context.Context, cfg *config.Config, img codexImageCallResult, usageRaw []byte, responseFormat string, streamPrefix string) []byte {
	eventName := strings.TrimSpace(streamPrefix) + ".completed"
	data := []byte(`{"type":""}`)
	data, _ = sjson.SetBytes(data, "type", eventName)
	if len(usageRaw) > 0 && json.Valid(usageRaw) {
		data, _ = sjson.SetRawBytes(data, "usage", usageRaw)
	}
	storedURL := "data:" + codexMimeTypeFromOutputFormat(img.OutputFormat) + ";base64," + img.Result
	if cfg != nil && cfg.ImageStorage.Enabled {
		if url, errStore := imagestorage.StoreBase64(ctx, cfg, img.Result, img.OutputFormat); errStore == nil {
			storedURL = url
		} else {
			log.WithError(errStore).Warn("image storage failed; returning inline image URL")
		}
	}
	if codexNormalizeImageResponseFormat(responseFormat) == "url" {
		data, _ = sjson.SetBytes(data, "url", storedURL)
	} else {
		data, _ = sjson.SetBytes(data, "b64_json", img.Result)
		data, _ = sjson.SetBytes(data, "url", storedURL)
	}
	return codexBuildSSEFrame(eventName, data)
}

func codexBuildSSEFrame(eventName string, data []byte) []byte {
	var buf bytes.Buffer
	if strings.TrimSpace(eventName) != "" {
		buf.WriteString("event: ")
		buf.WriteString(eventName)
		buf.WriteString("\n")
	}
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	return buf.Bytes()
}

func codexMimeTypeFromOutputFormat(outputFormat string) string {
	switch strings.ToLower(strings.TrimSpace(outputFormat)) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}
