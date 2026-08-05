package app

import "ccLoad/internal/model"

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
