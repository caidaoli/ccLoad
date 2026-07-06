package version

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	githubLatestReleaseURL = "https://github.com/caidaoli/ccLoad/releases/latest"
	releaseTagPathMarker   = "/releases/tag/"
)

func fetchLatestRelease(ctx context.Context, client *http.Client, latestURL string) (GitHubRelease, error) {
	if client == nil {
		client = http.DefaultClient
	}

	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, latestURL, nil)
	if err != nil {
		return GitHubRelease{}, fmt.Errorf("create release request: %w", err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("User-Agent", OutboundUserAgent())

	resp, err := client.Do(req)
	if err != nil {
		return GitHubRelease{}, fmt.Errorf("fetch latest release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		return GitHubRelease{}, fmt.Errorf("fetch latest release: status %d", resp.StatusCode)
	}
	releaseURL := resolvedLatestReleaseURL(resp)
	if releaseURL == "" {
		return GitHubRelease{}, fmt.Errorf("fetch latest release: status %d", resp.StatusCode)
	}

	tag, err := releaseTagFromURL(releaseURL)
	if err != nil {
		return GitHubRelease{}, err
	}
	return GitHubRelease{
		TagName: tag,
		HTMLURL: releaseURL,
	}, nil
}

func resolvedLatestReleaseURL(resp *http.Response) string {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return ""
	}

	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		location := strings.TrimSpace(resp.Header.Get("Location"))
		if location == "" {
			return ""
		}
		next, err := url.Parse(location)
		if err != nil {
			return ""
		}
		return resp.Request.URL.ResolveReference(next).String()
	}

	return resp.Request.URL.String()
}

func releaseTagFromURL(rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("parse latest release URL: %w", err)
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	idx := strings.LastIndex(path, releaseTagPathMarker)
	if idx < 0 {
		return "", fmt.Errorf("latest release URL %q missing %s", rawURL, releaseTagPathMarker)
	}
	escapedTag := strings.Trim(path[idx+len(releaseTagPathMarker):], "/")
	if escapedTag == "" {
		return "", fmt.Errorf("latest release URL %q missing tag", rawURL)
	}
	tag, err := url.PathUnescape(escapedTag)
	if err != nil {
		return "", fmt.Errorf("unescape latest release tag: %w", err)
	}
	return tag, nil
}

func releaseDownloadURL(release GitHubRelease, assetName string) (string, error) {
	if strings.TrimSpace(release.TagName) == "" {
		return "", fmt.Errorf("latest release missing tag_name")
	}
	if strings.TrimSpace(assetName) == "" {
		return "", fmt.Errorf("release %s has empty asset name", release.TagName)
	}

	u, err := url.Parse(strings.TrimSpace(release.HTMLURL))
	if err != nil {
		return "", fmt.Errorf("parse release URL: %w", err)
	}
	path := u.EscapedPath()
	idx := strings.LastIndex(path, releaseTagPathMarker)
	if idx < 0 {
		return "", fmt.Errorf("release URL %q missing %s", release.HTMLURL, releaseTagPathMarker)
	}

	prefix, err := url.PathUnescape(strings.TrimRight(path[:idx], "/"))
	if err != nil {
		return "", fmt.Errorf("unescape release URL prefix: %w", err)
	}
	base := url.URL{
		Scheme: u.Scheme,
		Host:   u.Host,
		Path:   prefix,
	}
	downloadURL, err := url.JoinPath(base.String(), "releases", "download", release.TagName, assetName)
	if err != nil {
		return "", fmt.Errorf("build release download URL: %w", err)
	}
	return downloadURL, nil
}
