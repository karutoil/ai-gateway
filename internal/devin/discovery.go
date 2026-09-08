package devin

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// DiscoveredModel is one model config from GetCliModelConfigs.
type DiscoveredModel struct {
	ID            string
	Name          string
	ContextWindow int
	MaxTokens     int
	ImageInput    bool
	Reasoning     bool
}

const (
	defaultContextWindow = 200000
	defaultMaxTokens     = 64000
)

var (
	reasoningLabel   = regexp.MustCompile(`(?i)think|thinking|minimal|high|medium|low|xhigh|max|reasoning`)
	noReasoningLabel = regexp.MustCompile(`(?i)\bno thinking\b`)
)

// FetchModels asks Devin for the account's enabled model configs against the
// given transport base URL (empty selects the default host).
func FetchModels(sessionToken, base string, client *http.Client) ([]DiscoveredModel, error) {
	c := client
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Second}
	}
	if strings.TrimSpace(base) == "" {
		base = Host()
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+discoveryPath, bytes.NewReader(BuildUserJWTRequest(sessionToken)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("connect-protocol-version", "1")
	req.Header.Set("Accept", "*/*")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &httpError{Status: resp.StatusCode}
	}
	return DecodeModels(raw)
}

type httpError struct{ Status int }

func (e *httpError) Error() string { return http.StatusText(e.Status) }

// DecodeModels decodes a GetCliModelConfigs payload (raw or gzipped).
func DecodeModels(payload []byte) ([]DiscoveredModel, error) {
	configs, err := configEntries(payload)
	if err != nil {
		return nil, err
	}
	// Fall back to gunzip like the reference extension.
	if configs == nil {
		r, gerr := gzip.NewReader(bytes.NewReader(payload))
		if gerr != nil {
			return nil, err
		}
		raw, rerr := io.ReadAll(io.LimitReader(r, 5<<20))
		_ = r.Close()
		if rerr != nil {
			return nil, err
		}
		configs, err = configEntries(raw)
		if err != nil {
			return nil, err
		}
	}
	seen := map[string]DiscoveredModel{}
	for _, cfg := range configs {
		m, ok := normalizeConfig(cfg)
		if !ok {
			continue
		}
		seen[m.ID] = m
	}
	out := make([]DiscoveredModel, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func configEntries(payload []byte) ([][]byte, error) {
	fields, err := parseFields(payload)
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for _, f := range fields {
		if f.number == 1 && f.wire == 2 {
			out = append(out, f.value)
		}
	}
	if out == nil {
		return nil, nil
	}
	return out, nil
}

func normalizeConfig(cfg []byte) (DiscoveredModel, bool) {
	fields, err := parseFields(cfg)
	if err != nil {
		return DiscoveredModel{}, false
	}
	var id, name string
	var disabled uint64
	var hasDisabled bool
	var ctx, image uint64
	var hasCtx, hasImage bool
	for _, f := range fields {
		switch {
		case f.number == 22 && f.wire == 2:
			id = strings.TrimSpace(string(f.value))
		case f.number == 1 && f.wire == 2:
			name = strings.TrimSpace(string(f.value))
		case f.number == 4 && f.wire == 0:
			disabled, hasDisabled = f.vint, true
		case f.number == 18 && f.wire == 0:
			ctx, hasCtx = f.vint, true
		case f.number == 5 && f.wire == 0:
			image, hasImage = f.vint, true
		}
	}
	// Absent flag reads as 0 (enabled), matching the reference decoder.
	if hasDisabled && disabled != 0 {
		return DiscoveredModel{}, false
	}
	if id == "" {
		return DiscoveredModel{}, false
	}
	if name == "" {
		name = id
	}
	contextWindow := defaultContextWindow
	if hasCtx && ctx > 0 {
		contextWindow = int(ctx)
	}
	maxTokens := contextWindow
	if maxTokens > defaultMaxTokens {
		maxTokens = defaultMaxTokens
	}
	return DiscoveredModel{
		ID: id, Name: name,
		ContextWindow: contextWindow, MaxTokens: maxTokens,
		ImageInput: hasImage && image != 0,
		Reasoning:  !noReasoningLabel.MatchString(name) && reasoningLabel.MatchString(name),
	}, true
}
