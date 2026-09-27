package daemon

import (
	"fmt"
	"strings"

	"github.com/rcliao/shell/internal/discord"
	"github.com/rcliao/shell/internal/telegram"
)

// daemonPlaces implements rpc.Places over the platform bots
// (docs/DESIGN-PROJECT-AREAS.md). The area's place_ref picks the platform: a
// family chat can be linked on both, so the chat id alone cannot.
type daemonPlaces struct {
	tg *telegram.Bot // nil when headless
	dc *discord.Bot  // nil when Discord is off
}

// parsePlaceRef splits "discord:<forum id>" / "telegram".
func (p daemonPlaces) parse(ref string) (platform, forum string, err error) {
	switch {
	case ref == "telegram":
		if p.tg == nil {
			return "", "", fmt.Errorf("telegram is not running for this agent")
		}
		return "telegram", "", nil
	case strings.HasPrefix(ref, "discord:"):
		if p.dc == nil {
			return "", "", fmt.Errorf("discord is not running for this agent")
		}
		return "discord", strings.TrimPrefix(ref, "discord:"), nil
	}
	return "", "", fmt.Errorf("unknown place_ref %q", ref)
}

func (p daemonPlaces) CreateThread(chatID int64, ref, title, content string, tags []string) (int64, error) {
	platform, forum, err := p.parse(ref)
	if err != nil {
		return 0, err
	}
	if platform == "discord" {
		return p.dc.CreateForumPost(forum, title, content, tags)
	}
	return p.tg.CreateForumTopic(chatID, title, content)
}

func (p daemonPlaces) SetThreadTags(chatID, threadID int64, ref string, tags []string) error {
	platform, _, err := p.parse(ref)
	if err != nil {
		return err
	}
	if platform == "discord" {
		return p.dc.SetPostTags(threadID, tags)
	}
	return nil // Telegram topics have no tags.
}

func (p daemonPlaces) ArchiveThread(chatID, threadID int64, ref string) error {
	platform, _, err := p.parse(ref)
	if err != nil {
		return err
	}
	if platform == "discord" {
		return p.dc.ArchiveThread(threadID)
	}
	return p.tg.CloseForumTopic(chatID, threadID)
}
