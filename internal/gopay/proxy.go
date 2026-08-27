package gopay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// PreflightProxy confirms that a claimed proxy can establish an HTTPS request.
// A failed slot is intentionally not returned to the caller's pool.
func PreflightProxy(ctx context.Context, proxyURL string) error {
	normalizedProxyURL, err := normalizeProxyURL(proxyURL)
	if err != nil {
		return err
	}
	client, err := configuredHTTPClient(nil, normalizedProxyURL, 12*time.Second)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org?format=json", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		var recordHeaderErr tls.RecordHeaderError
		if errors.Is(err, http.ErrSchemeMismatch) || errors.As(err, &recordHeaderErr) {
			parsedProxyURL, _ := url.Parse(normalizedProxyURL)
			if parsedProxyURL != nil && parsedProxyURL.Scheme == "https" {
				return fmt.Errorf("proxy connectivity: HTTPS proxy endpoint returned plaintext; plain HTTP proxy endpoints must use http:// or omit the scheme: %w", err)
			}
			return fmt.Errorf("proxy connectivity: HTTP proxy did not establish a valid HTTPS CONNECT tunnel: %w", err)
		}
		return fmt.Errorf("proxy connectivity: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.CopyN(io.Discard, response.Body, 4096)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("proxy preflight returned HTTP %d", response.StatusCode)
	}
	return nil
}
