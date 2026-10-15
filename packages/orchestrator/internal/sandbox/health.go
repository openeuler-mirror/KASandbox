package sandbox

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
)

func (c *Checks) getHealth(ctx context.Context, timeout time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	address := fmt.Sprintf("http://%s:%d/health", c.sandbox.Slot.HostIPString(), consts.DefaultEnvdServerPort)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return false, err
	}

	response, err := sandboxHttpClient.Do(request)
	if err != nil {
		return false, err
	}
	defer func() {
		// Drain the response body to reuse the connection
		// From response.Body docstring:
		//  // The default HTTP client's Transport may not reuse HTTP/1.x "keep-alive" TCP connections
		//  if the Body is not read to completion and closed.
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}()

	// 兼容任意 2xx：envd /health 早期返回 204，新版返回 200 + JSON body
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, fmt.Errorf("unexpected status code: %d", response.StatusCode)
	}

	return true, nil
}
