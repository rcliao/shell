package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Shared docs (docs/DESIGN-PROJECT-AREAS.md, part 3): a project that lives
// in a Discord post, and an area bound to its channel, has ONE doc repo that
// both agents use: <root>/projects/post-<thread>/. Each agent's
// workspace/projects/<slug> is a symlink to it, so every path that goes
// through the workspace works unchanged. The first agent to link moves its
// doc there and becomes the owner (research and the post summary are its).

const (
	ownerFile   = "owner"
	summaryFile = "summary.json"
	lockFile    = ".doc.lock"
)

// SharedDir is the one doc repo for a post or channel.
func SharedDir(root string, threadID int64) string {
	return filepath.Join(root, "projects", "post-"+strconv.FormatInt(threadID, 10))
}

// NotesFile is where a later agent's own doc is kept when it links.
func NotesFile(agent string) string { return "notes-" + agent + ".md" }

// LinkShared makes workspace/projects/<slug> a symlink to the post's shared
// doc repo, creating or adopting it. Idempotent. It reports whether agent
// owns the shared doc.
//
// hasResearch says this agent runs the project's research. Ownership follows
// research: an owner without it gives way to an agent with it, so which
// daemon happens to link first can never leave a doc unresearched.
func LinkShared(root, workspaceDir, slug, agent string, threadID int64, hasResearch bool) (owner bool, err error) {
	if root == "" || workspaceDir == "" || slug == "" || agent == "" || threadID == 0 {
		return false, fmt.Errorf("link shared doc: missing root, workspace, slug, agent or thread")
	}
	sd := SharedDir(root, threadID)
	if err := os.MkdirAll(filepath.Dir(sd), 0o755); err != nil {
		return false, err
	}
	unlock, err := lockPath(filepath.Join(filepath.Dir(sd), ".link.lock"))
	if err != nil {
		return false, err
	}
	defer unlock()

	ws := filepath.Join(workspaceDir, "projects", slug)
	if err := os.MkdirAll(filepath.Dir(ws), 0o755); err != nil {
		return false, err
	}
	fi, lerr := os.Lstat(ws)
	isLink := lerr == nil && fi.Mode()&os.ModeSymlink != 0
	if isLink {
		if target, _ := filepath.EvalSymlinks(ws); target == mustEval(sd) {
			claimOwner(sd, agent, hasResearch)
			return Owner(sd) == agent, nil
		}
	}
	if _, err := os.Stat(filepath.Join(sd, ".git")); os.IsNotExist(err) {
		// First agent: bring this agent's doc and its history, or start one.
		if lerr == nil && !isLink && fi.IsDir() && hasGit(ws) {
			if err := os.Rename(ws, sd); err != nil {
				return false, fmt.Errorf("move doc to shared: %w", err)
			}
		} else {
			if err := os.MkdirAll(sd, 0o755); err != nil {
				return false, err
			}
			if _, err := git(sd, "init", "--quiet"); err != nil {
				return false, err
			}
		}
		writeOwner(sd, agent, hasResearch)
	} else if lerr == nil && !isLink {
		// A later agent: keep its own doc's content as notes to fold in, and
		// its repo as a backup; the shared doc is the doc from now on.
		if fi.IsDir() && ownContent(ws) {
			if data, err := os.ReadFile(filepath.Join(ws, DocFile)); err == nil {
				_ = os.WriteFile(filepath.Join(sd, NotesFile(agent)), data, 0o644)
			}
		}
		backup := ws + ".pre-shared-" + time.Now().Format("20060102-150405")
		if err := os.Rename(ws, backup); err != nil {
			return false, fmt.Errorf("back up own doc: %w", err)
		}
	}
	if isLink {
		_ = os.Remove(ws) // pointed elsewhere
	}
	if _, err := os.Lstat(ws); os.IsNotExist(err) {
		if err := os.Symlink(sd, ws); err != nil {
			return false, fmt.Errorf("link doc: %w", err)
		}
	}
	claimOwner(sd, agent, hasResearch)
	return Owner(sd) == agent, nil
}

