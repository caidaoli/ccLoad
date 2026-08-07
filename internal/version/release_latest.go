package version

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	githubLatestReleaseURL      = "https://github.com/caidaoli/ccLoad/releases/latest"
	githubReleasesAPIURL        = "https://api.github.com/repos/caidaoli/ccLoad/releases?per_page=100"
	githubDownloadBaseURL       = "https://github.com/caidaoli/ccLoad/releases/download"
	monlorLatestReleaseURL      = "https://gh.monlor.com/https://github.com/caidaoli/ccLoad/releases/latest"
	monlorDownloadBaseURL       = "https://gh.monlor.com/https://github.com/caidaoli/ccLoad/releases/download"
	fastgitLatestReleaseURL     = "https://fastgit.cc/https://github.com/caidaoli/ccLoad/releases/latest"
	fastgitDownloadBaseURL      = "https://fastgit.cc/https://github.com/caidaoli/ccLoad/releases/download"
	ghfastLatestReleaseURL      = "https://ghfast.top/https://github.com/caidaoli/ccLoad/releases/latest"
	ghfastDownloadBaseURL       = "https://ghfast.top/https://github.com/caidaoli/ccLoad/releases/download"
	releaseLatestDownloadSuffix = "/releases/latest/download"
	releaseTagPathMarker        = "/releases/tag/"
	releaseListMaxBodyBytes     = 2 << 20
)

// GitHubRelease describes a resolved GitHub release.
type GitHubRelease struct {
	TagName    string
	HTMLURL    string
	Prerelease bool
}

// ReleaseChannel controls which published versions are eligible for updates.
type ReleaseChannel string

const (
	// ReleaseChannelStable accepts stable releases only.
	ReleaseChannelStable ReleaseChannel = "stable"
	// ReleaseChannelPreview accepts stable and prerelease versions.
	ReleaseChannelPreview ReleaseChannel = "preview"
)

// ParseReleaseChannel validates a persisted update channel.
func ParseReleaseChannel(value string) (ReleaseChannel, error) {
	channel := ReleaseChannel(strings.ToLower(strings.TrimSpace(value)))
	switch channel {
	case ReleaseChannelStable, ReleaseChannelPreview:
		return channel, nil
	default:
		return "", fmt.Errorf("release channel must be stable or preview")
	}
}

// ReleaseSource describes one complete release endpoint.
type ReleaseSource struct {
	Name            string
	LatestURL       string
	ReleasesURL     string
	DownloadBaseURL string
}

func releaseSources(customBaseURL string) ([]ReleaseSource, error) {
	customBaseURL = strings.TrimRight(strings.TrimSpace(customBaseURL), "/")
	if customBaseURL == "" {
		return []ReleaseSource{
			{
				Name:            "gh.monlor.com",
				LatestURL:       monlorLatestReleaseURL,
				DownloadBaseURL: monlorDownloadBaseURL,
			},
			{
				Name:            "fastgit.cc",
				LatestURL:       fastgitLatestReleaseURL,
				DownloadBaseURL: fastgitDownloadBaseURL,
			},
			{
				Name:            "ghfast.top",
				LatestURL:       ghfastLatestReleaseURL,
				DownloadBaseURL: ghfastDownloadBaseURL,
			},
			{
				Name:            "github.com",
				LatestURL:       githubLatestReleaseURL,
				ReleasesURL:     githubReleasesAPIURL,
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
		ReleasesURL:     githubReleasesAPIURL,
		DownloadBaseURL: repositoryBaseURL + "/releases/download",
	}}, nil
}

func resolveLatestRelease(ctx context.Context, client *http.Client, sources []ReleaseSource, channel ReleaseChannel) (GitHubRelease, error) {
	if channel == "" {
		channel = ReleaseChannelStable
	}
	if _, err := ParseReleaseChannel(string(channel)); err != nil {
		return GitHubRelease{}, err
	}

	var sourceErrors []error
	for _, source := range sources {
		var (
			release GitHubRelease
			err     error
		)
		switch channel {
		case ReleaseChannelStable:
			if strings.TrimSpace(source.LatestURL) == "" {
				continue
			}
			release, err = fetchLatestRelease(ctx, client, source.LatestURL)
		case ReleaseChannelPreview:
			if strings.TrimSpace(source.ReleasesURL) == "" {
				continue
			}
			release, err = fetchPreviewRelease(ctx, client, source.ReleasesURL)
		}
		if err == nil {
			return release, nil
		}
		sourceErrors = append(sourceErrors, fmt.Errorf("%s: %w", source.Name, err))
	}
	if len(sourceErrors) == 0 {
		return GitHubRelease{}, fmt.Errorf("no release metadata source configured for %s channel", channel)
	}
	return GitHubRelease{}, fmt.Errorf("resolve %s release: %w", channel, errors.Join(sourceErrors...))
}

type publishedRelease struct {
	TagName     string `json:"tag_name"`
	HTMLURL     string `json:"html_url"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	PublishedAt string `json:"published_at"`
}

func fetchPreviewRelease(ctx context.Context, client *http.Client, releasesURL string) (GitHubRelease, error) {
	if client == nil {
		client = http.DefaultClient
	}

	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, releasesURL, nil)
	if err != nil {
		return GitHubRelease{}, fmt.Errorf("create releases request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", OutboundUserAgent())

	resp, err := client.Do(req)
	if err != nil {
		return GitHubRelease{}, fmt.Errorf("fetch releases: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return GitHubRelease{}, fmt.Errorf("fetch releases: status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, releaseListMaxBodyBytes+1))
	if err != nil {
		return GitHubRelease{}, fmt.Errorf("read releases: %w", err)
	}
	if len(data) > releaseListMaxBodyBytes {
		return GitHubRelease{}, fmt.Errorf("releases response exceeds %d bytes", releaseListMaxBodyBytes)
	}

	var releases []publishedRelease
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(&releases); err != nil {
		return GitHubRelease{}, fmt.Errorf("decode releases response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return GitHubRelease{}, fmt.Errorf("decode releases response: multiple JSON values")
		}
		return GitHubRelease{}, fmt.Errorf("decode releases response trailing data: %w", err)
	}

	var selected GitHubRelease
	for _, release := range releases {
		if release.Draft || strings.TrimSpace(release.PublishedAt) == "" {
			continue
		}
		tag := strings.TrimSpace(release.TagName)
		if _, ok := normalizeSemanticVersion(tag); !ok {
			continue
		}
		if selected.TagName == "" || compareSemanticVersions(tag, selected.TagName) > 0 {
			selected = GitHubRelease{
				TagName:    tag,
				HTMLURL:    strings.TrimSpace(release.HTMLURL),
				Prerelease: release.Prerelease,
			}
		}
	}
	if selected.TagName == "" {
		return GitHubRelease{}, fmt.Errorf("releases response contains no published semantic versions")
	}
	return selected, nil
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
