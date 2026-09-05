package main

import (
	"fmt"
	"strings"
	"testing"
)

const validDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestParseManifest_accepts_valid_typed_document(t *testing.T) {
	// Given
	document := manifestDocument("nfsdk/include/header.h", validDigest)

	// When
	manifest, err := parseManifest(strings.NewReader(document))

	// Then
	if err != nil {
		t.Fatalf("parse valid manifest: %v", err)
	}
	if manifest.version != "1.2.3" {
		t.Fatalf("version = %q, want 1.2.3", manifest.version)
	}
	if len(manifest.files) != 1 || manifest.files[0].path.String() != "nfsdk/include/header.h" {
		t.Fatalf("unexpected locked files: %#v", manifest.files)
	}
}

func TestParseManifest_rejects_unknown_or_trailing_JSON(t *testing.T) {
	tests := []struct {
		name     string
		document string
	}{
		{name: "unknown field", document: strings.Replace(manifestDocument("file.bin", validDigest), `"schema":1`, `"schema":1,"extra":true`, 1)},
		{name: "trailing value", document: manifestDocument("file.bin", validDigest) + `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// When
			_, err := parseManifest(strings.NewReader(test.document))

			// Then
			if err == nil {
				t.Fatal("parse succeeded, want error")
			}
		})
	}
}

func TestParseManifest_rejects_invalid_SHA256(t *testing.T) {
	tests := []struct {
		name   string
		digest string
	}{
		{name: "short", digest: strings.Repeat("a", 63)},
		{name: "uppercase", digest: strings.Repeat("A", 64)},
		{name: "non hexadecimal", digest: strings.Repeat("g", 64)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// When
			_, err := parseManifest(strings.NewReader(manifestDocument("file.bin", test.digest)))

			// Then
			if err == nil {
				t.Fatal("parse succeeded, want error")
			}
		})
	}
}

func TestParseManifest_rejects_unsafe_entry_paths(t *testing.T) {
	tests := []string{
		"",
		"/absolute",
		"C:/drive-qualified",
		`dir\file`,
		".",
		"..",
		"dir//file",
		"dir/./file",
		"dir/../file",
		"dir/",
	}
	for _, entryPath := range tests {
		t.Run(fmt.Sprintf("path_%q", entryPath), func(t *testing.T) {
			// When
			_, err := parseManifest(strings.NewReader(manifestDocument(entryPath, validDigest)))

			// Then
			if err == nil {
				t.Fatal("parse succeeded, want error")
			}
		})
	}
}

func TestParseManifest_rejects_duplicate_entry_paths(t *testing.T) {
	// Given
	document := strings.Replace(
		manifestDocument("file.bin", validDigest),
		`]}`,
		`,{"path":"file.bin","sha256":"`+validDigest+`"}]}`,
		1,
	)

	// When
	_, err := parseManifest(strings.NewReader(document))

	// Then
	if err == nil {
		t.Fatal("parse succeeded, want error")
	}
}

func manifestDocument(entryPath, digest string) string {
	return fmt.Sprintf(
		`{"schema":1,"version":"1.2.3","source_url":"https://example.invalid/sdk.zip","archive":{"name":"sdk.zip","sha256":"%s"},"platform":"windows/amd64","files":[{"path":%q,"sha256":"%s"}]}`,
		validDigest,
		entryPath,
		digest,
	)
}
