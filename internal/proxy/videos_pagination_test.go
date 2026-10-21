package proxy

// Pagination, ownership and lifecycle guards for GET /v1/videos.
//
// The list endpoint used to be a hard `LIMIT 100` with no cursor: a key with
// more than 100 jobs silently lost access to everything older, and there was no
// way to discover that it had happened. These tests pin the OpenAI list
// convention (`limit`, `after`, `has_more`, `last_id`).

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

type videoListPage struct {
	Object  string `json:"object"`
	HasMore bool   `json:"has_more"`
	LastID  string `json:"last_id"`
	Data    []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"data"`
}

func seedVideoJobs(t *testing.T, database *sql.DB, prefix string, n int) {
	t.Helper()
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("video_page_%02d", i)
		// Distinct, strictly increasing created_at so ordering is unambiguous.
		// Seeded via a parameterised time.Time (the shape insertVideoJob uses)
		// AND, for odd indexes, via SQL datetime() to reproduce the space-separated
		// text format that a hand-loaded or SQL-written row carries. Both formats
		// must page correctly — comparing a Go time.Time against the SQL-written
		// form silently re-emits the cursor row (see the row-value note in
		// VideoList).
		ts := base.Add(time.Duration(i) * time.Second)
		if i%2 == 1 {
			if _, err := database.Exec(`INSERT INTO video_jobs
				(id, provider_id, key_prefix, dialect, model, upstream_id, prompt, seconds, size, status, progress, created_at, updated_at)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,datetime(?),datetime(?))`,
				id, "prov-page", prefix, "openai", "sora-2", "up-"+id, "p", "4", "1280x720",
				"completed", 100, ts.Format("2006-01-02 15:04:05"), ts.Format("2006-01-02 15:04:05")); err != nil {
				t.Fatalf("seed job %d: %v", i, err)
			}
			continue
		}
		if _, err := database.Exec(`INSERT INTO video_jobs
			(id, provider_id, key_prefix, dialect, model, upstream_id, prompt, seconds, size, status, progress, created_at, updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, "prov-page", prefix, "openai", "sora-2", "up-"+id, "p", "4", "1280x720",
			"completed", 100, ts, ts); err != nil {
			t.Fatalf("seed job %d: %v", i, err)
		}
	}
}

// TestVideoListPaginates verifies limit/after walk the whole set exactly once,
// newest first, and that has_more is truthful on the final page.
func TestVideoListPaginates(t *testing.T) {
	srv, database, key, _ := videoServer(t)
	prefix := keyPrefixFor(key)
	seedVideoJobs(t, database, prefix, 7)

	var seen []string
	cursor := ""
	for page := 0; page < 10; page++ { // bounded: a cycle must fail, not hang
		url := srv.URL + "/v1/videos?limit=3"
		if cursor != "" {
			url += "&after=" + cursor
		}
		resp, body := doReq(t, "GET", url, key, "")
		if resp.StatusCode != 200 {
			t.Fatalf("page %d: status %d body=%s", page, resp.StatusCode, body)
		}
		var p videoListPage
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatalf("page %d: bad JSON: %v (%s)", page, err, body)
		}
		if len(p.Data) > 3 {
			t.Fatalf("page %d returned %d items, limit was 3", page, len(p.Data))
		}
		for _, d := range p.Data {
			seen = append(seen, d.ID)
		}
		if !p.HasMore {
			break
		}
		if p.LastID == "" {
			t.Fatalf("page %d: has_more=true but no last_id cursor", page)
		}
		cursor = p.LastID
	}

	if len(seen) != 7 {
		t.Fatalf("walked %d jobs, want 7 (ids=%v)", len(seen), seen)
	}
	// No duplicates: a broken cursor would repeat a row forever.
	uniq := map[string]bool{}
	for _, id := range seen {
		if uniq[id] {
			t.Fatalf("duplicate job %q across pages: %v", id, seen)
		}
		uniq[id] = true
	}
	// Newest first: video_page_06 was seeded last with the largest created_at.
	if seen[0] != "video_page_06" {
		t.Errorf("first page must start with the newest job (video_page_06), got %q", seen[0])
	}
	if seen[len(seen)-1] != "video_page_00" {
		t.Errorf("last page must end with the oldest job (video_page_00), got %q", seen[len(seen)-1])
	}
}

