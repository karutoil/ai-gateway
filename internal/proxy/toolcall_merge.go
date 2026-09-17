package proxy

import (
	"encoding/json"
)

// mergeFragmentedToolCalls repairs non-streaming chat completion bodies
// whose tool_calls arrive split into per-fragment entries: the first entry
// carries the id/name with empty (or partial) arguments and each following
// entry has an empty id/name with one arguments fragment:
//
//	{"id":"read_file_0#x","function":{"name":"read_file","arguments":""}},
//	{"id":"","function":{"name":"","arguments":"{"}},
//	{"id":"","function":{"name":"","arguments":"\"path\": \""}}, ...
//
// (Observed live on Cognition SWE models via a LiteLLM proxy, which leaks
// streaming-style deltas into the complete response.) Relayed verbatim,
// every standard harness reads arguments "" / "{" / ... — invalid JSON —
// and the agent loop dies with "failed to parse arguments" on the first
// tool turn, which then cascades into context loss on retries.
//
// The repair groups entries: an entry with a non-empty id starts a group;
// an entry with an empty id appends its arguments (and name, when the head
// has none yet) to the current group. Bodies without empty-id fragments
// are returned byte-identical — well-formed upstreams are untouched, and
// orphans with no group head are kept as-is rather than invented.
func mergeFragmentedToolCalls(body []byte) ([]byte, bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, false
	}
	choicesRaw, ok := top["choices"]
	if !ok {
		return body, false
	}
	var choices []json.RawMessage
	if err := json.Unmarshal(choicesRaw, &choices); err != nil || len(choices) == 0 {
		return body, false
	}
	changed := false
	for i, cRaw := range choices {
		var choice map[string]json.RawMessage
		if err := json.Unmarshal(cRaw, &choice); err != nil {
			continue
		}
		msgRaw, ok := choice["message"]
		if !ok {
			continue
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(msgRaw, &msg); err != nil {
			continue
		}
		tcRaw, ok := msg["tool_calls"]
		if !ok {
			continue
		}
		var calls []map[string]interface{}
		if err := json.Unmarshal(tcRaw, &calls); err != nil || len(calls) < 2 {
			continue
		}
		merged, did := mergeToolCallFragments(calls)
		if !did {
			continue
		}
		nb, err := json.Marshal(merged)
		if err != nil {
			continue
		}
		msg["tool_calls"] = nb
		mb, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		choice["message"] = mb
		cb, err := json.Marshal(choice)
		if err != nil {
			continue
		}
		choices[i] = cb
		changed = true
	}
	if !changed {
		return body, false
	}
	choicesOut, err := json.Marshal(choices)
	if err != nil {
		return body, false
	}
	top["choices"] = choicesOut
	out, err := json.Marshal(top)
	if err != nil {
		return body, false
	}
	return out, true
}

// mergeToolCallFragments folds empty-id fragment entries into their group
// head. Reports whether any fold happened.
func mergeToolCallFragments(calls []map[string]interface{}) ([]map[string]interface{}, bool) {
	// Fast path: no fragments, nothing to do.
	hasFragment := false
	for _, c := range calls {
		if id, _ := c["id"].(string); id == "" {
			hasFragment = true
			break
		}
	}
	if !hasFragment {
		return calls, false
	}
	var out []map[string]interface{}
	var head map[string]interface{}
	headFn := func() map[string]interface{} {
		fn, _ := head["function"].(map[string]interface{})
		if fn == nil {
			fn = map[string]interface{}{}
			head["function"] = fn
		}
		return fn
	}
	flush := func() {
		if head != nil {
			out = append(out, head)
			head = nil
		}
	}
	changed := false
	for _, c := range calls {
		id, _ := c["id"].(string)
		if id != "" {
			flush()
			head = c
			continue
		}
		// Empty-id entry: fragment of the current group, when there is one.
		if head == nil {
			out = append(out, c)
			continue
		}
		fn, _ := c["function"].(map[string]interface{})
		var fragArgs, fragName string
		if fn != nil {
			fragArgs, _ = fn["arguments"].(string)
			fragName, _ = fn["name"].(string)
		}
		// A named but id-less entry is ambiguous; only fold it when it
		// carries no identity of its own beyond fragments.
		if fragName != "" {
			hfn := headFn()
			if hn, _ := hfn["name"].(string); hn == "" {
				hfn["name"] = fragName
				changed = true
			} else if fragName != hn {
				// Different tool name without an id: keep separate rather
				// than corrupt two calls into one.
				flush()
				head = c
				continue
			}
		}
		if fragArgs != "" {
			hfn := headFn()
			prev, _ := hfn["arguments"].(string)
			hfn["arguments"] = prev + fragArgs
			changed = true
		} else if fragName != "" {
			changed = true
		}
	}
	flush()
	if !changed {
		return calls, false
	}
	return out, true
}
