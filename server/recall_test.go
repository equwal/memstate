package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

func TestSlugProjectProperties(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z0-9_]+$`)
	rapid.Check(t, func(t *rapid.T) {
		name := rapid.String().Draw(t, "name")
		s := slugProject(name)
		if !valid.MatchString(s) {
			t.Fatalf("slug %q of %q has characters outside [a-z0-9_]", s, name)
		}
		if strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") {
			t.Fatalf("slug %q of %q has an edge underscore", s, name)
		}
		if again := slugProject(s); again != s {
			t.Fatalf("slug is not idempotent: %q -> %q", s, again)
		}
	})
}

func TestDeriveProject(t *testing.T) {
	// Non-repository directory: its own name, slugged.
	plain := filepath.Join(t.TempDir(), "My-App v2")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := deriveProject(plain); got != "my_app_v2" {
		t.Fatalf("plain dir: got %q want my_app_v2", got)
	}
	if got := deriveProject(""); got != "default" {
		t.Fatalf("empty cwd: got %q want default", got)
	}

	// Repository: the top-level name wins even from a nested directory.
	repo := filepath.Join(t.TempDir(), "Repo.Name")
	nested := filepath.Join(repo, "sub", "dir")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init unavailable: %v %s", err, out)
	}
	if got := deriveProject(nested); got != "repo_name" {
		t.Fatalf("nested repo dir: got %q want repo_name", got)
	}
}

func TestRenderRecall(t *testing.T) {
	long := strings.Repeat("x", 600)
	fts, both := []string{"fts"}, []string{"fts", "semantic"}
	hits := []recallHit{
		{Keypath: "a", Content: "seen already", Sources: both},
		{Keypath: "b", Content: long, Category: "gotcha", Sources: fts},
		{Keypath: "c", Content: "  short  ", Sources: fts},
		{Keypath: "d", Content: "fourth", Sources: both},
		{Keypath: "e", Content: "fifth", Sources: both},
	}
	text, shown := renderRecall("proj", hits, nil, map[string]bool{"a": true}, 3, 500)
	if want := []string{"b", "c", "d"}; strings.Join(shown, ",") != strings.Join(want, ",") {
		t.Fatalf("shown = %v want %v", shown, want)
	}
	// A hit below the cap fills a freed slot only with a semantic source.
	hits[3].Sources = fts
	if _, shown := renderRecall("proj", hits, nil, map[string]bool{"a": true}, 3, 500); strings.Join(shown, ",") != "b,c,e" {
		t.Fatalf("fts-only backfill must be skipped, semantic backfill taken: shown = %v", shown)
	}
	hits[4].Sources = fts
	if _, shown := renderRecall("proj", hits, nil, map[string]bool{"a": true}, 3, 500); strings.Join(shown, ",") != "b,c" {
		t.Fatalf("no semantic candidates below the cap: shown = %v", shown)
	}
	hits[3].Sources = both
	if !strings.HasPrefix(text, "<memstate-recall project=\"proj\">\n") ||
		!strings.HasSuffix(text, "</memstate-recall>\n") {
		t.Fatalf("block markers missing:\n%s", text)
	}
	if strings.Contains(text, "seen already") || strings.Contains(text, "fifth") {
		t.Fatalf("seen or over-cap hit leaked:\n%s", text)
	}
	if !strings.Contains(text, "### b [gotcha]\n"+strings.Repeat("x", 500)+"[…truncated]\n") {
		t.Fatalf("truncation or category header wrong:\n%s", text)
	}
	if !strings.Contains(text, "### c\nshort\n") {
		t.Fatalf("content should be trimmed:\n%s", text)
	}
	if text, shown := renderRecall("proj", hits[:1], nil, map[string]bool{"a": true}, 3, 500); text != "" || shown != nil {
		t.Fatalf("all-seen must render nothing, got %q %v", text, shown)
	}
}

func TestRunRecallEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEMSTATE_DB", filepath.Join(dir, "t.db"))
	t.Setenv("MEMSTATE_NO_RECALL", "")
	t.Setenv("MEMSTATE_RECALL_DEBUG", "")
	ts := newTestServer(t)
	t.Setenv("MEMSTATE_ADDR", strings.TrimPrefix(ts.URL, "http://"))

	cwd := filepath.Join(t.TempDir(), "recall-proj")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "recall_proj", "keypath": "gotchas.timeout",
		"content": "the embed timeout must cover a cold model load", "category": "gotcha",
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
	if !strings.Contains(first, `<memstate-recall project="recall_proj">`) ||
		!strings.Contains(first, "### gotchas.timeout [gotcha]") {
		t.Fatalf("first prompt should inject the hit, got:\n%s", first)
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

func TestRenderRecallUserScope(t *testing.T) {
	both := []string{"fts", "semantic"}
	hits := []recallHit{
		{Keypath: "p1", Content: "project one", Sources: both},
		{Keypath: "p2", Content: "project two", Sources: both},
		{Keypath: "p3", Content: "project three", Sources: both},
	}
	user := []recallHit{
		{Keypath: "preferences.commit_style", Content: "no trailers", Category: "config", Sources: both},
		{Keypath: "profile.role", Content: "backend", Sources: both},
	}
	text, shown := renderRecall("proj", hits, user, map[string]bool{}, 3, 500)
	// One slot goes to the user scope, the rest to the project.
	if want := "p1,p2,_user/preferences.commit_style"; strings.Join(shown, ",") != want {
		t.Fatalf("shown = %v want %s", shown, want)
	}
	if !strings.Contains(text, "### preferences.commit_style [user] [config]\nno trailers\n") {
		t.Fatalf("user hit marker missing:\n%s", text)
	}
	if strings.Contains(text, "profile.role") || strings.Contains(text, "project three") {
		t.Fatalf("second user hit or fourth hit leaked:\n%s", text)
	}
	// A seen user hit frees its slot for the next user hit.
	seen := map[string]bool{"_user/preferences.commit_style": true}
	if _, shown := renderRecall("proj", hits, user, seen, 3, 500); strings.Join(shown, ",") != "p1,p2,_user/profile.role" {
		t.Fatalf("shown = %v", shown)
	}
	// A project keypath equal to a user keypath is not suppressed by it.
	seen = map[string]bool{"preferences.commit_style": true}
	if _, shown := renderRecall("proj", hits, user, seen, 3, 500); strings.Join(shown, ",") != "p1,p2,_user/preferences.commit_style" {
		t.Fatalf("seen keys must be scoped: shown = %v", shown)
	}
	// User hits alone still render.
	if text, _ := renderRecall("proj", nil, user, map[string]bool{}, 3, 500); !strings.Contains(text, "### preferences.commit_style [user]") {
		t.Fatalf("user-only render:\n%s", text)
	}
}

func TestFilterHostHits(t *testing.T) {
	hits := []recallHit{
		{Keypath: "preferences.x"},
		{Keypath: "host.mbp.env.go_bin"},
		{Keypath: "host.other.env.go_bin"},
		{Keypath: "host"},
	}
	got := filterHostHits(hits, "mbp")
	if len(got) != 3 || got[0].Keypath != "preferences.x" || got[1].Keypath != "host.mbp.env.go_bin" || got[2].Keypath != "host" {
		t.Fatalf("got %+v", got)
	}
}

func TestRunRecallUserScope(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEMSTATE_DB", filepath.Join(dir, "t.db"))
	t.Setenv("MEMSTATE_NO_RECALL", "")
	ts := newTestServer(t)
	t.Setenv("MEMSTATE_ADDR", strings.TrimPrefix(ts.URL, "http://"))

	cwd := filepath.Join(t.TempDir(), "scope-proj")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := func(kp, content string) {
		t.Helper()
		code, out := postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
			"project_id": userProject, "keypath": kp, "content": content,
		})
		if code != 200 {
			t.Fatalf("seed %s: %d %v", kp, code, out)
		}
	}
	seed("preferences.commit_style", "the user wants short commit subjects and no trailers")
	seed("host."+hostSlug()+".env.go_bin", "commit binaries of go live in GOBIN under the home dir")
	seed("host.other_box.env.go_bin", "commit binaries of go live in /opt on the other box")

	var out bytes.Buffer
	event := `{"session_id":"u1","cwd":` + jsonString(cwd) +
		`,"prompt":"what commit style does the user want for go binaries"}`
	if code := runRecall(strings.NewReader(event), &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	text := out.String()
	if !strings.Contains(text, "[user]") {
		t.Fatalf("user hit missing:\n%s", text)
	}
	if strings.Contains(text, "other_box") {
		t.Fatalf("other host leaked:\n%s", text)
	}
	if strings.Count(text, "[user]") != 1 {
		t.Fatalf("more than one user slot:\n%s", text)
	}
}