// claimOwner makes agent the owner when there is none, or when the owner
// does not run research and agent does.
func claimOwner(sd, agent string, hasResearch bool) {
	o := Owner(sd)
	switch {
	case o == "":
		writeOwner(sd, agent, hasResearch)
	case o == agent:
		if hasResearch != OwnerHasResearch(sd) {
			writeOwner(sd, agent, hasResearch)
		}
	case hasResearch && !OwnerHasResearch(sd):
		writeOwner(sd, agent, true)
	}
}

func writeOwner(sd, agent string, hasResearch bool) {
	body := agent + "\n"
	if hasResearch {
		body += "research\n"
	}
	_ = os.WriteFile(filepath.Join(sd, ownerFile), []byte(body), 0o644)
}

// OwnerHasResearch reports whether the owner runs the doc's research.
func OwnerHasResearch(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, ownerFile))
	return err == nil && strings.Contains(string(data), "\nresearch")
}

// MayResearch reports whether agent should run the doc's research pass: the
// owner does; another agent does only if the owner has no research of its
// own (a doc must never end up researched by nobody).
func MayResearch(dir, agent string) bool {
	o := Owner(dir)
	return o == "" || o == agent || agent == "" || !OwnerHasResearch(dir)
}

// Owner returns the agent that owns a shared doc dir, "" for an unshared
// doc (the dir may be the workspace symlink).
func Owner(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, ownerFile))
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSpace(first)
}

// IsShared reports whether a doc dir is a shared doc.
func IsShared(dir string) bool { return Owner(dir) != "" }

// MayRunAs reports whether agent does owner-only work (research, the post
// summary) for this doc dir: always for an unshared doc.
func MayRunAs(dir, agent string) bool {
	o := Owner(dir)
	return o == "" || o == agent || agent == ""
}

func hasGit(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// ownContent: the doc has history beyond its scaffold commit.
func ownContent(dir string) bool {
	if !hasGit(dir) {
		return false
	}
	out, err := git(dir, "rev-list", "--count", "HEAD")
	if err != nil {
		return false
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n > 1
}

func mustEval(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// lockPath takes an exclusive advisory lock shared by both agents' daemons.
func lockPath(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// LockDoc serializes writes to a doc across both agents (a no-op lock file
// in an unshared repo costs nothing).
func LockDoc(dir string) (func(), error) { return lockPath(filepath.Join(mustEval(dir), lockFile)) }

// The rev this agent last saw of each doc, so a write to a shared doc can be
// refused when the other agent wrote in between (no lost updates). One
// daemon is one agent, so a process-wide map is per agent.
var seen = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

// NoteSeen records that this agent was shown the doc at rev.
func NoteSeen(dir, rev string) {
	if rev == "" {
		return
	}
	seen.Lock()
	seen.m[mustEval(dir)] = rev
	seen.Unlock()
}

// SeenRev is the rev this agent last saw, "" if none.
func SeenRev(dir string) string {
	seen.Lock()
	defer seen.Unlock()
	return seen.m[mustEval(dir)]
}

// ErrStaleWrite: the shared doc moved since this agent read it.
var ErrStaleWrite = errors.New("stale write")

// CheckFresh refuses a write to a shared doc this agent has not seen at its
// current rev. Call it holding LockDoc.
func CheckFresh(dir string) error {
	if !IsShared(dir) {
		return nil
	}
	head, err := Head(dir)
	if err != nil {
		return nil // an empty repo has nothing to lose
	}
	switch s := SeenRev(dir); {
	case s == "":
		return fmt.Errorf("%w: this doc is shared with the other agent; project doc-read first, then write", ErrStaleWrite)
	case s != head:
		return fmt.Errorf("%w: the doc changed since you read it (now rev %s); project doc-read again and redo your edit on top", ErrStaleWrite, head)
	}
	return nil
}

// SummaryState is the post summary message the owner keeps when it cannot
// edit the post's first message.
type SummaryState struct {
	Agent     string `json:"agent"`
	MessageID int64  `json:"message_id"`
}

func ReadSummary(dir string) SummaryState {
	var s SummaryState
	if data, err := os.ReadFile(filepath.Join(dir, summaryFile)); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	return s
}

func WriteSummary(dir string, s SummaryState) error {
	data, _ := json.Marshal(s)
	return os.WriteFile(filepath.Join(dir, summaryFile), data, 0o644)
}
