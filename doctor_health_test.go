package refraktal

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/harness"
)

type doctorProbeHarness struct {
	fake
	prepare func(context.Context, harness.Options) (harness.Launch, error)
}

func (h doctorProbeHarness) Prepare(ctx context.Context, o harness.Options) (harness.Launch, error) {
	return h.prepare(ctx, o)
}

type doctorProbeTransport func(*http.Request) (*http.Response, error)

func (f doctorProbeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func BenchmarkDoctorIndependentChecks(b *testing.B) {
	old := http.DefaultTransport
	http.DefaultTransport = doctorProbeTransport(func(r *http.Request) (*http.Response, error) {
		time.Sleep(100 * time.Millisecond)
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("down")), Header: make(http.Header)}, nil
	})
	defer func() { http.DefaultTransport = old }()
	dir := b.TempDir()
	h := doctorProbeHarness{fake: fake{name: "probe"}, prepare: func(context.Context, harness.Options) (harness.Launch, error) {
		time.Sleep(100 * time.Millisecond)
		return harness.Launch{}, errors.New("unavailable")
	}}
	o := DoctorOptions{Root: filepath.Join(dir, "repo"), LiveDir: filepath.Join(dir, "live"), Neutral: filepath.Join(dir, "neutral"),
		ConfigPath: filepath.Join(dir, "no-config.json"), Harnesses: []harness.Harness{h},
		Getenv: func(string) string { return "test-key" }, DeciderEndpoint: "http://127.0.0.1/decisions"}
	if err := os.MkdirAll(o.Root, 0o700); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Doctor(context.Background(), o); err != nil {
			b.Fatal(err)
		}
	}
}

func TestDoctorIndependentChecksOverlap(t *testing.T) {
	probeStarted, harnessStarted := make(chan struct{}), make(chan struct{})
	old := http.DefaultTransport
	http.DefaultTransport = doctorProbeTransport(func(r *http.Request) (*http.Response, error) {
		close(probeStarted)
		select {
		case <-harnessStarted:
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("down")), Header: make(http.Header)}, nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	defer func() { http.DefaultTransport = old }()
	dir := t.TempDir()
	h := doctorProbeHarness{fake: fake{name: "probe"}, prepare: func(ctx context.Context, o harness.Options) (harness.Launch, error) {
		if !o.ProbeVersions {
			t.Error("doctor did not request version probes")
		}
		close(harnessStarted)
		select {
		case <-probeStarted:
			return harness.Launch{}, errors.New("unavailable")
		case <-ctx.Done():
			return harness.Launch{}, ctx.Err()
		}
	}}
	o := DoctorOptions{Root: filepath.Join(dir, "repo"), LiveDir: filepath.Join(dir, "live"), Neutral: filepath.Join(dir, "neutral"),
		ConfigPath: filepath.Join(dir, "no-config.json"), Harnesses: []harness.Harness{h},
		Getenv: func(string) string { return "test-key" }, DeciderEndpoint: "http://127.0.0.1/decisions"}
	if err := os.MkdirAll(o.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rep, err := Doctor(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Decider.Kind != "fallback" || !strings.Contains(rep.Decider.Reason, "HTTP 503") || len(rep.Harnesses) != 1 ||
		rep.Harnesses[0].Name != "probe" || rep.Harnesses[0].Status != StatusUnavailable || rep.Harnesses[0].Reason != "unavailable" {
		t.Fatalf("independent checks did not finish together: %+v", rep)
	}
	events, err := os.ReadFile(filepath.Join(o.Root, rep.Run, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	harnessEvent, doctorEvent := strings.Index(string(events), `"event":"harness_check"`), strings.Index(string(events), `"event":"doctor"`)
	if harnessEvent < 0 || doctorEvent < 0 || harnessEvent >= doctorEvent {
		t.Fatalf("reporting order changed: %s", events)
	}
}

func TestDoctorConfigErrorStillReportsDecider(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "invalid-config.json")
	if err := os.WriteFile(configPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := DoctorOptions{Root: dir, LiveDir: filepath.Join(dir, "live"), Neutral: filepath.Join(dir, "neutral"), ConfigPath: configPath,
		Harnesses: []harness.Harness{}, Getenv: func(string) string { return "" }}
	rep, err := Doctor(context.Background(), o)
	if err == nil || rep.Decider.Kind != "fallback" || rep.Decider.Reason != "GEMINI_API_KEY not set (environment or .env)" || rep.Harnesses != nil {
		t.Fatalf("config error changed partial report: %+v, %v", rep, err)
	}
}
