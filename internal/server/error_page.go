package server

import (
	"fmt"
	"net/http"
)

// errorPageInfo defines the content for a styled HTML error page.
type errorPageInfo struct {
	Icon        string // inline SVG for the hero area
	IconSpin    bool   // whether to add a spin animation to the icon
	Title       string // e.g. "Sandbox Not Found"
	Description string // longer explanation
	StatusCode  int
}

var (
	errPageSandboxNotFound = errorPageInfo{
		Icon:        iconCircleX,
		Title:       "Sandbox Not Found",
		Description: "The sandbox you're looking for doesn't exist or you don't have access to it.",
		StatusCode:  http.StatusNotFound,
	}
	errPageSandboxNotRunning = errorPageInfo{
		Icon:        iconPause,
		Title:       "Sandbox Not Running",
		Description: "This sandbox is currently paused or stopped. Resume it from the dashboard to continue.",
		StatusCode:  http.StatusServiceUnavailable,
	}
	errPageAgentOffline = errorPageInfo{
		Icon:        iconWifiOff,
		Title:       "Agent Offline",
		Description: "The local agent is not connected. Reconnect it to access this sandbox.",
		StatusCode:  http.StatusServiceUnavailable,
	}
	errPagePodNotReady = errorPageInfo{
		Icon:        iconSpinner,
		IconSpin:    true,
		Title:       "Sandbox Starting",
		Description: "The sandbox is still booting up. This page will refresh automatically — hang tight.",
		StatusCode:  http.StatusServiceUnavailable,
	}
)

// writeErrorPage renders a styled full-page HTML error to the response.
func writeErrorPage(w http.ResponseWriter, info errorPageInfo) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(info.StatusCode)

	autoRefresh := ""
	if info.StatusCode == http.StatusServiceUnavailable {
		autoRefresh = `<meta http-equiv="refresh" content="5">`
	}

	iconClass := "icon"
	if info.IconSpin {
		iconClass = "icon icon-spin"
	}

	fmt.Fprintf(w, errorPageTemplate,
		autoRefresh,
		iconClass, info.Icon,
		info.Title,
		info.Description,
		info.StatusCode,
	)
}

// Inline SVG icons (Lucide-style, no external dependencies).
const iconCircleX = `<svg xmlns="http://www.w3.org/2000/svg" width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="m15 9-6 6"/><path d="m9 9 6 6"/></svg>`

const iconPause = `<svg xmlns="http://www.w3.org/2000/svg" width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><line x1="10" x2="10" y1="15" y2="9"/><line x1="14" x2="14" y1="15" y2="9"/></svg>`

const iconWifiOff = `<svg xmlns="http://www.w3.org/2000/svg" width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M12 20h.01"/><path d="M8.5 16.429a5 5 0 0 1 7 0"/><path d="M5 12.859a10 10 0 0 1 5.17-2.69"/><path d="M13.83 10.17A10 10 0 0 1 19 12.86"/><path d="M2 8.82a15 15 0 0 1 4.17-2.65"/><path d="M10.66 5c4.01-.36 8.14.9 11.34 3.76"/><line x1="2" x2="22" y1="2" y2="22"/></svg>`

const iconSpinner = `<svg xmlns="http://www.w3.org/2000/svg" width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-6.219-8.56"/></svg>`

const errorPageTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
%s
<title>Error</title>
<style>
  *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }

  @media (prefers-color-scheme: light) {
    :root {
      --bg: #ffffff;
      --fg: #0a0a0a;
      --muted: #71717a;
      --border: #e4e4e7;
      --icon-color: #a1a1aa;
      --badge-bg: #f4f4f5;
      --badge-fg: #52525b;
    }
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #0a0a0a;
      --fg: #fafafa;
      --muted: #a1a1aa;
      --border: #27272a;
      --icon-color: #52525b;
      --badge-bg: #27272a;
      --badge-fg: #a1a1aa;
    }
  }

  body {
    font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
    background: var(--bg);
    color: var(--fg);
    display: flex;
    align-items: center;
    justify-content: center;
    min-height: 100vh;
    padding: 1.5rem;
    -webkit-font-smoothing: antialiased;
  }

  .container {
    max-width: 420px;
    width: 100%%;
    text-align: center;
  }

  .icon {
    color: var(--icon-color);
    margin-bottom: 1.5rem;
    display: inline-block;
  }

  @keyframes spin { to { transform: rotate(360deg); } }
  .icon-spin svg { animation: spin 1s linear infinite; }

  h1 {
    font-size: 1.375rem;
    font-weight: 600;
    letter-spacing: -0.01em;
    margin-bottom: 0.625rem;
  }

  .description {
    color: var(--muted);
    font-size: 0.9375rem;
    line-height: 1.6;
    margin-bottom: 1.75rem;
  }

  .badge {
    display: inline-block;
    background: var(--badge-bg);
    color: var(--badge-fg);
    font-size: 0.75rem;
    font-weight: 500;
    font-family: 'SF Mono', SFMono-Regular, ui-monospace, monospace;
    padding: 0.25rem 0.75rem;
    border-radius: 9999px;
    border: 1px solid var(--border);
  }

  .divider {
    width: 3rem;
    height: 1px;
    background: var(--border);
    margin: 1.5rem auto;
  }

  .back-link {
    color: var(--muted);
    font-size: 0.8125rem;
    text-decoration: none;
    transition: color 0.15s;
  }
  .back-link:hover { color: var(--fg); }
</style>
</head>
<body>
  <div class="container">
    <div class="%s">%s</div>
    <h1>%s</h1>
    <p class="description">%s</p>
    <span class="badge">HTTP %d</span>
    <div class="divider"></div>
    <a href="javascript:history.back()" class="back-link">&larr; Go back</a>
  </div>
</body>
</html>`
