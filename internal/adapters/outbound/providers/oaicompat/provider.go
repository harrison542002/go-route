// Package oaicompat implements ports.Provider for any upstream speaking
// the OpenAI chat-completions dialect: OpenAI itself, Azure OpenAI, Groq,
// Together, DeepInfra, vLLM, Ollama, and most of the long tail.

package oaicompat

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/harrison542002/go-route/internal/core/sse"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/tidwall/sjson"
)

type Config struct {
	Name                string
	BaseURL             string
	APIKey              string
	DisableStreamOption bool
	ExtraHeaders        map[string]string
	HTTPClient          *http.Client
}

type Provider struct {
	cfg    Config
	client *http.Client
}

var _ ports.Provider = (*Provider)(nil)

func New(cfg Config) (*Provider, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("oaicompat: Name is required")
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("oaicompat: BaseURL is required")
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")

	client := cfg.HTTPClient
	if client == nil {
		client = defaultClient()
	}

	return &Provider{cfg: cfg, client: client}, nil
}

func (p *Provider) Name() string {
	return p.cfg.Name
}

func (p *Provider) Stream(ctx context.Context, req *ports.ProviderRequest) (ports.StreamReader, error) {
	body, err := p.buildBody(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &ports.ProviderError{
			Kind:      ports.FailureConnect,
			Provider:  p.cfg.Name,
			Message:   err.Error(),
			Retryable: true,
			Err:       err,
		}
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept-Encoding", "identity")
	if p.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	for k, v := range p.cfg.ExtraHeaders {
		httpReq.Header.Set(k, v)
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, &ports.ProviderError{
			Kind:      ports.FailureConnect,
			Provider:  p.cfg.Name,
			Message:   err.Error(),
			Retryable: true,
			Err:       err,
		}
	}
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		_ = resp.Body.Close()
		return nil, classify(p.cfg.Name, resp.StatusCode, errBody)
	}

	if !req.Stream {
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			_ = resp.Body.Close()
			return nil, &ports.ProviderError{
				Kind:      ports.FailureUpstream,
				Provider:  p.cfg.Name,
				Message:   err.Error(),
				Retryable: true,
				Err:       err,
			}
		}
		return &nonStreamReader{body: resp.Body, raw: raw}, nil
	}

	return &reader{
		dec:  sse.NewDecoder(bufio.NewReaderSize(resp.Body, 16<<10)),
		body: resp.Body,
	}, nil
}

func (p *Provider) buildBody(req *ports.ProviderRequest) ([]byte, error) {
	body, err := sjson.SetBytes(req.Body, "model", req.Model)
	if err != nil {
		return nil, &ports.ProviderError{
			Kind:      ports.FailureBadRequest,
			Provider:  p.cfg.Name,
			Message:   "rewrite model: " + err.Error(),
			Retryable: false,
			Err:       err,
		}
	}

	if req.Stream && !p.cfg.DisableStreamOption {
		body, err = sjson.SetBytes(body, "stream_options.include_usage", true)
		if err != nil {
			return nil, &ports.ProviderError{
				Kind:      ports.FailureBadRequest,
				Provider:  p.cfg.Name,
				Message:   "inject stream_options: " + err.Error(),
				Retryable: false,
				Err:       err,
			}
		}
	}

	return body, nil
}

func defaultClient() *http.Client {
	return &http.Client{
		// MUST be zero. http.Client.Timeout bounds the whole exchange
		// including body reads, so any non-zero value silently truncates
		// long generations. Per-request deadlines belong on the context.
		Timeout: 0,
		Transport: &http.Transport{
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 100, // the default of 2 throttles a proxy badly
			IdleConnTimeout:     90 * time.Second,

			// Bounds time-to-headers without bounding the stream. This is
			// exactly the pre-commit window the dispatcher can still fail
			// over from.
			ResponseHeaderTimeout: 30 * time.Second,

			// SSE must never be compressed.
			DisableCompression: true,
		},
	}
}
