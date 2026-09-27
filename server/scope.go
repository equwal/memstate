package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// userProject is the one reserved project. It holds facts about the user
// and the host that are true in every repository: preferences, profile,
// environment, tool configuration. It never collides with a repository
// slug because slugProject trims edge underscores.
const userProject = "_user"

// ErrReservedWrite marks a write that the reserved-project gate refused.
// Handlers map it to HTTP 400.
var ErrReservedWrite = errors.New("reserved write")

// userAreas are the top-level areas allowed in userProject for facts that
// hold for the user on every machine.
var userAreas = []string{"preferences", "profile"}

// hostAreas are the areas allowed under host.<host_slug> for facts that hold
// only on one machine.
var hostAreas = []string{"env", "tools"}

const allowedUserShapes = "preferences.<topic>, profile.<topic>, " +
	"host.<host_slug>.env.<topic>, host.<host_slug>.tools.<topic>"

// checkReservedWrite is the line between project memory and user memory.
// Any project id that starts with "_" is reserved; only userProject exists,
// and a keypath there must match one of the allowed shapes. A short
// allowlist is the wall against session summaries, todos and decisions
// landing in a shared store. Normal projects pass untouched.
func checkReservedWrite(projectID, keypath string) error {
	if !strings.HasPrefix(projectID, "_") {
		return nil
	}
	if projectID != userProject {
		return fmt.Errorf("%w: project id prefix \"_\" is reserved; the only reserved project is %q",
			ErrReservedWrite, userProject)
	}
	seg := strings.Split(keypath, ".")
	for _, s := range seg {
		if s == "" {
			return reservedKeypathErr(keypath)
		}
	}
	switch {
	case len(seg) >= 2 && slices.Contains(userAreas, seg[0]):
		return nil
	case len(seg) >= 4 && seg[0] == "host" && slices.Contains(hostAreas, seg[2]):
		return nil
	}
	return reservedKeypathErr(keypath)
}

func reservedKeypathErr(keypath string) error {
	return fmt.Errorf("%w: keypath %q is not allowed in %s; allowed shapes: %s",
		ErrReservedWrite, keypath, userProject, allowedUserShapes)
}

// isOtherHost reports whether a user-scope keypath describes another
// machine: anything under host.<slug> where slug is not host.
func isOtherHost(keypath, host string) bool {
	seg := strings.SplitN(keypath, ".", 3)
	return len(seg) >= 2 && seg[0] == "host" && seg[1] != host
}

// hostSlug names this machine inside userProject: the first label of the
// hostname, slugged like a project id. The TS proxy and the Python skill
// apply the same rule, so all three agree on the segment.
func hostSlug() string {
	h, _ := os.Hostname()
	return slugHost(h)
}

func slugHost(hostname string) string {
	label, _, _ := strings.Cut(hostname, ".")
	return slugProject(label)
}
