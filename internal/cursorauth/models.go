package cursorauth

func asString(value any) string {
	text, _ := value.(string)
	return text
}
