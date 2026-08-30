package netfilter

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"bork/internal/gameproxy/intercept"
)

type bridgeFactoryContract interface {
	Supported() bool
	EnsureAvailable(context.Context) error
	New(context.Context, []string) (intercept.Bridge, error)
}

var _ bridgeFactoryContract = NewFactory()

func TestFactoryWindowsSource_has_exact_build_and_embed_contract(t *testing.T) {
	// Given
	const filename = "factory_windows.go"
	const buildConstraint = "//go:build windows && amd64 && cgo && netfilter_sdk"
	const embedDirective = "//go:embed sdk/nfsdk/wfp/bin/release_c_api/x64/nfapi.dll"

	// When
	contents, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), filename, contents, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}

	// Then
	firstLine, _, _ := strings.Cut(string(contents), "\n")
	if firstLine != buildConstraint {
		t.Fatalf("build constraint = %q, want %q", firstLine, buildConstraint)
	}
	var embeds []string
	for _, group := range parsed.Comments {
		for _, comment := range group.List {
			if strings.HasPrefix(comment.Text, "//go:embed ") {
				embeds = append(embeds, comment.Text)
			}
		}
	}
	if len(embeds) != 1 || embeds[0] != embedDirective {
		t.Fatalf("embed directives = %q, want [%q]", embeds, embedDirective)
	}
	assertFactorySourceHasNoDriverPayload(t, string(contents))
}

func TestFactoryUnsupportedSource_has_exact_complement_without_native_references(t *testing.T) {
	// Given
	const filename = "factory_unsupported.go"
	const buildConstraint = "//go:build !windows || !amd64 || !cgo || !netfilter_sdk"

	// When
	contents, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}

	// Then
	firstLine, _, _ := strings.Cut(string(contents), "\n")
	if firstLine != buildConstraint {
		t.Fatalf("build constraint = %q, want %q", firstLine, buildConstraint)
	}
	source := strings.ToLower(string(contents))
	for _, forbidden := range []string{"go:embed", `"embed"`, "sdk/", "newnativebackend", "newbridge"} {
		if strings.Contains(source, forbidden) {
			t.Errorf("unsupported factory source contains forbidden reference %q", forbidden)
		}
	}
	assertFactorySourceHasNoDriverPayload(t, source)
}

func assertFactorySourceHasNoDriverPayload(t *testing.T, source string) {
	t.Helper()
	source = strings.ToLower(source)
	for _, forbidden := range []string{".sys", "nfregdrv"} {
		if strings.Contains(source, forbidden) {
			t.Errorf("factory source contains forbidden payload reference %q", forbidden)
		}
	}
}
