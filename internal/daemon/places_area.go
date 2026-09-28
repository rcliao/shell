package daemon

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rcliao/shell/internal/config"
	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/store"
)

// Area places (docs/DESIGN-PROJECT-AREAS.md, part 2) are Discord only: the
// family moved there, and Telegram cannot list topics, so find-or-create
// (what makes two agents converge on one set of channels) is impossible.

func (p daemonPlaces) EnsureAreaPlaces(chatID int64, name, forumName string, tags []string) (int64, string, bool, error) {
	if p.dc == nil {
		return 0, "", false, fmt.Errorf("areas with their own channels need Discord; on Telegram, create a topic and pass --thread and --place-ref telegram")
	}
	ac, err := p.dc.EnsureAreaChannels(chatID, name, forumName, tags)
	if err != nil {
		return 0, "", false, err
	}
	thread, err := strconv.ParseInt(ac.ChannelID, 10, 64)
	if err != nil {
		return 0, "", false, err
	}
	return thread, "discord:" + ac.ForumID, ac.Created, nil
}

func (p daemonPlaces) FindThread(chatID int64, ref, title string) (int64, bool, error) {
	if !strings.HasPrefix(ref, "discord:") || p.dc == nil {
		return 0, false, nil // Telegram topics cannot be listed.
	}
	return p.dc.FindOpenPost(strings.TrimPrefix(ref, "discord:"), title)
}

func (p daemonPlaces) ThreadInfo(chatID, threadID int64) (string, string, []string, error) {
	if p.dc == nil {
		return "", "", nil, fmt.Errorf("joining a post needs Discord")
	}
	info, err := p.dc.PostInfo(threadID)
	if err != nil {
		return "", "", nil, err
	}
	ref := ""
	if info.ForumID != "" {
		ref = "discord:" + info.ForumID
	}
	return info.Title, ref, info.Tags, nil
}

func (p daemonPlaces) UpdateOpener(chatID, threadID int64, ref, text string) error {
	if !strings.HasPrefix(ref, "discord:") || p.dc == nil {
		return nil // A Telegram topic's first message id is not known.
	}
	return p.dc.EditPostOpener(threadID, text)
}

func (p daemonPlaces) PostSummary(chatID, threadID int64, ref, text string) (int64, error) {
	if !strings.HasPrefix(ref, "discord:") || p.dc == nil {
		return 0, fmt.Errorf("post summaries need Discord")
	}
	return p.dc.PostPinned(threadID, text)
}

func (p daemonPlaces) EditSummary(chatID, threadID, msgID int64, ref, text string) error {
	if !strings.HasPrefix(ref, "discord:") || p.dc == nil {
		return fmt.Errorf("post summaries need Discord")
	}
	return p.dc.EditIn(threadID, msgID, text)
}

// sharedRootFor is where docs both agents share live: beside the shared
// group transcript (~/.shell/shared by default), which both agents already
// agree on.
func sharedRootFor(cfg config.Config) string {
	if p := cfg.Agent.TranscriptPath; p != "" {
		return filepath.Dir(p)
	}
	return filepath.Join(config.DefaultConfigDir(), "shared")
}

// linkSharedDocs puts every project with its own Discord place (a post, or
// an area's channel) on the doc both agents share (part 3). Run at startup:
// it is how projects made before shared docs migrate, and it is idempotent.
func linkSharedDocs(st *store.Store, root, workspaceDir, agent string) {
	if st == nil || root == "" || workspaceDir == "" || agent == "" {
		return
	}
	projects, err := st.ListProjects(0)
	if err != nil {
		return
	}
	for _, p := range projects {
		if p.Status != "active" || p.MessageThreadID < 100_000_000_000_000_000 {
			continue
		}
		if p.Kind != store.ProjectKindArea && p.Area == "" {
			continue
		}
		owner, err := project.LinkShared(root, workspaceDir, p.Slug, agent, p.MessageThreadID)
		if err != nil {
			slog.Warn("shared doc: link failed", "slug", p.Slug, "error", err)
			continue
		}
		slog.Info("shared doc: linked", "slug", p.Slug, "thread", p.MessageThreadID, "owner", owner)
	}
}
