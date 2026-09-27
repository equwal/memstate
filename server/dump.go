package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ANSI rendering shared by the memstate CLI verbs (cli.go). dump/search
// survive here only as aliases of those verbs.

// ---------- ANSI rendering ----------

// palette wraps text in ANSI escapes when enabled. Colors stay in the basic
// 16-color range so they respect the user's terminal theme.
type palette struct{ on bool }

func (p palette) wrap(code, s string) string {
	if !p.on || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// wrapPair uses a targeted off-code instead of the full \x1b[0m reset so
// spans can nest: inline code inside a bold span restores the default
// foreground ([39m) without cancelling bold, and vice versa ([22m).
func (p palette) wrapPair(on, off, s string) string {
	if !p.on || s == "" {
		return s
	}
	return "\x1b[" + on + "m" + s + "\x1b[" + off + "m"
}

func (p palette) boldSpan(s string) string { return p.wrapPair("1", "22", s) }
func (p palette) codeSpan(s string) string { return p.wrapPair("36", "39", s) }

func (p palette) bold(s string) string    { return p.wrap("1", s) }
func (p palette) dim(s string) string     { return p.wrap("2", s) }
func (p palette) cyan(s string) string    { return p.wrap("36", s) }
func (p palette) green(s string) string   { return p.wrap("32", s) }
func (p palette) yellow(s string) string  { return p.wrap("33", s) }
func (p palette) blue(s string) string    { return p.wrap("34", s) }
func (p palette) magenta(s string) string { return p.wrap("35", s) }

func newPalette(noColorFlag bool) palette {
	if noColorFlag || os.Getenv("NO_COLOR") != "" {
		return palette{}
	}
	fi, err := os.Stdout.Stat()
	return palette{on: err == nil && fi.Mode()&os.ModeCharDevice != 0}
}

var (
	inlineCodeRe = regexp.MustCompile("`[^`]+`")
	boldSpanRe   = regexp.MustCompile(`\*\*[^*]+\*\*`)
	listMarkerRe = regexp.MustCompile(`^(\s*)([-*+]|\d+\.)(\s+)`)
	mdHeadingRe  = regexp.MustCompile(`^#{1,6}\s`)
	fenceRe      = regexp.MustCompile("^(```|~~~)")
)

// renderMarkdown gives stored content a light syntax-highlighting pass:
// headings, fenced code blocks, inline code, bold spans, list markers,
// blockquotes. Line-based on purpose — content is agent-written markdown,
// not something worth a full parser.
func renderMarkdown(content string, indent string, p palette) string {
	var b strings.Builder
	fence := "" // opening marker when inside a fenced block, else ""
	for _, line := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		b.WriteString(indent)
		trimmed := strings.TrimSpace(line)
		switch {
		case fence == "" && fenceRe.MatchString(trimmed):
			fence = trimmed[:3]
			b.WriteString(p.dim(line))
		case fence != "" && strings.HasPrefix(trimmed, fence):
			// Only the matching marker closes the block — a ~~~ line inside
			// a ``` fence is content, not a toggle.
			fence = ""
			b.WriteString(p.dim(line))
		case fence != "":
			b.WriteString(p.green(line))
		case mdHeadingRe.MatchString(line):
			b.WriteString(p.bold(p.magenta(line)))
		case strings.HasPrefix(trimmed, ">"):
			b.WriteString(p.dim(line))
		default:
			if m := listMarkerRe.FindStringSubmatch(line); m != nil {
				b.WriteString(m[1] + p.yellow(m[2]) + m[3])
				line = line[len(m[0]):]
			}
			line = inlineCodeRe.ReplaceAllStringFunc(line, p.codeSpan)
			line = boldSpanRe.ReplaceAllStringFunc(line, p.boldSpan)
			b.WriteString(line)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// entryMeta builds the dim metadata suffix: version, category, topics, date.
func entryMeta(m *Memory) string {
	parts := []string{fmt.Sprintf("v%d", m.Version)}
	if m.Category != "" {
		parts = append(parts, m.Category)
	}
	if len(m.Topics) > 0 {
		parts = append(parts, strings.Join(m.Topics, ", "))
	}
	parts = append(parts, time.Unix(m.CreatedAt, 0).Format("2006-01-02 15:04"))
	return strings.Join(parts, " · ")
}

func printEntry(w io.Writer, m *Memory, showProject bool, p palette) {
	kp := m.Keypath
	if showProject {
		kp = m.ProjectID + ":" + kp
	}
	fmt.Fprintf(w, "%s  %s\n", p.bold(p.cyan(kp)), p.dim(entryMeta(m)))
	fmt.Fprint(w, renderMarkdown(m.Content, "  ", p))
	fmt.Fprintln(w)
}

// printKeyTree renders sorted keypaths as an indented tree. A segment that
// only exists as a prefix of deeper keys prints as a branch; a segment with
// its own row prints as a leaf with metadata.
func printKeyTree(w io.Writer, mems []*Memory, p palette) {
	var prev []string
	for _, m := range mems {
		parts := strings.Split(m.Keypath, ".")
		common := 0
		for common < len(parts)-1 && common < len(prev) && parts[common] == prev[common] {
			common++
		}
		for i := common; i < len(parts)-1; i++ {
			fmt.Fprintf(w, "%s%s\n", strings.Repeat("  ", i), p.bold(p.blue(parts[i])))
		}
		fmt.Fprintf(w, "%s%s  %s\n",
			strings.Repeat("  ", len(parts)-1),
			p.cyan(parts[len(parts)-1]),
			p.dim(entryMeta(m)))
		prev = parts
	}
}

// projectHint appends the live project list to a not-found error so the user
// doesn't need a second command to recover.
func projectHint(store *Store) string {
	ps, err := store.ListProjects()
	if err != nil || len(ps) == 0 {
		return ""
	}
	ids := make([]string, len(ps))
	for i, pr := range ps {
		ids[i] = pr.ID
	}
	return "live projects: " + strings.Join(ids, ", ")
}

// flagAfterPositional catches "dump PROJECT --keys": stdlib flag parsing
// stops at the first positional arg, so a trailing flag would silently be
// taken as a keypath or query term. Reject it with a usage hint instead.
// An explicit "--" terminator in the raw args opts out, so hyphen-leading
// terms stay expressible: memstated search -- --idle-timeout
func flagAfterPositional(raw, positionals []string) bool {
	if slices.Contains(raw, "--") {
		return false
	}
	for _, a := range positionals {
		if strings.HasPrefix(a, "-") && a != "-" {
			return true
		}
	}
	return false
}

// ---------- legacy subcommands ----------

// cmdDump keeps `memstated dump [--keys] PROJECT [KEYPATH]` working as an
// alias of `memstate get --project PROJECT [KEYPATH]` (or `memstate tree`
// with --keys).
func cmdDump(args []string) int {
	fs := flag.NewFlagSet("dump", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	db := fs.String("db", "", "")
	keys := fs.Bool("keys", false, "")
	noColor := fs.Bool("no-color", false, "")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) < 1 || len(pos) > 2 {
		fmt.Fprintln(os.Stderr, "usage: memstated dump [--keys] [--db PATH] PROJECT [KEYPATH]")
		return 2
	}
	verb := "get"
	if *keys {
		verb = "tree"
	}
	cli := []string{verb}
	if len(pos) == 2 {
		cli = append(cli, pos[1])
	}
	cli = append(cli, "--project", pos[0])
	if *db != "" {
		cli = append(cli, "--db", *db)
	}
	if *noColor {
		cli = append(cli, "--no-color")
	}
	return cmdCLI(cli)
}

// cmdSearch keeps `memstated search` as an alias of `memstate search`.
func cmdSearch(args []string) int {
	return cmdCLI(append([]string{"search"}, args...))
}
