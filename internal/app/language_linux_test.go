package app

import "testing"

func TestSystemLanguageUsesMessagePreferences(t *testing.T) {
	for _, test := range []struct {
		name, all, messages, lang, preferred, want string
	}{
		{name: "message locale", messages: "zh_CN.UTF-8", lang: "en_US.UTF-8", want: "zh_CN.UTF-8"},
		{name: "all overrides messages", all: "en_GB.UTF-8", messages: "zh_CN.UTF-8", want: "en_GB.UTF-8"},
		{name: "preferred language", lang: "en_US.UTF-8", preferred: "zh_CN:en_US", want: "zh_CN"},
		{name: "C ignores preferred language", all: "C", preferred: "zh_CN", want: "en"},
		{name: "UTF-8 C ignores preferred language", all: "C.UTF-8", preferred: "zh_CN", want: "en"},
		{name: "no preference"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("LC_ALL", test.all)
			t.Setenv("LC_MESSAGES", test.messages)
			t.Setenv("LANG", test.lang)
			t.Setenv("LANGUAGE", test.preferred)
			if got := systemLanguage(); got != test.want {
				t.Fatalf("systemLanguage() = %q, want %q", got, test.want)
			}
		})
	}
}