// TestVideoListRejectsBadPagination: a malformed limit or a foreign cursor is a
// 400, not a silent full dump.
func TestVideoListRejectsBadPagination(t *testing.T) {
	srv, _, key, _ := videoServer(t)

	for _, q := range []string{"?limit=0", "?limit=abc", "?limit=-5"} {
		resp, body := doReq(t, "GET", srv.URL+"/v1/videos"+q, key, "")
		if resp.StatusCode != 400 {
			t.Errorf("%s: status %d, want 400 (%s)", q, resp.StatusCode, body)
		}
	}

	// A cursor that is not one of this key's jobs must not be accepted, or a
	// client could probe for another key's job ids.
	resp, body := doReq(t, "GET", srv.URL+"/v1/videos?after=video_nonexistent", key, "")
	if resp.StatusCode != 400 {
		t.Errorf("foreign cursor: status %d, want 400 (%s)", resp.StatusCode, body)
	}
}

// TestVideoListClampsLimit: an over-large limit is clamped rather than rejected
// or honoured, so a client cannot ask for an unbounded response.
func TestVideoListClampsLimit(t *testing.T) {
	srv, database, key, _ := videoServer(t)
	prefix := keyPrefixFor(key)
	seedVideoJobs(t, database, prefix, 3)

	resp, body := doReq(t, "GET", srv.URL+"/v1/videos?limit=100000", key, "")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var p videoListPage
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(p.Data) != 3 {
		t.Fatalf("got %d jobs, want 3", len(p.Data))
	}
	if p.HasMore {
		t.Error("has_more must be false when the set is exhausted")
	}
}

// TestVideoListEmptyShape: a key with no jobs still gets the documented object
// shape (data, not null).
func TestVideoListEmptyShape(t *testing.T) {
	srv, _, key, _ := videoServer(t)
	resp, body := doReq(t, "GET", srv.URL+"/v1/videos", key, "")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var p videoListPage
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if p.Object != "list" {
		t.Errorf("object = %q, want list", p.Object)
	}
	if p.HasMore {
		t.Error("empty list must report has_more=false")
	}
	if p.LastID != "" {
		t.Errorf("empty list must omit last_id, got %q", p.LastID)
	}
}

// TestVideoJobUnownedRowsAreNotVisible is the P3-11 guard.
//
// video_jobs.key_prefix was nullable and the ownership check treated an empty
// prefix on either side as visible. Migration 031 backfills legacy NULL/empty
// rows to the literal 'unowned'; the Go check denies an empty caller prefix
// outright. Both halves matter: the schema backfill alone would still leave the
// caller side fail-open, and the Go check alone would leave orphan rows
// ambiguous.
func TestVideoJobUnownedRowsAreNotVisible(t *testing.T) {
	srv, database, key, _ := videoServer(t)
	ts := time.Now().UTC()

	// An orphan row with an empty prefix (the pre-031 shape).
	if _, err := database.Exec(`INSERT INTO video_jobs
		(id, provider_id, key_prefix, dialect, model, upstream_id, prompt, status, progress, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		"video_orphan", "prov-x", "", "openai", "sora-2", "up-orphan", "secret prompt",
		"completed", 100, ts, ts); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	// A caller with a real key must not see it.
	resp, body := doReq(t, "GET", srv.URL+"/v1/videos/video_orphan", key, "")
	if resp.StatusCode != 404 {
		t.Errorf("GET orphan with a real key = %d, want 404 (%s)", resp.StatusCode, body)
	}
	resp, body = doReq(t, "DELETE", srv.URL+"/v1/videos/video_orphan", key, "")
	if resp.StatusCode != 404 {
		t.Errorf("DELETE orphan with a real key = %d, want 404 (%s)", resp.StatusCode, body)
	}

	// A caller with NO prefix must also not see it (the fail-open case).
	resp, body = doReqAs(t, "GET", srv.URL+"/v1/videos/video_orphan", key, "", "")
	if resp.StatusCode != 404 {
		t.Errorf("GET orphan with an empty prefix = %d, want 404 — empty must deny, not allow all (%s)", resp.StatusCode, body)
	}

	// And it must not leak into the list.
	resp, body = doReq(t, "GET", srv.URL+"/v1/videos", key, "")
	if resp.StatusCode != 200 {
		t.Fatalf("list status %d: %s", resp.StatusCode, body)
	}
	var p videoListPage
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	for _, d := range p.Data {
		if d.ID == "video_orphan" {
			t.Error("an unowned job must not appear in a key's list")
		}
	}

	// The row still exists (we must not have destroyed data).
	var cnt int
	if err := database.QueryRow(`SELECT COUNT(*) FROM video_jobs WHERE id=?`, "video_orphan").Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Error("the orphan row must be denied, not deleted")
	}
}
