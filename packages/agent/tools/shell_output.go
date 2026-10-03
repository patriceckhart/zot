package tools

import "unicode/utf8"

func shellUTF8Head(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	start := len(data) - 1
	for start > 0 && !utf8.RuneStart(data[start]) {
		start--
	}
	if !utf8.FullRune(data[start:]) {
		data = data[:start]
	}
	return string(data)
}

func shellUTF8Tail(data []byte) string {
	for len(data) > 0 && !utf8.RuneStart(data[0]) {
		data = data[1:]
	}
	return string(data)
}
