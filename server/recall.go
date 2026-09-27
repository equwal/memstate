package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// `memstated recall` is the UserPromptSubmit hook for Claude Code. It reads
// the hook event on stdin, searches the project's memories with the prompt
// text, and prints the best unseen hits so they land in the model's context
// before it answers. Every failure path exits 0 with nothing on stdout: a
// hook that fails must never block the prompt.

const (
	recallMinWords  = 4   // shorter prompts ("yes", "continue") carry no topic
	recallMaxHits   = 3   // hits printed per prompt
	recallMaxChars  = 500 // content cut per hit
	recallSearchLim = 10  // candidates fetched, so dedupe still leaves hits
	recallTimeout   = 3 * time.Second
	recallSeenTTL   = 7 * 24 * time.Hour
)

type hookEvent struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Prompt    string `json:"prompt"`
}

type recallHit struct {
	Keypath  string   `json:"keypath"`
	Content  string   `json:"content"`
	Category string   `json:"category"`
	Sources  []string `json:"sources"`
}

var (
	slugRE      = regexp.MustCompile(`[^a-z0-9]+`)
	sessionIDRE = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
)

func cmdRecall(args []string) int {
	return runRecall(os.Stdin, os.Stdout)
}

// runRecall is cmdRecall with injectable streams for tests.
func runRecall(stdin io.Reader, stdout io.Writer) int {
	debug := func(format string, a ...any) {
		if os.Getenv("MEMSTATE_RECALL_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "memstated recall: "+format+"\n", a...)
		}
	}
	if os.Getenv("MEMSTATE_NO_RECALL") != "" {
		return 0
	}
	var ev hookEvent
	if err := json.NewDecoder(stdin).Decode(&ev); err != nil {
		debug("decode hook event: %v", err)
		return 0
	}
	if !recallEligible(ev.Prompt) {
		return 0
	}
	addr, ok := discoverAddr()
	if !ok {
		debug("no shared daemon found")
		return 0
	}
	project := deriveProject(ev.Cwd)
	hits, err := recallSearch(addr, project, ev.Prompt)
	if err != nil {
		debug("search: %v", err)
		return 0
	}
	// The user scope is a bonus: a failure there must not hide project hits.
	userHits, err := recallSearch(addr, userProject, ev.Prompt)
	if err != nil {
		debug("user scope search: %v", err)
		userHits = nil
	}
	userHits = filterHostHits(userHits, hostSlug())
	seenPath := recallSeenPath(ev.SessionID)
	seen := loadSeen(seenPath)
	text, shown := renderRecall(project, hits, userHits, seen, recallMaxHits, recallMaxChars)
	if text == "" {
		return 0
	}
	fmt.Fprint(stdout, text)
	if err := appendSeen(seenPath, shown); err != nil {
		debug("record seen: %v", err)
	}
	pruneSeen(filepath.Dir(seenPath), recallSeenTTL)
	return 0
}

// recallEligible reports whether a prompt carries enough words to search on.
func recallEligible(prompt string) bool {
	return len(strings.Fields(prompt)) >= recallMinWords
}

// deriveProject maps a working directory to a project id with the same rule
// as the TS proxy and the Python skill: the git repository name (the
// directory name outside a repository), lowercased, with every run of
// characters outside [a-z0-9] replaced by "_" and edge underscores trimmed.
func deriveProject(cwd string) string {
	base := ""
	if cwd != "" {
		out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
		if err == nil {
			base = filepath.Base(strings.TrimSpace(string(out)))
		}
	}
	if base == "" {
		base = filepath.Base(cwd)
	}
	return slugProject(base)
}

func slugProject(name string) string {
	s := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(name), "_"), "_")
	if s == "" {
		return "default"
	}
	return s
}

