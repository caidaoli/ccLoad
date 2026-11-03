package app

import (
	"log"
	"sync/atomic"
)

// privacyStats accumulates redaction activity since process start. It holds
// counts only: no original values, placeholders or scope identities.
type privacyStats struct {
	maskedRequests, maskedSecret, maskedPersonal, maskedWord, maskedCustom atomic.Uint64
	restored, unrestored, blocked                                          atomic.Uint64
}

// finish records one HTTP request or Responses WebSocket turn. The masked
// counts are the union of placeholders every attempt actually sent, so retries
// do not inflate them.
func (p *privacyStats) finish(output *privacyResponseWriter, requestID int64) {
	count := output.observer.UnrestoredTokens()
	p.restored.Add(output.observer.RestoredTokens())
	p.unrestored.Add(count)
	if sent := output.session.SentCounts(); sent.Total() > 0 {
		p.maskedRequests.Add(1)
		p.maskedSecret.Add(uint64(sent.Secret))
		p.maskedPersonal.Add(uint64(sent.Personal))
		p.maskedWord.Add(uint64(sent.Word))
		p.maskedCustom.Add(uint64(sent.Custom))
	}
	// Request diagnostics intentionally exclude token text, original values and scope.
	if count > 0 {
		log.Printf("[WARN] privacy response has unrestored placeholders request_id=%d count=%d", requestID, count)
	}
}

type privacyRuntimeMetrics struct {
	Enabled                bool   `json:"enabled"`
	MaskedRequests         uint64 `json:"masked_requests"`
	MaskedSecret           uint64 `json:"masked_secret"`
	MaskedPersonal         uint64 `json:"masked_personal"`
	MaskedWord             uint64 `json:"masked_word"`
	MaskedCustom           uint64 `json:"masked_custom"`
	RestoredPlaceholders   uint64 `json:"restored_placeholders"`
	UnrestoredPlaceholders uint64 `json:"unrestored_placeholders"`
	BlockedRequests        uint64 `json:"blocked_requests"`
	// The dictionary excludes one-off engines used by requests without identity.
	DictionaryScopes  int `json:"dictionary_scopes"`
	DictionaryEntries int `json:"dictionary_entries"`
	DictionaryBytes   int `json:"dictionary_bytes"`
}

func (s *Server) privacyRuntimeMetrics() privacyRuntimeMetrics {
	scopes, entries, size := s.privacyEngine.Stats()
	p := &s.privacyStats
	return privacyRuntimeMetrics{
		Enabled:                s.privacyPolicy.Load().Enabled(),
		MaskedRequests:         p.maskedRequests.Load(),
		MaskedSecret:           p.maskedSecret.Load(),
		MaskedPersonal:         p.maskedPersonal.Load(),
		MaskedWord:             p.maskedWord.Load(),
		MaskedCustom:           p.maskedCustom.Load(),
		RestoredPlaceholders:   p.restored.Load(),
		UnrestoredPlaceholders: p.unrestored.Load(),
		BlockedRequests:        p.blocked.Load(),
		DictionaryScopes:       scopes,
		DictionaryEntries:      entries,
		DictionaryBytes:        size,
	}
}
