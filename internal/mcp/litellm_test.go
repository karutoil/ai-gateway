package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseServerSelection(t *testing.T) {
	cases := []struct {
		url   string
		all   bool
		names []string
		err   bool
	}{
		{"litellm_proxy", true, nil, false},
		{"litellm_proxy/mcp", true, nil, false},
		{"litellm_proxy/mcp/", true, nil, false},
		{"litellm_proxy/mcp/github", false, []string{"github"}, false},
		{"litellm_proxy/mcp/github,filesystem", false, []string{"github", "filesystem"}, false},
		{"litellm_proxy/mcp/github, filesystem", false, []string{"github", "filesystem"}, false},
		{"litellm_proxy/github", false, []string{"github"}, false},
		{"https://evil.example.com/mcp", false, nil, true},
		{"github", false, nil, true},
	}
	for _, tc := range cases {
		sel, names, err := parseServerSelection(tc.url)
		if tc.err {
			if err == nil {
				t.Errorf("%q: expected an error", tc.url)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.url, err)
			continue
		}
		if tc.all && sel != selectionAll {
			t.Errorf("%q: expected all-servers selection", tc.url)
		}
		if !tc.all && sel != selectionNamed {
			t.Errorf("%q: expected named selection", tc.url)
		}
		if strings.Join(names, ",") != strings.Join(tc.names, ",") {
			t.Errorf("%q: names = %v, want %v", tc.url, names, tc.names)
		}
	}
}

