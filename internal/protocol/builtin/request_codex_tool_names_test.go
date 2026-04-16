package builtin

import "testing"

func TestCodexToolNameAliases(t *testing.T) {
	aliases := buildCodexToolAliases([]string{
		"mcp__weather__a_very_long_tool_name_that_exceeds_sixty_four_characters_limit_here_test",
		"mcp__weather__a_very_long_tool_name_that_exceeds_sixty_four_characters_limit_here_test_2",
	})
	if len(aliases.OriginalToShort) != 2 || len(aliases.ShortToOriginal) != 2 {
		t.Fatalf("unexpected aliases: %+v", aliases)
	}
	for original, short := range aliases.OriginalToShort {
		if len(short) > codexToolNameLimit {
			t.Fatalf("short name too long: %s -> %s", original, short)
		}
		if aliases.ShortToOriginal[short] != original {
			t.Fatalf("reverse mapping lost original name: %+v", aliases)
		}
	}
}