// recallSearch runs a hybrid search on the daemon at addr. Any non-200
// reply is an error, including "unknown mode" from a daemon that predates
// hybrid search.
func recallSearch(addr, project, prompt string) ([]recallHit, error) {
	body, _ := json.Marshal(map[string]any{
		"query":      prompt,
		"project_id": project,
		"mode":       "hybrid",
		"limit":      recallSearchLim,
	})
	client := &http.Client{Timeout: recallTimeout}
	resp, err := client.Post("http://"+addr+"/api/v1/memories/search",
		"application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out struct {
		Results []recallHit `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// userSeenPrefix scopes seen-file keys for user-scope hits, so a project
// keypath with the same name is not suppressed by them.
const userSeenPrefix = userProject + "/"

// filterHostHits drops user-scope hits that describe another machine:
// anything under host.<slug> where slug is not this host.
func filterHostHits(hits []recallHit, host string) []recallHit {
	out := hits[:0:0]
	for _, h := range hits {
		if !isOtherHost(h.Keypath, host) {
			out = append(out, h)
		}
	}
	return out
}

// pickRecall selects up to max unseen hits. Hits ranked below max fill the
// slots that seen hits free up, but only when the semantic side returned
// them: an FTS-only hit that deep is a common-word match, not a topic match.
func pickRecall(hits []recallHit, seen map[string]bool, seenPrefix string, max int) []recallHit {
	var out []recallHit
	for i, h := range hits {
		if len(out) == max {
			break
		}
		if seen[seenPrefix+h.Keypath] {
			continue
		}
		if i >= max && !slices.Contains(h.Sources, "semantic") {
			continue
		}
		out = append(out, h)
	}
	return out
}

// renderRecall formats up to maxHits hits from the project and the user
// scope. One slot is reserved for the best user-scope hit, marked [user];
// project hits fill the rest. It returns the block and the seen-file keys it
// printed. An empty block means nothing new to show.
func renderRecall(project string, hits, userHits []recallHit, seen map[string]bool, maxHits, maxChars int) (string, []string) {
	user := pickRecall(userHits, seen, userSeenPrefix, 1)
	proj := pickRecall(hits, seen, "", maxHits-len(user))
	if len(user)+len(proj) == 0 {
		return "", nil
	}
	var b strings.Builder
	var shown []string
	fmt.Fprintf(&b, "<memstate-recall project=%q>\n", project)
	b.WriteString("Memories related to this prompt. Call memstate_get(keypath) for full content.\n\n")
	write := func(h recallHit, marker, seenKey string) {
		fmt.Fprintf(&b, "### %s%s", h.Keypath, marker)
		if h.Category != "" {
			fmt.Fprintf(&b, " [%s]", h.Category)
		}
		b.WriteString("\n")
		b.WriteString(cutRunes(strings.TrimSpace(h.Content), maxChars))
		b.WriteString("\n\n")
		shown = append(shown, seenKey)
	}
	for _, h := range proj {
		write(h, "", h.Keypath)
	}
	for _, h := range user {
		write(h, " [user]", userSeenPrefix+h.Keypath)
	}
	b.WriteString("</memstate-recall>\n")
	return b.String(), shown
}

// cutRunes shortens s to max runes and marks the cut.
func cutRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "[…truncated]"
}

// recallSeenPath is the per-session file of keypaths already injected,
// kept next to the database so MEMSTATE_DB moves it too.
func recallSeenPath(sessionID string) string {
	id := sessionIDRE.ReplaceAllString(sessionID, "")
	if id == "" {
		id = "no_session"
	}
	return filepath.Join(filepath.Dir(defaultDBPath()), "recall", id)
}

func loadSeen(path string) map[string]bool {
	seen := map[string]bool{}
	b, err := os.ReadFile(path)
	if err != nil {
		return seen
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			seen[line] = true
		}
	}
	return seen
}

func appendSeen(path string, keypaths []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(keypaths, "\n") + "\n")
	return err
}

// pruneSeen deletes seen files older than ttl so finished sessions do not
// accumulate forever.
func pruneSeen(dir string, ttl time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-ttl)
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && !e.IsDir() && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
