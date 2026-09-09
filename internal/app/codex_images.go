package app

import "strings"

// GPT Image 2.5 uses the Responses image tool, as in CLIProxyAPI PR #5653
// (d673ba822ece5ff5fba703e4ba8060fc6d4bd93e).
func codexImageUsesResponses(model string) bool {
	canonical, _ := canonicalCodexImageModel(model)
	canonical = strings.TrimSuffix(canonical, "-2026-09-08")
	return canonical == "gpt-image-2.5-flare" || canonical == "gpt-image-2.5-sunburst"
}

func buildCodexImagesResponsesRequest(raw []byte, imageModel string) ([]byte, error) {
	canonical, supported := canonicalCodexImageModel(imageModel)
	if !supported || !codexImageUsesResponses(canonical) {
		return nil, codexImageUnsupportedModelError(imageModel)
	}
	// Channel selection has already resolved the image model's routing prefix.
	return buildImagesResponsesRequest(raw, "gpt-5.6-luna", canonical)
}
