package app

import (
	"os"
	"strings"
)

func systemLanguage() string {
	language := ""
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if language = os.Getenv(name); language != "" {
			break
		}
	}
	// GNU message-language preferences override the locale, except when the
	// locale explicitly requests untranslated C/POSIX messages.
	locale, _, _ := strings.Cut(language, ".")
	if locale == "C" || locale == "POSIX" {
		return "en"
	}
	if preferred := os.Getenv("LANGUAGE"); preferred != "" {
		language, _, _ = strings.Cut(preferred, ":")
	}
	return language
}
