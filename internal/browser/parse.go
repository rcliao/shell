// Package browser provides headless Chrome automation via chromedp.
package browser

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ActionType identifies a browser action.
type ActionType int

const (
	ActionNavigate ActionType = iota
	ActionClick
	ActionType_
	ActionWait
	ActionScreenshot
	ActionExtract
	ActionJS
	ActionSleep
)

// Action represents a single browser step.
type Action struct {
	Type     ActionType
	Selector string // CSS selector (click, type, wait, extract)
	Value    string // typed text, JS expression, sleep duration, or URL
}

func (a Action) String() string {
	switch a.Type {
	case ActionNavigate:
		return fmt.Sprintf("navigate %q", a.Value)
	case ActionClick:
		return fmt.Sprintf("click %q", a.Selector)
	case ActionType_:
		return fmt.Sprintf("type %q %q", a.Selector, a.Value)
	case ActionWait:
		return fmt.Sprintf("wait %q", a.Selector)
	case ActionScreenshot:
		return "screenshot"
	case ActionExtract:
		return fmt.Sprintf("extract %q", a.Selector)
	case ActionJS:
		return fmt.Sprintf("js %q", a.Value)
	case ActionSleep:
		return fmt.Sprintf("sleep %q", a.Value)
	default:
		return "unknown"
	}
}

// Directive holds the parsed URL and action list from a [browser] block.
type Directive struct {
	URL     string
	Actions []Action
}

// browserRe matches [browser url="..."]...[/browser] blocks.
var BrowserRe = regexp.MustCompile(`(?s)\[browser url="([^"]+)"\]\s*(.*?)\s*\[/browser\]`)

// quoted captures a double-quoted string.
var quotedRe = regexp.MustCompile(`"([^"]*)"`)

// ParseDirective extracts the URL and actions from a browser block body.
func ParseDirective(url, body string) Directive {
	d := Directive{URL: url}

	lines := strings.Split(body, "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}

		switch {
		case line == "navigate":
			d.Actions = append(d.Actions, Action{Type: ActionNavigate, Value: url})

		case line == "screenshot":
			d.Actions = append(d.Actions, Action{Type: ActionScreenshot})

		case strings.HasPrefix(line, "click "):
			qs := quotedRe.FindStringSubmatch(line)
			if len(qs) >= 2 {
				d.Actions = append(d.Actions, Action{Type: ActionClick, Selector: qs[1]})
			}

		case strings.HasPrefix(line, "type "):
			qs := quotedRe.FindAllStringSubmatch(line, 2)
			if len(qs) >= 2 {
				d.Actions = append(d.Actions, Action{Type: ActionType_, Selector: qs[0][1], Value: qs[1][1]})
			}

		case strings.HasPrefix(line, "wait "):
			qs := quotedRe.FindStringSubmatch(line)
			if len(qs) >= 2 {
				d.Actions = append(d.Actions, Action{Type: ActionWait, Selector: qs[1]})
			}

		case strings.HasPrefix(line, "extract "):
			qs := quotedRe.FindStringSubmatch(line)
			if len(qs) >= 2 {
				d.Actions = append(d.Actions, Action{Type: ActionExtract, Selector: qs[1]})
			}

		case strings.HasPrefix(line, "js "):
			qs := quotedRe.FindStringSubmatch(line)
			if len(qs) >= 2 {
				d.Actions = append(d.Actions, Action{Type: ActionJS, Value: qs[1]})
			}

		case strings.HasPrefix(line, "sleep "):
			qs := quotedRe.FindStringSubmatch(line)
			if len(qs) >= 2 {
				d.Actions = append(d.Actions, Action{Type: ActionSleep, Value: qs[1]})
			}
		}
	}

	return d
}

// ParseSleepDuration parses a sleep value like "2s", "500ms" into time.Duration.
func ParseSleepDuration(val string) (time.Duration, error) {
	d, err := time.ParseDuration(val)
	if err != nil {
		return 0, fmt.Errorf("invalid sleep duration %q: %w", val, err)
	}
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d, nil
}
