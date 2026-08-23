package httpapi

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

var ErrInvalidRequest = errors.New("invalid request")

type completionProbe struct {
	Model        string        `json:"model"`
	Stream       bool          `json:"stream"`
	StreamOption *streamOption `json:"stream_options"`
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
		Tenant:         tenant,
		RequestedModel: probe.Model,
		Stream:         probe.Stream,
		WantsUsage:     wantsUsage,
		Metadata:       extractMetadata(r.Header),
		ReceivedAt:     now,
	}, nil
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
		if key == "" {
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
