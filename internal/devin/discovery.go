package devin

import (
	"bytes"
	"compress/gzip"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DiscoveredModel is one model config from GetCliModelConfigs.
type DiscoveredModel struct {
	ID            string
	Name          string
	Description   string
	ContextWindow int
	MaxTokens     int
	ImageInput    bool
	Reasoning     bool
	ToolCalls     bool
	InputCost     float64
	OutputCost    float64
	CacheReadCost float64
	DisplayOption int
	IsRouter      bool
	HarnessUids   []string
	IsNew         bool
	IsBeta        bool
	IsRecommended bool
	Family        *FamilyInfo
	// DefaultInFamily marks the server's default member of its family lane.
	DefaultInFamily bool
}

// FamilyInfo is the server-declared family lane metadata of one config.
type FamilyInfo struct {
	Label     string
	Entries   []FamilyEntry
	IsDefault bool
}

// FamilyEntry is one modelFamilyMetadata entry: the effort/service axis
// value (order) under a human name.
type FamilyEntry struct {
	Key   string
	Order int
	Name  string
}

// Display slots the native client filters client-side: quick-review and
// internal-default configs are never surfaced.
const (
	displayQuickReview     = 4
	displayInternalDefault = 6
)

// Wire uids whose configs advertise image support but whose backend silently
// drops prompt images (verified live against the native CLI).
var imageBlindUIDs = map[string]bool{"swe-1-6": true, "swe-1-6-fast": true}

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
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+discoveryPath, bytes.NewReader(BuildDiscoveryRequest(sessionToken)))
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
	var (
		id, name, description string
		disabled              bool
		supportsImages        bool
		hasImages             bool
		ctx                   int
		isBeta, isRec, isNew  bool
		modelInfo             []byte
		family                *FamilyInfo
		defaultInFamily       bool
		dims                  [][]byte
	)
	for _, f := range fields {
		switch {
		case f.number == 22 && f.wire == 2:
			id = strings.TrimSpace(string(f.value))
		case f.number == 1 && f.wire == 2:
			name = strings.TrimSpace(string(f.value))
		case f.number == 4 && f.wire == 0:
			disabled = f.vint != 0
		case f.number == 5 && f.wire == 0:
			supportsImages, hasImages = f.vint != 0, true
		case f.number == 9 && f.wire == 0:
			isBeta = f.vint != 0
		case f.number == 11 && f.wire == 0:
			isRec = f.vint != 0
		case f.number == 15 && f.wire == 0:
			isNew = f.vint != 0
		case f.number == 27 && f.wire == 2:
			description = strings.TrimSpace(string(f.value))
		case f.number == 18 && f.wire == 0:
			if f.vint > 0 {
				ctx = int(f.vint)
			}
		case f.number == 23 && f.wire == 2:
			modelInfo = f.value
		case f.number == 30 && f.wire == 2:
			family = parseFamily(f.value)
		case f.number == 31 && f.wire == 0:
			defaultInFamily = f.vint != 0
		case f.number == 32 && f.wire == 2:
			dims = append(dims, f.value)
		}
	}
	// Absent flag reads as 0 (enabled), matching the reference decoder.
	if disabled {
		return DiscoveredModel{}, false
	}
	if id == "" {
		return DiscoveredModel{}, false
	}
	var (
		maxOut, display    int
		isRouter           bool
		harness            []string
		featSeen           bool
		featThink, featImg bool
		featTools          bool
	)
	if modelInfo != nil {
		mf, merr := parseFields(modelInfo)
		if merr != nil {
			return DiscoveredModel{}, false
		}
		var feat []byte
		for _, f := range mf {
			switch {
			case f.number == 13 && f.wire == 0:
				if f.vint > 0 {
					maxOut = int(f.vint)
				}
			case f.number == 22 && f.wire == 0:
				display = int(f.vint)
			case f.number == 25 && f.wire == 0:
				isRouter = f.vint != 0
			case f.number == 20 && f.wire == 2:
				harness = append(harness, strings.TrimSpace(string(f.value)))
			case f.number == 6 && f.wire == 2:
				feat = f.value
			}
		}
		if feat != nil {
			ff, ferr := parseFields(feat)
			if ferr != nil {
				return DiscoveredModel{}, false
			}
			featSeen = true
			for _, f := range ff {
				if f.wire != 0 {
					continue
				}
				switch f.number {
				case 11:
					featImg = f.vint != 0
				case 12:
					featTools = f.vint != 0
				case 15:
					featThink = f.vint != 0
				}
			}
		}
	}
	// Internal display slots are never surfaced, exactly as native filters.
	if display == displayQuickReview || display == displayInternalDefault {
		return DiscoveredModel{}, false
	}
	// Harness-less routers are server-side dispatchers, not chat uids: only
	// harness-backed composites take the chat path.
	if isRouter && len(harness) == 0 {
		return DiscoveredModel{}, false
	}
	if name == "" {
		name = id
	}
	// Server features are authoritative for reasoning support; the label
	// heuristic only covers configs that ship no modelFeatures at all.
	reasoning := featThink
	if !featSeen {
		reasoning = !noReasoningLabel.MatchString(name) && reasoningLabel.MatchString(name)
	}
	images := supportsImages
	if featSeen {
		images = featImg
	} else {
		images = hasImages && supportsImages
	}
	images = images && !imageBlindUIDs[id]
	toolCalls := true
	if featSeen {
		toolCalls = featTools
	}
	contextWindow := ctx
	if contextWindow <= 0 {
		contextWindow = defaultContextWindow
	}
	maxTokens := maxOut
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	cost := costFromDimensions(dims)
	return DiscoveredModel{
		ID: id, Name: name, Description: description,
		ContextWindow: contextWindow, MaxTokens: maxTokens,
		ImageInput: images, Reasoning: reasoning, ToolCalls: toolCalls,
		InputCost: cost.in, OutputCost: cost.out, CacheReadCost: cost.read,
		DisplayOption: display, IsRouter: isRouter, HarnessUids: harness,
		IsNew: isNew, IsBeta: isBeta, IsRecommended: isRec,
		Family: family, DefaultInFamily: defaultInFamily,
	}, true
}

