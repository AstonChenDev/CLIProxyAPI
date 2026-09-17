package managementasset

import (
	"bytes"
	"embed"
)

//go:embed concurrency/index.html concurrency/concurrency.js concurrency/concurrency.css
var concurrencyAssets embed.FS

// CredentialConcurrencyAsset returns only the public, bundled settings assets.
func CredentialConcurrencyAsset(name string) ([]byte, string, bool) {
	contentType := ""
	switch name {
	case "index.html":
		contentType = "text/html; charset=utf-8"
	case "concurrency.js":
		contentType = "text/javascript; charset=utf-8"
	case "concurrency.css":
		contentType = "text/css; charset=utf-8"
	default:
		return nil, "", false
	}
	data, errRead := concurrencyAssets.ReadFile("concurrency/" + name)
	return data, contentType, errRead == nil
}

// WithCredentialConcurrencyLink preserves the official panel and adds a settings shortcut.
func WithCredentialConcurrencyLink(data []byte) []byte {
	const marker = `id="cpa-concurrency-settings"`
	if bytes.Contains(data, []byte(marker)) {
		return data
	}
	link := `<a id="cpa-concurrency-settings" href="/credential-concurrency.html" style="position:fixed;right:24px;bottom:24px;z-index:9999;background:#117f6e;color:#fff;padding:12px 18px;border-radius:10px;font:600 14px sans-serif;text-decoration:none;box-shadow:0 4px 16px #0002">凭证并发设置 ↗</a>`
	return bytes.Replace(data, []byte("</body>"), []byte(link+"</body>"), 1)
}
