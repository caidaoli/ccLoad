package app

import (
	"context"
	"time"

	"ccLoad/internal/model"
)

type channelURLFailure struct {
	statusCode          int
	body                []byte
	network             bool
	antigravityCapacity bool
}

type channelURLRetryDecision struct {
	retry         bool
	delay         time.Duration
	capacity      bool
	firstCapacity bool
}

// channelURLAttemptPolicy owns provider-specific URL fallback state for one
// logical request. Callers still own transport and response rendering.
type channelURLAttemptPolicy struct {
	antigravityCapacityFailures int
	antigravityCapacityRetries  int
	antigravityCapacityObserved bool
}

func (p *channelURLAttemptPolicy) decide(
	cfg *model.Config,
	hasNext bool,
	failure channelURLFailure,
) channelURLRetryDecision {
	if cfg == nil || !cfg.UsesAntigravityOAuth() {
		return channelURLRetryDecision{}
	}

	capacity := failure.antigravityCapacity ||
		isAntigravityModelCapacityExhausted(failure.statusCode, failure.body)
	if capacity {
		p.antigravityCapacityFailures++
		decision := channelURLRetryDecision{
			capacity:      true,
			firstCapacity: !p.antigravityCapacityObserved,
		}
		p.antigravityCapacityObserved = true
		if hasNext && p.antigravityCapacityFailures < antigravityModelCapacityAttempts {
			p.antigravityCapacityRetries++
			decision.retry = true
			decision.delay = antigravityBaseURLFallbackDelay
		}
		return decision
	}

	p.antigravityCapacityFailures = 0
	if hasNext && (failure.network || shouldFallbackAntigravityBaseURL(failure.statusCode, failure.body)) {
		return channelURLRetryDecision{retry: true, delay: antigravityBaseURLFallbackDelay}
	}
	return channelURLRetryDecision{}
}

func waitForChannelURLRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func configuredURLFrom(configured model.ChannelURLs, index int, runtimeURL string) model.ChannelURL {
	if index >= 0 && index < len(configured) && configured[index].RuntimeURL() == runtimeURL {
		return configured[index]
	}
	for _, entry := range configured {
		if entry.RuntimeURL() == runtimeURL {
			return entry
		}
	}
	return model.ChannelURL{
		URL:   model.StripExactUpstreamURLMarker(runtimeURL),
		Exact: model.HasExactUpstreamURLMarker(runtimeURL),
	}
}

func prioritizeProtocolURLs(sorted []sortedURL, configured model.ChannelURLs, automaticFirst bool) []sortedURL {
	preferred := make([]sortedURL, 0, len(sorted))
	fallback := make([]sortedURL, 0, len(sorted))
	for _, candidate := range sorted {
		entry := configuredURLFrom(configured, candidate.idx, candidate.url)
		if entry.UsesAutomaticProtocolDetection() == automaticFirst {
			preferred = append(preferred, candidate)
			continue
		}
		fallback = append(fallback, candidate)
	}
	return append(preferred, fallback...)
}

func prioritizeAutomaticProtocolURLs(sorted []sortedURL, configured model.ChannelURLs) []sortedURL {
	return prioritizeProtocolURLs(sorted, configured, true)
}

func prioritizeDeclaredProtocolURLs(sorted []sortedURL, configured model.ChannelURLs) []sortedURL {
	return prioritizeProtocolURLs(sorted, configured, false)
}

// orderURLsWithSelector 返回用于故障切换的URL尝试顺序。
// 当 selector 可用且存在多个URL时，优先用加权随机选首跳，其余URL按排序结果兜底。
func orderURLsWithSelector(selector *URLSelector, channelID int64, urls []string) []sortedURL {
	if len(urls) == 0 {
		return nil
	}
	if len(urls) == 1 {
		return []sortedURL{{url: urls[0], idx: 0}}
	}
	if selector == nil {
		ordered := make([]sortedURL, len(urls))
		for i, u := range urls {
			ordered[i] = sortedURL{url: u, idx: i}
		}
		return ordered
	}

	sortedURLs := selector.SortURLs(channelID, urls)
	if len(sortedURLs) <= 1 {
		return sortedURLs
	}

	preferredURL, _ := selector.SelectURL(channelID, urls)
	for i, entry := range sortedURLs {
		if entry.url != preferredURL {
			continue
		}
		if i == 0 {
			return sortedURLs
		}

		reordered := make([]sortedURL, 0, len(sortedURLs))
		reordered = append(reordered, entry)
		reordered = append(reordered, sortedURLs[:i]...)
		reordered = append(reordered, sortedURLs[i+1:]...)
		return reordered
	}

	return sortedURLs
}

// orderChannelAttemptURLs is the single ordering policy used by proxy and
// admin test traffic. Antigravity provider priority is fixed; other channels
// keep latency-aware selection.
func orderChannelAttemptURLs(selector *URLSelector, cfg *model.Config, urls []string) []sortedURL {
	if cfg != nil && cfg.UsesAntigravityOAuth() {
		return orderURLsInConfiguredOrder(selector, cfg.ID, urls)
	}
	channelID := int64(0)
	if cfg != nil {
		channelID = cfg.ID
	}
	return orderURLsWithSelector(selector, channelID, urls)
}

// orderURLsInConfiguredOrder preserves provider-defined fallback priority while
// still honoring the selector's manually disabled URL state.
func orderURLsInConfiguredOrder(selector *URLSelector, channelID int64, urls []string) []sortedURL {
	ordered := make([]sortedURL, 0, len(urls))
	for i, rawURL := range urls {
		if selector != nil && selector.IsDisabled(channelID, rawURL) {
			continue
		}
		ordered = append(ordered, sortedURL{url: rawURL, idx: i})
	}
	return ordered
}
