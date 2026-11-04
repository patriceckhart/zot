package provider

import (
	"net/http"
	"net/url"
	"strings"
)

// OpenRouter app-attribution headers. OpenRouter uses HTTP-Referer as the
// primary identifier for an app in its public rankings; the title headers set
// the display name, and the categories header files the app into the
// marketplace. Attribution is optional metadata, so a failure to send it must
// never fail a request.
//
// See https://openrouter.ai/docs/app-attribution
const (
	openRouterReferer    = "https://www.zot.sh"
	openRouterTitle      = "zot"
	openRouterCategories = "cli-agent"
)

// setOpenRouterAttribution stamps zot's app-attribution headers on req. The
// caller decides when req targets OpenRouter; see openRouterHost for the
// endpoint-based check used where the provider name is not in scope.
func setOpenRouterAttribution(req *http.Request) {
	req.Header.Set("HTTP-Referer", openRouterReferer)
	req.Header.Set("X-OpenRouter-Title", openRouterTitle)
	// X-Title is the legacy name OpenRouter still accepts for the title.
	req.Header.Set("X-Title", openRouterTitle)
	req.Header.Set("X-OpenRouter-Categories", openRouterCategories)
}

// isOpenRouterURL reports whether u addresses OpenRouter itself, including
// regional subdomains. Used where a request is built from a model-supplied
// base URL rather than a known provider name.
func isOpenRouterURL(u *url.URL) bool {
	if u == nil {
		return false
	}
	return openRouterHost(u.Hostname())
}

// openRouterHost reports whether host is openrouter.ai or one of its
// subdomains.
func openRouterHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai")
}
