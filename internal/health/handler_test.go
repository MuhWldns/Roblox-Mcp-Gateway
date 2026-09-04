package health_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"robloxkit/internal/health"
)

// leakingChecker fails with an error carrying data that must never reach a
// probe response: a full DSN and a driver-style diagnostic.
type leakingChecker struct{}

func (leakingChecker) PingContext(context.Context) error {
	return errors.New("dial tcp 127.0.0.1:3306: connect: connection refused (dsn root:hunter2@tcp(10.0.0.9:3306)/prod?parseTime=true)")
}

type okChecker struct{}

func (okChecker) PingContext(context.Context) error { return nil }

func probe(t *testing.T, handler http.HandlerFunc) *http.Response {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	res := recorder.Result()
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestLiveAnswersFixedOKBody(t *testing.T) {
	res := probe(t, health.NewHandler(nil, nil).Live)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("liveness status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != `{"status":"ok"}` {
		t.Fatalf("liveness body = %q", body)
	}
}

func TestReadyWithoutCheckerReportsOK(t *testing.T) {
	res := probe(t, health.NewHandler(nil, nil).Ready)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("readiness status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != `{"status":"ok"}` {
		t.Fatalf("readiness body = %q", body)
	}
}

func TestReadyReportsOKWhileDependencyAnswers(t *testing.T) {
	res := probe(t, health.NewHandler(okChecker{}, nil).Ready)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("readiness status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != `{"status":"ok"}` {
		t.Fatalf("readiness body = %q", body)
	}
}

func TestReadyFailureAnswersFixedBodyWithoutDetail(t *testing.T) {
	res := probe(t, health.NewHandler(leakingChecker{}, nil).Ready)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readiness status = %d, want %d", res.StatusCode, http.StatusServiceUnavailable)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != `{"status":"unavailable"}` {
		t.Fatalf("readiness body = %q", body)
	}
	for _, forbidden := range []string{"3306", "root", "hunter2", "10.0.0.9", "parseTime", "connection refused"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("readiness body leaked %q: %s", forbidden, body)
		}
	}
}
