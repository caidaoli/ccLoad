package app

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
)

// codexCloudflareCookieJar 对齐官方 Codex（rust-v0.157.1
// http-client/src/chatgpt_cloudflare_cookies.rs）：只为 https 的 ChatGPT 主机保存
// Cloudflare 基础设施 Cookie 与 __oailb 路由 Cookie，HTTP 与 WebSocket 握手共用。
// 账号、会话等其他 Cookie 一律丢弃。官方是进程级存储；ccLoad 承载多个账号，
// 共用 Cookie 会让上游把账号重新关联起来，因此每个账号凭证池各持一个。
type codexCloudflareCookieJar struct {
	jar *cookiejar.Jar
}

func newCodexCloudflareCookieJar() http.CookieJar {
	// 主机已限定为 ChatGPT 域，无需公共后缀表；options 为 nil 时 New 不会失败。
	jar, _ := cookiejar.New(nil)
	return &codexCloudflareCookieJar{jar: jar}
}

func (j *codexCloudflareCookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if !isChatGPTCookieURL(u) {
		return
	}
	allowed := make([]*http.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		if isCodexCloudflareCookieName(cookie.Name) {
			allowed = append(allowed, cookie)
		}
	}
	if len(allowed) > 0 {
		j.jar.SetCookies(u, allowed)
	}
}

func (j *codexCloudflareCookieJar) Cookies(u *url.URL) []*http.Cookie {
	if !isChatGPTCookieURL(u) {
		return nil
	}
	return j.jar.Cookies(u)
}

func isChatGPTCookieURL(u *url.URL) bool {
	if u == nil || u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "chatgpt.com", "chat.openai.com", "chatgpt-staging.com":
		return true
	}
	return strings.HasSuffix(host, ".chatgpt.com") || strings.HasSuffix(host, ".chatgpt-staging.com")
}

func isCodexCloudflareCookieName(name string) bool {
	switch name {
	case "__cf_bm", "__cflb", "__cfruid", "__cfseq", "__cfwaitingroom", "__oailb",
		"_cfuvid", "cf_clearance", "cf_ob_info", "cf_use_ob":
		return true
	}
	return strings.HasPrefix(name, "cf_chl_")
}
