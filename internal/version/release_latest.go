package version

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	githubLatestReleaseURL      = "https://github.com/caidaoli/ccLoad/releases/latest"
	githubDownloadBaseURL       = "https://github.com/caidaoli/ccLoad/releases/download"
	ghproxyLatestReleaseURL     = "https://ghproxy.net/https://github.com/caidaoli/ccLoad/releases/latest"
	ghproxyDownloadBaseURL      = "https://ghproxy.net/https://github.com/caidaoli/ccLoad/releases/download"
	releaseLatestDownloadSuffix = "/releases/latest/download"
	releaseTagPathMarker        = "/releases/tag/"
)

// ReleaseSource describes one complete release endpoint.
type ReleaseSource struct {
	Name            string
	LatestURL       string
	DownloadBaseURL string
}

func releaseSources(customBaseURL string) ([]ReleaseSource, error) {
	customBaseURL = strings.TrimRight(strings.TrimSpace(customBaseURL), "/")
	if customBaseURL == "" {
		return []ReleaseSource{
			{
				Name:            "ghproxy.net",
				LatestURL:       ghproxyLatestReleaseURL,
				DownloadBaseURL: ghproxyDownloadBaseURL,
			},
			{
				Name:            "github.com",
				LatestURL:       githubLatestReleaseURL,
				DownloadBaseURL: githubDownloadBaseURL,
			},
		}, nil
	}

	if !strings.HasSuffix(customBaseURL, releaseLatestDownloadSuffix) {
		return nil, fmt.Errorf("CCLOAD_RELEASE_BASE_URL must end with %s", releaseLatestDownloadSuffix)
	}
	parsed, err := url.Parse(customBaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("invalid CCLOAD_RELEASE_BASE_URL %q", customBaseURL)
	}

	repositoryBaseURL := strings.TrimSuffix(customBaseURL, releaseLatestDownloadSuffix)
	return []ReleaseSource{{
		Name:            "custom",
		LatestURL:       repositoryBaseURL + "/releases/latest",
		DownloadBaseURL: repositoryBaseURL + "/releases/download",
	}}, nil
}

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

func releaseDownloadURL(source ReleaseSource, tag, assetName string) (string, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return "", fmt.Errorf("latest release missing tag_name")
	}
	assetName = strings.TrimSpace(assetName)
	if assetName == "" {
		return "", fmt.Errorf("release %s has empty asset name", tag)
	}
	downloadBaseURL := strings.TrimRight(strings.TrimSpace(source.DownloadBaseURL), "/")
	if downloadBaseURL == "" {
		return "", fmt.Errorf("release source %q has empty download base URL", source.Name)
	}
	parsed, err := url.Parse(downloadBaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("release source %q has invalid download base URL %q", source.Name, source.DownloadBaseURL)
	}
	return downloadBaseURL + "/" + url.PathEscape(tag) + "/" + url.PathEscape(assetName), nil
}
