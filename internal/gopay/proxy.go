package gopay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// PreflightProxy confirms that a claimed proxy can establish an HTTPS request.
// A failed slot is intentionally not returned to the caller's pool.
func PreflightProxy(ctx context.Context, proxyURL string) error {
	client, err := configuredHTTPClient(nil, proxyURL, 12*time.Second)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org?format=json", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("proxy connectivity: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.CopyN(io.Discard, response.Body, 4096)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("proxy preflight returned HTTP %d", response.StatusCode)
	}
	return nil
}
