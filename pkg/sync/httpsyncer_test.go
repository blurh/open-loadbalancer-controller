package sync

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}
}

func testHttpSyncer(transport roundTripFunc, sleep func(time.Duration)) *HttpSyncer {
	return &HttpSyncer{
		client:          &http.Client{Timeout: time.Second, Transport: transport},
		maxRetries:      2,
		retryBackoff:    10 * time.Millisecond,
		maxRetryBackoff: 100 * time.Millisecond,
		sleep:           sleep,
	}
}

func TestHttpSyncerRetriesTransientStatus(t *testing.T) {
	calls := 0
	var delays []time.Duration
	client := testHttpSyncer(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls < 3 {
			return testResponse(req, http.StatusBadGateway, "temporary"), nil
		}
		body, _ := json.Marshal(RespBody{InternalIPs: []string{"10.0.0.1"}})
		return testResponse(req, http.StatusOK, string(body)), nil
	}, func(delay time.Duration) {
		delays = append(delays, delay)
	})

	resp := &RespBody{}
	if err := client.Sync("http://agent.test", ControllerResources{}, resp); err != nil {
		t.Fatalf("Sync error: %v", err)
	}

	if calls != 3 {
		t.Fatalf("request count = %d, want 3", calls)
	}
	if !reflect.DeepEqual(delays, []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}) {
		t.Fatalf("retry delays = %v, want [10ms 20ms]", delays)
	}
	if !reflect.DeepEqual(resp.InternalIPs, []string{"10.0.0.1"}) {
		t.Fatalf("response = %+v, want internal IP", resp)
	}
}

func TestHttpSyncerDoesNotRetryPermanentStatus(t *testing.T) {
	calls := 0
	client := testHttpSyncer(func(req *http.Request) (*http.Response, error) {
		calls++
		return testResponse(req, http.StatusBadRequest, "invalid request"), nil
	}, func(time.Duration) {
		t.Fatal("permanent error should not back off")
	})

	if err := client.Sync("http://agent.test", ControllerResources{}, &RespBody{}); err == nil {
		t.Fatal("Sync succeeded, want error")
	}
	if calls != 1 {
		t.Fatalf("request count = %d, want 1", calls)
	}
}

func TestHttpSyncerRetriesCleanup(t *testing.T) {
	calls := 0
	client := testHttpSyncer(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return testResponse(req, http.StatusServiceUnavailable, "agent unavailable"), nil
		}
		return testResponse(req, http.StatusNoContent, ""), nil
	}, func(time.Duration) {})

	if err := client.Clean("http://agent.test", ServiceRef{Namespace: "default", Name: "web"}); err != nil {
		t.Fatalf("Clean error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("request count = %d, want 2", calls)
	}
}

func TestHttpSyncerTimeoutIsRetriedAndBounded(t *testing.T) {
	calls := 0
	client := &HttpSyncer{
		client: &http.Client{
			Timeout: 5 * time.Millisecond,
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				<-req.Context().Done()
				return nil, req.Context().Err()
			}),
		},
		maxRetries:      2,
		retryBackoff:    0,
		maxRetryBackoff: 0,
	}
	if err := client.Sync("http://agent.test", ControllerResources{}, &RespBody{}); err == nil {
		t.Fatal("Sync succeeded, want timeout error")
	}
	if calls != 3 {
		t.Fatalf("request count = %d, want 3", calls)
	}
}
