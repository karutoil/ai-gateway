package main

// Embedded OpenAPI drift guard.
//
// The binary does NOT serve the repository-root openapi.yaml. cmd/gateway
// embeds a COPY at cmd/gateway/openapi.yaml (//go:embed, see main.go) and that
// is what GET /openapi.yaml returns to clients. The two files were allowed to
// drift, and the embedded copy at one point did not even parse as YAML — so
// every client fetching the spec received a broken document while the root
// file validated cleanly in review.
//
// A CI step now diffs the two files, but that only fires on a developer
// remembering to run the openapi job. This test fails the ordinary `go test`
// run instead, which is the gate everybody actually runs locally.

import (
	"bytes"
	"os"
	"testing"
)

// TestEmbeddedOpenAPIMatchesRootSpec asserts the spec compiled into the binary
// is byte-identical to the reviewed spec at the repository root.
func TestEmbeddedOpenAPIMatchesRootSpec(t *testing.T) {
	root, err := os.ReadFile("../../openapi.yaml")
	if err != nil {
		t.Fatalf("read root openapi.yaml: %v", err)
	}
	embedded, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatalf("read embedded cmd/gateway/openapi.yaml: %v", err)
	}
	if !bytes.Equal(root, embedded) {
		t.Fatalf("cmd/gateway/openapi.yaml has drifted from openapi.yaml "+
			"(root %d bytes, embedded %d bytes).\n"+
			"The embedded copy is what GET /openapi.yaml serves to clients.\n"+
			"Fix with: cp openapi.yaml cmd/gateway/openapi.yaml",
			len(root), len(embedded))
	}
}

// TestEmbeddedOpenAPIIsServedVerbatim asserts the embed directive still points
// at the file this test compares — if someone switches main.go to embed the
// root file, the drift guard above would silently stop meaning anything.
func TestEmbeddedOpenAPIIsServedVerbatim(t *testing.T) {
	if len(embeddedOpenAPI) == 0 {
		t.Fatal("embeddedOpenAPI is empty: the //go:embed openapi.yaml directive did not resolve")
	}
	onDisk, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatalf("read cmd/gateway/openapi.yaml: %v", err)
	}
	if !bytes.Equal(embeddedOpenAPI, onDisk) {
		t.Fatal("embeddedOpenAPI does not match cmd/gateway/openapi.yaml; " +
			"the embed directive is bound to a different file than the drift guard checks")
	}
}

// TestEmbeddedOpenAPILooksLikeOpenAPI is a cheap sanity check that the embedded
// document is a plausible OpenAPI spec (correct version header, non-empty paths)
// without pulling a YAML dependency into the binary's test surface. The full
// schema validation is done by the CI `openapi` job.
func TestEmbeddedOpenAPILooksLikeOpenAPI(t *testing.T) {
	if !bytes.Contains(embeddedOpenAPI, []byte("openapi:")) {
		t.Fatal("embedded spec has no `openapi:` version key")
	}
	if !bytes.Contains(embeddedOpenAPI, []byte("paths:")) {
		t.Fatal("embedded spec has no `paths:` section")
	}
	// The YAML scanner error that shipped had an unquoted comma inside a flow
	// mapping in a response description; the symptom was that the file did not
	// parse at all. Guard the specific regression shape cheaply.
	if bytes.Contains(embeddedOpenAPI, []byte(", 400: ")) {
		t.Fatal("embedded spec contains an unquoted comma in a mapping key — invalid YAML")
	}
}
