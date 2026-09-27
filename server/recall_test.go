package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

func TestRenderRecall(t *testing.T) {
	long := strings.Repeat("x", 600)
	fts, both := []string{"fts"}, []string{"fts", "semantic"}
	hits := []recallHit{
		{ProjectID: "p", Keypath: "a", Content: "seen already", Sources: both},
		{ProjectID: "p", Keypath: "b", Content: long, Category: "gotcha", Sources: fts},
		{ProjectID: "q", Keypath: "c", Content: "  short  ", Sources: fts},
		{ProjectID: "p", Keypath: "d", Content: "fourth", Sources: both},
		{ProjectID: "p", Keypath: "e", Content: "fifth", Sources: both},
	}
	seen := map[string]bool{"p:a": true}
	text, shown := renderRecall(hits, seen, 3, 500)
	if want := "p:b,q:c,p:d"; strings.Join(shown, ",") != want {
		t.Fatalf("shown = %v want %v", shown, want)
	}
	// A hit below the cap fills a freed slot only with a semantic source.
	hits[3].Sources = fts
	if _, shown := renderRecall(hits, seen, 3, 500); strings.Join(shown, ",") != "p:b,q:c,p:e" {
		t.Fatalf("fts-only backfill must be skipped, semantic backfill taken: shown = %v", shown)
	}
	hits[4].Sources = fts
	if _, shown := renderRecall(hits, seen, 3, 500); strings.Join(shown, ",") != "p:b,q:c" {
		t.Fatalf("no semantic candidates below the cap: shown = %v", shown)
	}
	hits[3].Sources = both
	if !strings.HasPrefix(text, "<memstate-recall>\n") ||
		!strings.HasSuffix(text, "</memstate-recall>\n") {
		t.Fatalf("block markers missing:\n%s", text)
	}
	if strings.Contains(text, "seen already") || strings.Contains(text, "fifth") {
		t.Fatalf("seen or over-cap hit leaked:\n%s", text)
	}
	if !strings.Contains(text, "### p:b [gotcha]\n"+strings.Repeat("x", 500)+"[…truncated]\n") {
		t.Fatalf("truncation or category header wrong:\n%s", text)
	}
	if !strings.Contains(text, "### q:c\nshort\n") {
		t.Fatalf("content should be trimmed:\n%s", text)
	}
	// The same keypath in another project is a different memory.
	if _, shown := renderRecall([]recallHit{{ProjectID: "q", Keypath: "a", Sources: both}}, seen, 3, 500); strings.Join(shown, ",") != "q:a" {
		t.Fatalf("a seen keypath of one project must not hide another project's: shown = %v", shown)
	}
	if text, shown := renderRecall(hits[:1], seen, 3, 500); text != "" || shown != nil {
		t.Fatalf("all-seen must render nothing, got %q %v", text, shown)
	}
}

func TestRenderRecallProperties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		id := rapid.StringMatching(`[a-c]`)
		hits := rapid.SliceOf(rapid.Custom(func(t *rapid.T) recallHit {
			return recallHit{
				ProjectID: id.Draw(t, "project"),
				Keypath:   id.Draw(t, "keypath"),
				Content:   rapid.String().Draw(t, "content"),
				Sources:   rapid.SampledFrom([][]string{{"fts"}, {"fts", "semantic"}}).Draw(t, "sources"),
			}
		})).Draw(t, "hits")
		seen := map[string]bool{}
		for _, key := range rapid.SliceOf(rapid.StringMatching(`[a-c]:[a-c]`)).Draw(t, "seen") {
			seen[key] = true
		}
		maxHits := rapid.IntRange(1, 5).Draw(t, "maxHits")
		text, shown := renderRecall(hits, seen, maxHits, 50)
		if len(shown) > maxHits {
			t.Fatalf("shown %d hits, cap is %d", len(shown), maxHits)
		}
		if (text == "") != (len(shown) == 0) {
			t.Fatalf("text and shown disagree: %q %v", text, shown)
		}
		for _, key := range shown {
			if seen[key] {
				t.Fatalf("seen key %q shown again", key)
			}
		}
	})
}

func TestRunRecallEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEMSTATE_DB", filepath.Join(dir, "t.db"))
	t.Setenv("MEMSTATE_NO_RECALL", "")
	t.Setenv("MEMSTATE_RECALL_DEBUG", "")
	ts := newTestServer(t)
	t.Setenv("MEMSTATE_ADDR", strings.TrimPrefix(ts.URL, "http://"))

	// The folder name matches no project: recall must not depend on it.
	cwd := filepath.Join(t.TempDir(), "unrelated-folder")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "recall_proj", "keypath": "gotchas.timeout",
		"content": "the embed timeout must cover a cold model load", "category": "gotcha",
	})
	postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "other_proj", "keypath": "notes.cold",
		"content": "a cold model load takes a long embed timeout",
	})

	run := func(event string) string {
		var out bytes.Buffer
		if code := runRecall(strings.NewReader(event), &out); code != 0 {
			t.Fatalf("runRecall exit %d", code)
		}
		return out.String()
	}
	event := `{"session_id":"s1","cwd":` + jsonString(cwd) +
		`,"prompt":"why does the embed timeout matter for cold loads"}`

	first := run(event)
	if !strings.Contains(first, "<memstate-recall>") ||
		!strings.Contains(first, "### recall_proj:gotchas.timeout [gotcha]") ||
		!strings.Contains(first, "### other_proj:notes.cold") {
		t.Fatalf("first prompt should inject the hits of both projects, got:\n%s", first)
	}
	if second := run(event); second != "" {
		t.Fatalf("same session must not repeat a keypath, got:\n%s", second)
	}
	other := strings.Replace(event, `"s1"`, `"s2"`, 1)
	if third := run(other); !strings.Contains(third, "gotchas.timeout") {
		t.Fatalf("a new session starts with an empty seen set, got:\n%s", third)
	}

	short := `{"session_id":"s3","cwd":` + jsonString(cwd) + `,"prompt":"yes do it"}`
	if got := run(short); got != "" {
		t.Fatalf("three-word prompt must print nothing, got %q", got)
	}
	t.Setenv("MEMSTATE_NO_RECALL", "1")
	if got := run(strings.Replace(event, `"s1"`, `"s4"`, 1)); got != "" {
		t.Fatalf("MEMSTATE_NO_RECALL must print nothing, got %q", got)
	}
	t.Setenv("MEMSTATE_NO_RECALL", "")

	// Unreachable daemon: silent exit 0.
	t.Setenv("MEMSTATE_ADDR", "127.0.0.1:9")
	if got := run(strings.Replace(event, `"s1"`, `"s5"`, 1)); got != "" {
		t.Fatalf("unreachable daemon must print nothing, got %q", got)
	}

	// Seen files older than the TTL are pruned on the next run.
	old := filepath.Join(dir, "recall", "ancient")
	if err := os.WriteFile(old, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMSTATE_ADDR", strings.TrimPrefix(ts.URL, "http://"))
	run(strings.Replace(event, `"s1"`, `"s6"`, 1))
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("stale seen file should be pruned, stat err=%v", err)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
