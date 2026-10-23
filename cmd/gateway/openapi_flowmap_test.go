package main

// Flow-mapping comma guard.
//
// YAML flow mappings — `{ description: ... }` — are the one place where an
// unquoted comma inside a VALUE is silently catastrophic: the scanner treats it
// as an entry separator, so
//
//	'200': { description: SSE stream, or the dashboard HTML for a navigation }
//
// parses as TWO entries, `description: SSE stream` and
// `or the dashboard HTML for a navigation: null`. Nothing errors. The document
// loads, the JSON-Schema validator then rejects the response object for having
// an unknown property, and the failure text names the schema — not the missing
// quotes. That is exactly how the OpenAPI CI job failed once, and the same
// shape appeared three times in a single commit.
//
// The full schema validation lives in the CI `openapi` job and needs a Python
// dependency, so it is optional locally (see Makefile `ci-all`) and, worse,
// reports only the FIRST error. This test needs no parser and no dependency: a
// flow mapping entry is always a `key: value` pair, so any comma-separated
// segment without a colon is a value that got split.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// blockScalarStart matches a line that opens a block scalar (`key: |`, `key: >-`,
// `key: |2`). Braces inside such content are literal prose, not flow mappings.
var blockScalarStart = regexp.MustCompile(`:[ \t]*[|>][-+0-9]*[ \t]*$`)

// TestEmbeddedOpenAPIHasNoSplitFlowMappingValues fails when a single-line flow
// mapping in the embedded spec contains an entry that is not a key:value pair.
func TestEmbeddedOpenAPIHasNoSplitFlowMappingValues(t *testing.T) {
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatalf("read cmd/gateway/openapi.yaml: %v", err)
	}

	// Block-scalar state: while inBlock, lines indented deeper than keyIndent
	// are scalar content and must not be scanned for flow mappings.
	var (
		inBlock   bool
		keyIndent int
	)

	for i, line := range strings.Split(string(data), "\n") {
		indent := len(line) - len(strings.TrimLeft(line, " \t"))

		if inBlock {
			if strings.TrimSpace(line) == "" {
				continue // blank lines are part of the scalar
			}
			if indent > keyIndent {
				continue // still inside the block scalar
			}
			inBlock = false // dedented: the scalar ended before this line
		}

		if bad, detail := hasSplitFlowMapEntry(line); bad {
			t.Errorf("cmd/gateway/openapi.yaml:%d: %s\n    %s\n"+
				"A comma inside a flow-mapping value must be quoted: "+
				"{ description: 'a, b' }", i+1, detail, strings.TrimSpace(line))
		}

		if blockScalarStart.MatchString(line) {
			inBlock, keyIndent = true, indent
		}
	}
}

// hasSplitFlowMapEntry scans one line for flow mappings and reports the first
// entry that is not a `key: value` pair.
//
// Only flow mappings that open and close on the same line are examined: every
// flow mapping in this spec is single-line, and multi-line ones would need a
// real parser (the CI validator covers those).
func hasSplitFlowMapEntry(line string) (bool, string) {
	runes := []rune(line)
	var (
		inSingle, inDouble bool
		braceDepth         int
		bracketDepth       int
		// bracketDepth when the current flow mapping opened: a map nested in
		// a flow sequence (`[{ a: b }]`) must still recognize its own commas
		// and colons, so these are compared against that baseline rather than
		// against zero.
		openBracketDepth int
		entryStart       int
		entryHasColon    bool
	)

	// atOwnLevel reports whether the scanner is currently inside the entries of
	// the flow mapping that opened last, not in a nested collection.
	atOwnLevel := func() bool {
		return braceDepth == 1 && bracketDepth == openBracketDepth
	}

	// startsFlowMap reports whether a `{` at position i opens a flow mapping
	// rather than being literal text. URI templates (`/api/keys/{id}`) contain
	// braces in a KEY, so a `{` only opens a mapping in value position: after
	// `:`, `,`, `[`, `{`, or at the start of the line.
	startsFlowMap := func(i int) bool {
		for j := i - 1; j >= 0; j-- {
			if runes[j] == ' ' || runes[j] == '\t' {
				continue
			}
			switch runes[j] {
			case ':', ',', '[', '{':
				return true
			}
			return false
		}
		return true // nothing before it on the line
	}

	// check closes the entry currently being accumulated.
	check := func(end int) (bool, string) {
		entry := strings.TrimSpace(string(runes[entryStart:end]))
		if entry == "" || entryHasColon {
			return false, ""
		}
		return true, "flow-mapping entry without a colon: " + entry
	}

	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
			continue
		case inDouble:
			// A backslash escapes the next rune inside a double-quoted scalar.
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				inDouble = false
			}
			continue
		}

		switch c {
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		case '{':
			if braceDepth > 0 || startsFlowMap(i) {
				braceDepth++
				if braceDepth == 1 {
					openBracketDepth = bracketDepth
					entryStart = i + 1
					entryHasColon = false
				}
			}
		case '}':
			if braceDepth == 0 {
				continue // literal `}` from a URI template, not a mapping close
			}
			if braceDepth == 1 {
				if bad, detail := check(i); bad {
					return true, detail
				}
			}
			braceDepth--
		case '[':
			bracketDepth++
		case ']':
			bracketDepth--
		case ':':
			// A colon only makes it a key:value pair at the mapping's own
			// level, not inside a nested flow collection.
			if atOwnLevel() {
				entryHasColon = true
			}
		case ',':
			if atOwnLevel() {
				if bad, detail := check(i); bad {
					return true, detail
				}
				entryStart = i + 1
				entryHasColon = false
			}
		}
	}
	return false, ""
}

// TestHasSplitFlowMapEntry pins the detector's edge cases. Each of these shapes
// appears in the real spec, and getting one wrong either misses the bug or
// fails the build on a valid document.
func TestHasSplitFlowMapEntry(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool // true = split value detected
	}{
		{
			name: "unquoted comma in a description is the bug",
			line: `        '200': { description: SSE stream, or the dashboard HTML }`,
			want: true,
		},
		{
			name: "quoted comma is correct",
			line: `        '200': { description: 'SSE stream, or the dashboard HTML' }`,
			want: false,
		},
		{
			name: "two entries separated by a comma is normal",
			line: `            schema: { type: object, additionalProperties: true }`,
			want: false,
		},
		{
			name: "URI template braces in a key are not a flow mapping",
			line: `  /api/profile/tokens/{id}:`,
			want: false,
		},
		{
			name: "map nested in a flow sequence still checks its own commas",
			line: `      security: [{ bearerAuth: [] }]`,
			want: false,
		},
		{
			name: "split value inside a flow sequence is caught",
			line: `      security: [{ note: a, b }]`,
			want: true,
		},
		{
			name: "commas inside a quoted value are ignored",
			line: `        '400': { description: 'invalid, malformed, or unknown' }`,
			want: false,
		},
		{
			name: "escaped quote inside a double-quoted value",
			line: `        x: { description: "a \"b, c\" d" }`,
			want: false,
		},
		{
			name: "empty flow mapping",
			line: `        example: {}`,
			want: false,
		},
		{
			name: "nested flow mappings",
			line: `        x: { a: { b: 1, c: 2 }, d: 3 }`,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := hasSplitFlowMapEntry(tc.line)
			if got != tc.want {
				t.Fatalf("hasSplitFlowMapEntry(%q) = %v (%s), want %v",
					tc.line, got, detail, tc.want)
			}
		})
	}
}