// costCard is per-million-token rates parsed from cost dimensions.
type costCard struct{ in, out, read float64 }

// costFromDimensions reads the config's cost dimensions (kind COST=1 or
// COST_FUZZY=2): labels "input" / "cached input" / "output" with a value per
// denominator ("1M tokens"). A "sidekick" marker dimension separates a
// composite's own card from its components' cards — reading stops there.
// Cache writes bill at the input rate and stay 0.
func costFromDimensions(dims [][]byte) costCard {
	var cost costCard
	for _, raw := range dims {
		fields, err := parseFields(raw)
		if err != nil {
			continue
		}
		var label, denom string
		var value float64
		var hasValue bool
		var kind uint64
		for _, f := range fields {
			switch {
			case f.number == 1 && f.wire == 2:
				label = strings.ToLower(strings.TrimSpace(string(f.value)))
			case f.number == 2 && f.wire == 5:
				value = float64(math.Float32frombits(f.fixed))
				hasValue = true
			case f.number == 3 && f.wire == 2:
				denom = string(f.value)
			case f.number == 6 && f.wire == 0:
				kind = f.vint
			}
		}
		if label == "sidekick" {
			break
		}
		if !hasValue || (kind != 1 && kind != 2) {
			continue
		}
		// Round off float32 noise at sub-cent precision.
		perMillion := math.Round(((value*1_000_000)/denomTokens(denom))*1e6) / 1e6
		switch label {
		case "input":
			cost.in = perMillion
		case "cached input":
			cost.read = perMillion
		case "output":
			cost.out = perMillion
		}
	}
	return cost
}

var denomRe = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*([kmb])?`)

// denomTokens parses a cost denominator ("1M tokens") into covered tokens.
func denomTokens(d string) float64 {
	m := denomRe.FindStringSubmatch(d)
	if m == nil {
		return 1_000_000
	}
	num, _ := strconv.ParseFloat(m[1], 64)
	scale := 1.0
	switch strings.ToLower(m[2]) {
	case "k":
		scale = 1_000
	case "m":
		scale = 1_000_000
	case "b":
		scale = 1_000_000_000
	}
	if num*scale <= 0 {
		return 1_000_000
	}
	return num * scale
}

// parseFamily decodes modelFamilyMetadata: label (1), entries (2:
// key (1), value (2: order (1), name (2))), isDefault (3).
func parseFamily(raw []byte) *FamilyInfo {
	fields, err := parseFields(raw)
	if err != nil {
		return nil
	}
	fam := &FamilyInfo{}
	for _, f := range fields {
		switch {
		case f.number == 1 && f.wire == 2:
			fam.Label = strings.TrimSpace(string(f.value))
		case f.number == 2 && f.wire == 2:
			if e := parseFamilyEntry(f.value); e != nil {
				fam.Entries = append(fam.Entries, *e)
			}
		case f.number == 3 && f.wire == 0:
			fam.IsDefault = f.vint != 0
		}
	}
	if fam.Label == "" && len(fam.Entries) == 0 && !fam.IsDefault {
		return nil
	}
	return fam
}

func parseFamilyEntry(raw []byte) *FamilyEntry {
	fields, err := parseFields(raw)
	if err != nil {
		return nil
	}
	var e FamilyEntry
	var hasValue bool
	for _, f := range fields {
		switch {
		case f.number == 1 && f.wire == 2:
			e.Key = string(f.value)
		case f.number == 2 && f.wire == 2:
			vf, verr := parseFields(f.value)
			if verr != nil {
				return nil
			}
			hasValue = true
			for _, v := range vf {
				switch {
				case v.number == 1 && v.wire == 0:
					e.Order = int(v.vint)
				case v.number == 2 && v.wire == 2:
					e.Name = string(v.value)
				}
			}
		}
	}
	if e.Key == "" || !hasValue {
		return nil
	}
	return &e
}
