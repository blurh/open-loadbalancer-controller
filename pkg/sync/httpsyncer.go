package sync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/blurh/open-loadbalancer-controller/pkg/config"
)

const (
	RouteCleanupPath = "/cleanup"
	RouteSyncPath    = "/sync"
)

type HttpSyncer struct {
	client          *http.Client
	maxRetries      int
	retryBackoff    time.Duration
	maxRetryBackoff time.Duration
	sleep           func(time.Duration)
}

func NewHttpSyncer() Syncer {
	c := config.NewConfig()
	return &HttpSyncer{
		client:          &http.Client{Timeout: c.SyncHTTPTimeout()},
		maxRetries:      c.SyncMaxRetries(),
		retryBackoff:    c.SyncRetryBackoff(),
		maxRetryBackoff: c.SyncRetryMaxBackoff(),
		sleep:           time.Sleep,
	}
}

// normalizeBase 给没有 scheme 的 instance 补上 http://。
func normalizeBase(instance string) string {
	if strings.HasPrefix(instance, "http://") || strings.HasPrefix(instance, "https://") {
		return instance
	}
	return "http://" + instance
}

func (hs *HttpSyncer) Sync(instance string, cr ControllerResources, respBody *RespBody) error {
	url := normalizeBase(instance) + RouteSyncPath
	body, err := json.Marshal(cr)
	if err != nil {
		return err
	}
	respBytes, err := hs.doRequest(http.MethodPost, url, body, func(status int) bool {
		return status == http.StatusOK
	})
	if err != nil {
		return fmt.Errorf("sync to %s: %w", instance, err)
	}
	if len(respBytes) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBytes, respBody); err != nil {
		return fmt.Errorf("sync to %s: invalid response: %w", instance, err)
	}
	return nil
}

func (hs *HttpSyncer) Clean(instance string, ref ServiceRef) error {
	url := normalizeBase(instance) + RouteCleanupPath
	body, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	_, err = hs.doRequest(http.MethodPost, url, body, func(status int) bool {
		return status == http.StatusOK || status == http.StatusNoContent
	})
	if err != nil {
		return fmt.Errorf("cleanup to %s: %w", instance, err)
	}
	return nil
}

// doRequest 执行一次带有限重试的 JSON POST。Apply/Delete 都是幂等操作，
// 因此网络错误和瞬时 HTTP 错误可以安全重试；明确的 4xx 直接返回。
func (hs *HttpSyncer) doRequest(method, url string, payload []byte, isSuccess func(int) bool) ([]byte, error) {
	maxRetries := hs.maxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	attempts := maxRetries + 1
	attemptsUsed := 0
	var lastErr error

	for attempt := 0; attempt < attempts; attempt++ {
		attemptsUsed++
		if attempt > 0 {
			hs.wait(hs.retryDelay(attempt))
		}

		req, err := http.NewRequest(method, url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := hs.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request failed: %w", err)
			continue
		}

		respBytes, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("read response failed: %w", readErr)
			continue
		}

		if isSuccess(resp.StatusCode) {
			return respBytes, nil
		}

		lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
		if !retryableStatus(resp.StatusCode) {
			break
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("request failed")
	}
	return nil, fmt.Errorf("request failed after %d attempt(s): %w", attemptsUsed, lastErr)
}

func (hs *HttpSyncer) wait(delay time.Duration) {
	if delay <= 0 {
		return
	}
	if hs.sleep != nil {
		hs.sleep(delay)
		return
	}
	time.Sleep(delay)
}

// retryDelay 使用指数退避：backoff, 2*backoff, 4*backoff...，并限制上限。
func (hs *HttpSyncer) retryDelay(retryNumber int) time.Duration {
	if retryNumber <= 0 || hs.retryBackoff <= 0 {
		return 0
	}
	delay := hs.retryBackoff
	maxDelay := hs.maxRetryBackoff
	if maxDelay <= 0 || maxDelay < delay {
		maxDelay = delay
	}
	for i := 1; i < retryNumber; i++ {
		if delay >= maxDelay/2 {
			return maxDelay
		}
		delay *= 2
	}
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		status >= http.StatusInternalServerError
}
