package daemon

import (
	"fmt"
	"strconv"
	"strings"
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
