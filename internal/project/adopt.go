// Adoption (P3.5): binding a project to a Notion page a HUMAN made.
//
// A month of production polling found no comments, because the pages people
// actually open are hand-made shared pages, not the pages this package
// renders. Adoption lets the comment loop watch one of those.
//
// An adopted page is WATCH-ONLY. The renderer and the edit reconciler only
// speak headings, bullets and paragraphs; a human page holds tables,
// checkboxes and whatever else Notion offers, and a render pass over it would
// archive every block it cannot express. So for an adopted map: comments
// become events exactly as for a rendered page, a page edit counts as human
// activity and nothing more, and Render / reconcile are hard no-ops. The
// agent changes an adopted page the way it always has — by hand, with its own
// Notion tools, when someone asks.
package project

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// adoptedSection is the single block-map section an adopted page uses: the
// page's top-level block ids, which are the comment-poll targets.
const adoptedSection = "_adopted"

// notionIDPattern matches a Notion id: 32 hex chars, dashed or not.
var notionIDPattern = regexp.MustCompile(`(?i)[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}`)

// ParseNotionPageID extracts the page id from a Notion URL or a bare id and
// returns it undashed and lowercase. In a URL the id is the tail of the last
// path segment ("Some-Title-<id>"); the query string can carry OTHER ids
// (?v=<view id>, ?p=<peek page>), so it is cut before matching.
func ParseNotionPageID(ref string) (string, error) {
	s := strings.TrimSpace(ref)
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	matches := notionIDPattern.FindAllString(s, -1)
	if len(matches) == 0 {
		return "", fmt.Errorf("no Notion page id found in %q", ref)
	}
	id := matches[len(matches)-1]
	return strings.ToLower(strings.ReplaceAll(id, "-", "")), nil
}

// AdoptedBlockMap builds the watch-only map for a page from its top-level
// blocks. Comments anchored to NESTED blocks (inside a toggle, a table row, a
// column) are out of reach — Notion's comment listing is per block and not
// recursive, and walking every subtree on every poll is the cost adoption is
// meant to avoid. Page-level and top-level-block comments cover how people
// comment in practice.
func AdoptedBlockMap(children []NotionBlockRef) BlockMap {
	ids := make([]string, 0, len(children))
	for _, c := range children {
		if c.ID != "" {
			ids = append(ids, c.ID)
		}
	}
	return BlockMap{
		Adopted:  true,
		Sections: map[string]BlockMapSection{adoptedSection: {Blocks: ids}},
		Order:    []string{adoptedSection},
	}
}

// AdoptPage verifies the integration can read pageID and returns the encoded
// watch-only block map for it. The read IS the permission check: a page that
// was never shared with the integration fails here, before anything is
// stored.
func AdoptPage(ctx context.Context, api NotionAPI, pageID string) (blockMap string, blocks int, err error) {
	if api == nil || !api.Enabled() {
		return "", 0, fmt.Errorf("notion is not configured (NOTION_TOKEN)")
	}
	if _, err := api.GetPageLastEdited(ctx, pageID); err != nil {
		return "", 0, fmt.Errorf("cannot read page %s — share it with the integration first (page ••• menu → Connections): %w", pageID, err)
	}
	children, err := api.GetBlockChildren(ctx, pageID)
	if err != nil {
		return "", 0, fmt.Errorf("list page blocks: %w", err)
	}
	bm := AdoptedBlockMap(children)
	return bm.encode(), len(bm.Sections[adoptedSection].Blocks), nil
}

// AdoptedCommentPrompt is the revision prompt for a comment on an ADOPTED
// page. There is no canonical doc and no doc-write: the page belongs to the
// people who made it, so the agent answers in the thread and edits the page
// by hand only when the comment asks for a change.
func AdoptedCommentPrompt(slug, title, instructions, lang, pageID, comment, anchorBlock string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Project comment: %s]\n", slug)
	fmt.Fprintf(&b, "Project: %s\n", title)
	if instructions != "" {
		fmt.Fprintf(&b, "Standing instructions: %s\n", instructions)
	}
	fmt.Fprintf(&b, "Notion page id: %s (a page the family made and edits by hand — not one you render)\n", pageID)
	b.WriteString("\nSomeone commented on this page")
	if anchorBlock != "" {
		fmt.Fprintf(&b, ", on block %s", anchorBlock)
	}
	b.WriteString(":\n---\n")
	b.WriteString(comment)
	if !strings.HasSuffix(comment, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("---\n")
	b.WriteString("\nDo ONE bounded pass for this comment now. Hard rules:\n")
	b.WriteString("- If it is a question, answer it. If it asks for a change, make exactly that change on the page with your Notion tools — read the affected blocks first, touch nothing else, and never restructure or tidy beyond what was asked.\n")
	b.WriteString("- Do NOT use the project skill's doc-write: this page has no managed doc.\n")
	b.WriteString("- Your reply is posted INTO the Notion comment thread, not the chat: at most 2 short lines, saying what you did or the answer. Claim an edit only if you made it and read it back.\n")
	b.WriteString("- Text only — no images, files, or generated media.\n")
	if lang != "" {
		fmt.Fprintf(&b, "- Reply in the project's language: %s.\n", lang)
	}
	return b.String()
}