func TestApprovalNever(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`"never"`, true},
		{`"Never"`, true},
		{`"always"`, false},
		{`"on_request"`, false},
		{`{"never":{}}`, true},
		{`{"always":{}}`, false},
		{``, false},
		{`null`, false},
	}
	for _, tc := range cases {
		got := approvalNever(json.RawMessage(tc.raw))
		if got != tc.want {
			t.Errorf("approvalNever(%s) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func fixtureAccess() (*Access, []Tool) {
	a := Server{ID: "srv-a", Name: "GitHub", Slug: "github", Enabled: true}
	b := Server{ID: "srv-b", Name: "Filesystem", Slug: "filesystem", Enabled: true}
	tools := []Tool{
		{ServerID: a.ID, ServerSlug: a.Slug, ServerName: a.Name, Name: "search_issues", NamespacedName: "github-search_issues",
			Description: "Search issues", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
		{ServerID: a.ID, ServerSlug: a.Slug, ServerName: a.Name, Name: "create_issue", NamespacedName: "github-create_issue",
			Description: "Create an issue", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{ServerID: b.ID, ServerSlug: b.Slug, ServerName: b.Name, Name: "read_file", NamespacedName: "filesystem-read_file",
			Description: "Read a file", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	return &Access{Servers: []Server{a, b}, Tools: tools}, tools
}

func TestBuildPlanAllServers(t *testing.T) {
	access, _ := fixtureAccess()
	plan, tools, err := BuildPlan(access, []Reference{{ServerURL: "litellm_proxy"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Bindings) != 3 || len(tools) != 3 {
		t.Fatalf("bindings=%d tools=%d, want 3/3", len(plan.Bindings), len(tools))
	}
	if plan.AutoExecute {
		t.Fatal("require_approval was absent: auto execution must stay off")
	}
}

func TestBuildPlanSingleAndMultipleServers(t *testing.T) {
	access, _ := fixtureAccess()
	plan, _, err := BuildPlan(access, []Reference{{ServerURL: "litellm_proxy/mcp/github"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Bindings) != 2 {
		t.Fatalf("single-server bindings = %d, want 2", len(plan.Bindings))
	}
	for name := range plan.Bindings {
		if !strings.HasPrefix(name, "github-") {
			t.Fatalf("unexpected tool %q in a github-only selection", name)
		}
	}

	plan, _, err = BuildPlan(access, []Reference{{ServerURL: "litellm_proxy/mcp/github,filesystem"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Bindings) != 3 {
		t.Fatalf("multi-server bindings = %d, want 3", len(plan.Bindings))
	}
}

func TestBuildPlanAllowedToolsFilter(t *testing.T) {
	access, _ := fixtureAccess()
	plan, _, err := BuildPlan(access, []Reference{{
		ServerURL:    "litellm_proxy",
		AllowedTools: []string{"github-search_issues"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Bindings) != 1 {
		t.Fatalf("bindings = %d, want 1", len(plan.Bindings))
	}
	if _, ok := plan.Bindings["github-search_issues"]; !ok {
		t.Fatalf("wrong binding set: %v", plan.Bindings)
	}
}

func TestBuildPlanEmptyAllowedToolsPermitsAll(t *testing.T) {
	access, _ := fixtureAccess()
	plan, _, err := BuildPlan(access, []Reference{{ServerURL: "litellm_proxy", AllowedTools: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Bindings) != 3 {
		t.Fatalf("bindings = %d, want 3", len(plan.Bindings))
	}
}

func TestBuildPlanUnknownServerIsRefused(t *testing.T) {
	access, _ := fixtureAccess()
	_, _, err := BuildPlan(access, []Reference{{ServerURL: "litellm_proxy/mcp/nope"}})
	if err == nil || !strings.Contains(err.Error(), "not registered or not authorized") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildPlanNetworkURLLikeSentinelIsRefused(t *testing.T) {
	access, _ := fixtureAccess()
	_, _, err := BuildPlan(access, []Reference{{ServerURL: "https://mcp.example.com/mcp"}})
	if err == nil {
		t.Fatal("a real URL must be refused: the gateway only accepts the litellm_proxy sentinel")
	}
}

func TestBuildPlanApprovalNeverMarksAuto(t *testing.T) {
	access, _ := fixtureAccess()
	plan, _, err := BuildPlan(access, []Reference{{
		ServerURL:       "litellm_proxy/mcp/github",
		RequireApproval: json.RawMessage(`"never"`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.AutoExecute {
		t.Fatal("AutoExecute should be true for require_approval: never")
	}
	if len(plan.AutoNames) != 2 {
		t.Fatalf("auto names = %d, want 2", len(plan.AutoNames))
	}
}

func TestBuildPlanMixedApprovalKeepsManualToolsManual(t *testing.T) {
	access, _ := fixtureAccess()
	plan, _, err := BuildPlan(access, []Reference{
		{ServerURL: "litellm_proxy/mcp/github", RequireApproval: json.RawMessage(`"never"`)},
		{ServerURL: "litellm_proxy/mcp/filesystem", RequireApproval: json.RawMessage(`"always"`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.AutoNames["github-search_issues"] {
		t.Fatal("github tools should be auto")
	}
	if plan.AutoNames["filesystem-read_file"] {
		t.Fatal("filesystem tool must NOT be auto with require_approval: always")
	}
}

/* ------------------------------------------------------------------ */
/* Body expansion per protocol                                        */
/* ------------------------------------------------------------------ */

func TestExpandBodyOpenAIChat(t *testing.T) {
	access, _ := fixtureAccess()
	plan, tools, err := BuildPlan(access, []Reference{{ServerURL: "litellm_proxy/mcp/github"}})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/github","require_approval":"never"},{"type":"function","function":{"name":"keep_me","parameters":{"type":"object"}}}]}`)
	out, err := ExpandBody(body, ProtocolChat, plan, tools)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	rawTools, _ := root["tools"].([]any)
	if len(rawTools) != 3 { // keep_me + 2 github tools
		t.Fatalf("tools = %d, want 3 (%s)", len(rawTools), out)
	}
	var sawKeep, sawEcho, sawMCP bool
	for _, raw := range rawTools {
		m := raw.(map[string]any)
		if m["type"] == "mcp" {
			sawMCP = true
		}
		fn, _ := m["function"].(map[string]any)
		if fn == nil {
			continue
		}
		switch fn["name"] {
		case "keep_me":
			sawKeep = true
		case "github-search_issues":
			sawEcho = true
		}
		if _, ok := fn["parameters"].(map[string]any); !ok {
			t.Errorf("tool %v has no parameters object", fn["name"])
		}
	}
	if sawMCP {
		t.Fatal("the mcp reference entry leaked into the expanded body")
	}
	if !sawKeep || !sawEcho {
		t.Fatalf("keep=%v echo=%v body=%s", sawKeep, sawEcho, out)
	}
}

func TestExpandBodyAnthropic(t *testing.T) {
	access, _ := fixtureAccess()
	plan, tools, err := BuildPlan(access, []Reference{{ServerURL: "litellm_proxy/mcp/filesystem"}})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":"x"}],"tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/filesystem"}]}`)
	out, err := ExpandBody(body, ProtocolAnthropic, plan, tools)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(root.Tools))
	}
	tool := root.Tools[0]
	if tool["name"] != "filesystem-read_file" {
		t.Fatalf("name = %v", tool["name"])
	}
	if _, ok := tool["input_schema"].(map[string]any); !ok {
		t.Fatalf("anthropic tools need input_schema: %v", tool)
	}
	if _, ok := tool["parameters"]; ok {
		t.Fatal("anthropic tools must not carry an OpenAI parameters field")
	}
}

func TestExpandBodyResponses(t *testing.T) {
	access, _ := fixtureAccess()
	plan, tools, err := BuildPlan(access, []Reference{{ServerURL: "litellm_proxy/mcp/filesystem"}})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"gpt-5","input":"x","tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/filesystem"}]}`)
	out, err := ExpandBody(body, ProtocolResponses, plan, tools)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(root.Tools))
	}
	tool := root.Tools[0]
	if tool["type"] != "function" || tool["name"] != "filesystem-read_file" {
		t.Fatalf("unexpected responses tool: %v", tool)
	}
	if _, ok := tool["parameters"].(map[string]any); !ok {
		t.Fatalf("responses tools need parameters: %v", tool)
	}
}

func TestReferencesFromBodyIgnoresNonMCPTools(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","function":{"name":"x"}}]}`)
	refs, ok := ReferencesFromBody(body)
	if ok || len(refs) != 0 {
		t.Fatalf("refs=%v ok=%v", refs, ok)
	}
}
