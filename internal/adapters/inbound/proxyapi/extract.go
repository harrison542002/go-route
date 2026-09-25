package proxyapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/harrison542002/go-route/internal/core/domains"
)

const metaPrefix = "x-go-route-"

const tenantKey = "tenant"

var ErrInvalidRequest = errors.New("invalid request")

type completionProbe struct {
	Model               string          `json:"model"`
	Stream              bool            `json:"stream"`
	StreamOption        *streamOption   `json:"stream_options"`
	Messages            json.RawMessage `json:"messages"`
	Tools               json.RawMessage `json:"tools"`
	MaxTokens           json.RawMessage `json:"max_tokens"`
	MaxCompletionTokens json.RawMessage `json:"max_completion_tokens"`
	N                   json.RawMessage `json:"n"`
}

type streamOption struct {
	IncludeUsage bool `json:"include_usage"`
}

func ExtractFacts(r *http.Request, raw []byte, tenant domains.Tenant, now time.Time) (domains.RequestFacts, error) {
	var probe completionProbe
	if err := json.Unmarshal(raw, &probe); err != nil {
		return domains.RequestFacts{}, fmt.Errorf("%w: malformed JSON body: %v", ErrInvalidRequest, err)
	}
	if probe.Model == "" {
		return domains.RequestFacts{}, fmt.Errorf("%w: missing required field: model", ErrInvalidRequest)
	}
	wantsUsage := probe.StreamOption != nil && probe.StreamOption.IncludeUsage

	return domains.RequestFacts{
		Tenant:          tenant,
		RequestedModel:  probe.Model,
		Stream:          probe.Stream,
		WantsUsage:      wantsUsage,
		Metadata:        extractMetadata(r.Header),
		PromptSize:      promptSize(probe.Messages, probe.Tools),
		MaxOutputTokens: firstPositive(probe.MaxCompletionTokens, probe.MaxTokens),
		Choices:         firstPositive(probe.N),
		ReceivedAt:      now,
	}, nil
}

type probeMessage struct {
	Content json.RawMessage `json:"content"`
}

type probePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func promptSize(messages, tools json.RawMessage) domains.PromptSize {
	var size domains.PromptSize

	// Tool definitions are tokenised into the prompt too.
	if len(tools) > 0 && string(tools) != "null" {
		size.TextBytes += len(tools)
	}

	var msgs []probeMessage
	if err := json.Unmarshal(messages, &msgs); err != nil {
		if len(messages) > 0 && string(messages) != "null" {
			size.TextBytes += len(messages)
		}
		return size
	}

	size.Messages = len(msgs)
	for _, m := range msgs {
		var text string
		if json.Unmarshal(m.Content, &text) == nil {
			size.TextBytes += len(text)
			continue
		}

		var parts []probePart
		if json.Unmarshal(m.Content, &parts) != nil {
			size.TextBytes += len(m.Content)
			continue
		}
		for _, p := range parts {
			if p.Type == "text" {
				size.TextBytes += len(p.Text)
			} else {
				size.MediaParts++
			}
		}
	}
	return size
}

func firstPositive(vals ...json.RawMessage) int {
	for _, v := range vals {
		var n int
		if json.Unmarshal(v, &n) == nil && n > 0 {
			return n
		}
	}
	return 0
}

// extraMetadata pulls x-go-route-* headers into a flat map
func extractMetadata(h http.Header) map[string]string {
	keys := make([]string, 0, len(h))
	seens := make(map[string]string, len(h))

	for name, vals := range h {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, metaPrefix) || len(vals) == 0 {
			continue
		}

		key := strings.TrimPrefix(lower, metaPrefix)
		if key == "" || key == tenantKey {
			continue
		}

		val := vals[0]
		if len(val) > domains.MaxMetadataLen {
			val = val[:domains.MaxMetadataLen]
		}
		keys = append(keys, key)
		seens[key] = val
	}

	sort.Strings(keys)

	if len(keys) > domains.MaxMetadataKeys {
		keys = keys[:domains.MaxMetadataKeys]
	}

	md := make(map[string]string, len(keys))
	for _, k := range keys {
		md[k] = seens[k]
	}
	return md
}
