package builtin

import "strings"

type conversationReasoning struct {
	Subtype          string
	Text             string
	Signature        string
	EncryptedContent string
}

func newReasoningPart(subtype, text, signature, encrypted string) conversationPart {
	text = strings.TrimSpace(text)
	signature = strings.TrimSpace(signature)
	encrypted = strings.TrimSpace(encrypted)
	if text == "" && encrypted == "" {
		return conversationPart{}
	}
	return conversationPart{
		Kind: partKindReasoning,
		Reasoning: &conversationReasoning{
			Subtype:          strings.TrimSpace(subtype),
			Text:             text,
			Signature:        signature,
			EncryptedContent: encrypted,
		},
	}
}

func encodeCodexReasoningPart(reasoning *conversationReasoning) map[string]any {
	if reasoning == nil {
		return nil
	}
	return codexReasoningItem(reasoning.Text, reasoning.EncryptedContent)
}
