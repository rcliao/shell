package browser

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// Realistic Chrome user agent string.
const defaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36"

// JS snippet to remove automation indicators after page load.
const stealthJS = `
// Remove webdriver flag
Object.defineProperty(navigator, 'webdriver', {get: () => undefined});
// Fake plugins
Object.defineProperty(navigator, 'plugins', {get: () => [1, 2, 3, 4, 5]});
// Fake languages
Object.defineProperty(navigator, 'languages', {get: () => ['en-US', 'en']});
// Spoof chrome runtime
window.chrome = {runtime: {}};
`

// Config holds browser automation settings.
type Config struct {
	Enabled        bool   `json:"enabled"`
	Headless       bool   `json:"headless"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	ChromePath     string `json:"chrome_path"`
}

// StepResult holds the outcome of a single action.
type StepResult struct {
	Step        int
	Description string
	Output      string // text output ("OK", extracted text, JS result)
	Screenshot  []byte // non-nil only for screenshot actions
	Err         error
}

// Result holds the outcome of executing a browser directive.
type Result struct {
	URL   string
	Steps []StepResult
}

// Execute runs a browser directive: navigates to the URL and performs each action.
func Execute(ctx context.Context, cfg Config, d Directive) *Result {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Build chromedp options with anti-detection measures.
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		// Anti-detection: remove automation indicators
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("exclude-switches", "enable-automation"),
		chromedp.Flag("disable-features", "AutomationControlled"),

		// Realistic user agent
		chromedp.UserAgent(defaultUserAgent),

		// Realistic window size
		chromedp.WindowSize(1920, 1080),

		// Disable automation-related infobars
		chromedp.Flag("disable-infobars", true),
		chromedp.Flag("disable-extensions", true),
	)
	if cfg.Headless {
		// Use new headless mode which is harder to detect
		opts = append(opts, chromedp.Flag("headless", "new"))
	}
	if cfg.ChromePath != "" {
		opts = append(opts, chromedp.ExecPath(cfg.ChromePath))
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()

	browserCtx, browserCancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(slog.Info))
	defer browserCancel()

	result := &Result{URL: d.URL}

	// Inject stealth JS before navigation to patch navigator properties.
	if err := chromedp.Run(browserCtx, chromedp.Evaluate(stealthJS, nil)); err != nil {
		slog.Warn("browser: stealth injection failed (non-fatal)", "error", err)
	}

	// Always navigate first.
	slog.Info("browser: navigating", "url", d.URL)
	if err := chromedp.Run(browserCtx, chromedp.Navigate(d.URL)); err != nil {
		result.Steps = append(result.Steps, StepResult{
			Step:        1,
			Description: fmt.Sprintf("navigate %q", d.URL),
			Err:         err,
		})
		return result
	}
	result.Steps = append(result.Steps, StepResult{
		Step:        1,
		Description: fmt.Sprintf("navigate %q", d.URL),
		Output:      "OK",
	})

	// Re-inject stealth after navigation (page load resets JS state).
	_ = chromedp.Run(browserCtx, chromedp.Evaluate(stealthJS, nil))

	// Execute each action.
	for i, action := range d.Actions {
		stepNum := i + 2 // navigate is step 1
		if action.Type == ActionNavigate {
			// Already navigated above; skip duplicate navigate actions.
			continue
		}

		sr := executeAction(browserCtx, stepNum, action)
		result.Steps = append(result.Steps, sr)

		if sr.Err != nil {
			slog.Warn("browser: action failed", "step", stepNum, "action", action.String(), "error", sr.Err)
			// Continue executing remaining actions so Claude sees partial results.
		}
	}

	return result
}

func executeAction(ctx context.Context, step int, a Action) StepResult {
	sr := StepResult{Step: step, Description: a.String()}

	switch a.Type {
	case ActionClick:
		err := chromedp.Run(ctx, chromedp.Click(a.Selector, chromedp.ByQuery))
		if err != nil {
			sr.Err = err
		} else {
			sr.Output = "OK"
		}

	case ActionType_:
		err := chromedp.Run(ctx,
			chromedp.Clear(a.Selector, chromedp.ByQuery),
			chromedp.SendKeys(a.Selector, a.Value, chromedp.ByQuery),
		)
		if err != nil {
			sr.Err = err
		} else {
			sr.Output = "OK"
		}

	case ActionWait:
		waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		err := chromedp.Run(waitCtx, chromedp.WaitVisible(a.Selector, chromedp.ByQuery))
		if err != nil {
			sr.Err = err
		} else {
			sr.Output = "OK"
		}

	case ActionScreenshot:
		var buf []byte
		err := chromedp.Run(ctx, chromedp.FullScreenshot(&buf, 90))
		if err != nil {
			sr.Err = err
		} else {
			sr.Screenshot = buf
			sr.Output = "[sent to chat]"
		}

	case ActionExtract:
		var text string
		err := chromedp.Run(ctx, chromedp.Text(a.Selector, &text, chromedp.ByQuery))
		if err != nil {
			sr.Err = err
		} else {
			sr.Output = strings.TrimSpace(text)
		}

	case ActionJS:
		var res interface{}
		err := chromedp.Run(ctx, chromedp.Evaluate(a.Value, &res))
		if err != nil {
			sr.Err = err
		} else {
			sr.Output = fmt.Sprintf("%v", res)
		}

	case ActionSleep:
		dur, err := ParseSleepDuration(a.Value)
		if err != nil {
			sr.Err = err
		} else {
			time.Sleep(dur)
			sr.Output = "OK"
		}

	default:
		sr.Err = fmt.Errorf("unknown action type %d", a.Type)
	}

	return sr
}

// FormatResults formats the result for feeding back to Claude.
func FormatResults(r *Result) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[Browser results for %s]\n", r.URL)
	for _, s := range r.Steps {
		if s.Err != nil {
			fmt.Fprintf(&sb, "Step %d: %s → ERROR: %s\n", s.Step, s.Description, s.Err)
		} else {
			fmt.Fprintf(&sb, "Step %d: %s → %s\n", s.Step, s.Description, s.Output)
		}
	}
	sb.WriteString("[End of browser results]")
	return sb.String()
}
