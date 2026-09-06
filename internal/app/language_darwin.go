package app

/*
#cgo LDFLAGS: -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>

static void borkPreferredLanguage(char *buffer, int size) {
	CFArrayRef languages = CFLocaleCopyPreferredLanguages();
	if (!languages) return;
	if (CFArrayGetCount(languages) > 0) {
		CFStringRef language = (CFStringRef)CFArrayGetValueAtIndex(languages, 0);
		if (!CFStringGetCString(language, buffer, size, kCFStringEncodingUTF8)) buffer[0] = '\0';
	}
	CFRelease(languages);
}
*/
import "C"

func systemLanguage() string {
	var language [128]C.char
	C.borkPreferredLanguage(&language[0], C.int(len(language)))
	return C.GoString(&language[0])
}
