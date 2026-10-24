package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spore-host/puffball/internal/scrub"
	"github.com/spore-host/puffball/pkg/awscfg"
)

// recorder is an http.RoundTripper that keeps the body of the last response,
// so a probe can save exactly what the service sent rather than the SDK's
// re-marshalled view of it.
type recorder struct {
	base http.RoundTripper
	mu   sync.Mutex
	last []byte
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.base.RoundTrip(req)
	if err != nil || resp.Body == nil {
		return resp, err
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	r.mu.Lock()
	r.last = b
	r.mu.Unlock()
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return resp, nil
}

// Reset forgets the last body, so a failed call can't be paired with the
// previous call's response.
func (r *recorder) Reset() {
	r.mu.Lock()
	r.last = nil
	r.mu.Unlock()
}

// scrubbed returns err's text with account IDs and IAM principal names
// removed: SDK errors embed bucket hostnames, ARNs and "User: arn:aws:iam::…"
// (law 7). Every message the probe prints goes through it.
func scrubbed(err error) string {
	return string(scrub.Scrub([]byte(err.Error())))
}

// Last returns the most recent response body.
func (r *recorder) Last() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.last...)
}

// loadConfig returns an AWS config for region whose HTTP traffic goes through a
// fresh recorder. Each region gets its own config so parallel probes don't
// share a "last response".
func loadConfig(ctx context.Context, region string) (aws.Config, *recorder, error) {
	rec := &recorder{base: http.DefaultTransport}
	cfg, err := awscfg.Load(ctx, region)
	if err != nil {
		return aws.Config{}, nil, err
	}
	cfg.HTTPClient = &http.Client{Transport: rec}
	return cfg, rec, nil
}

// seedRegion is the region the shared spore.host config resolves to, used as
// the starting point for device-region discovery.
func seedRegion(ctx context.Context) (string, error) {
	cfg, err := awscfg.Load(ctx, "")
	if err != nil {
		return "", err
	}
	if cfg.Region == "" {
		return "", usageErr{"no AWS region configured to seed discovery; set AWS_REGION or the profile's region"}
	}
	return cfg.Region, nil
}

// writeRaw writes a raw response body to path, indenting it when it is JSON.
func writeRaw(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}
	var buf bytes.Buffer
	if json.Indent(&buf, body, "", "  ") == nil {
		body = append(buf.Bytes(), '\n')
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// writeJSON writes v as indented JSON to path.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	return writeRaw(path, b)
}
