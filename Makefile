.DEFAULT_GOAL := build

WAILS ?= wails
WAILS_DIR := cmd/bork
WAILS_CMD := cd $(WAILS_DIR) && $(WAILS)
ifndef VERSION
COMMIT_HASH = $(shell git rev-parse --short=7 HEAD 2>/dev/null)
VERSION = $(shell date +%Y%m%d)-$(COMMIT_HASH)
CHECK_BUILD_VERSION = @test -n "$(COMMIT_HASH)" || (echo "Unable to determine the build version; run from a Git checkout or set VERSION" >&2; exit 1)
endif
PLATFORMS ?=
TAGS ?=
BUILD_FLAGS ?=
DEV_FLAGS ?=
ifndef MSIX_VERSION
MSIX_COMMIT_COUNT = $(shell git rev-list --count HEAD 2>/dev/null)
# Store package versions must be numeric; the app and artifact keep VERSION.
MSIX_VERSION = $(shell date +%Y.%-m%d).$(MSIX_COMMIT_COUNT).0
CHECK_MSIX_VERSION = @test -n "$(MSIX_COMMIT_COUNT)" || (echo "Unable to determine the MSIX version; run from a Git checkout or set MSIX_VERSION" >&2; exit 1)
endif
MAKEAPPX ?= C:/Program Files (x86)/Windows Kits/10/App Certification Kit/makeappx.exe
MSIX_NAME = bork-windows-amd64-$(VERSION).msix
MSIX_STAGE = build/msix/$(VERSION)
MSIX_OUTPUT = build/bin/$(MSIX_NAME)

comma := ,
ifneq ($(findstring $(comma),$(TAGS)),)
$(error TAGS must be space-separated, not comma-separated)
endif

ifneq ($(findstring -tags,$(BUILD_FLAGS)),)
$(error BUILD_FLAGS must not contain -tags; use TAGS instead)
endif
ifneq ($(findstring -tags,$(DEV_FLAGS)),)
$(error DEV_FLAGS must not contain -tags; use TAGS instead)
endif
ifneq ($(findstring -platform,$(BUILD_FLAGS)),)
$(error BUILD_FLAGS must not contain -platform; use PLATFORMS instead)
endif

TAG_FLAGS := $(if $(strip $(TAGS)),-tags "$(strip $(TAGS))")

ifneq ($(strip $(PLATFORMS)),)
PLATFORM_FLAGS := -platform "$(PLATFORMS)"
endif

.PHONY: build dev bindings frontend-deps typecheck-frontend prepare-packaging package-msix

bindings:
	$(WAILS_CMD) generate module

frontend-deps:
	node -e "const f=require('fs');process.exit(['vite','typescript'].every(p=>f.existsSync('frontend/node_modules/'+p+'/package.json'))?0:1)" || npm --prefix frontend ci

typecheck-frontend: bindings frontend-deps
	npm --prefix frontend run typecheck

prepare-packaging:
	mkdir -p build/darwin build/windows
	cp assets/brand/appicon.png build/appicon.png
	cp assets/brand/appicon.ico build/windows/icon.ico
	cp frontend/packaging/darwin/Info.plist build/darwin/Info.plist
	cp frontend/packaging/darwin/Info.dev.plist build/darwin/Info.dev.plist

build: prepare-packaging
	$(CHECK_BUILD_VERSION)
	$(WAILS_CMD) build -clean -trimpath -ldflags "-s -w -X bork/internal/app.BuildVersion=$(VERSION)" $(PLATFORM_FLAGS) $(TAG_FLAGS) $(BUILD_FLAGS)

package-msix: PLATFORM_FLAGS = -platform "windows/amd64"
package-msix: build
	$(CHECK_MSIX_VERSION)
	@test -f "$(MAKEAPPX)" || (echo "MakeAppx.exe was not found; set MAKEAPPX to its Windows SDK path" >&2; exit 1)
	mkdir -p "$(MSIX_STAGE)/Assets"
	cp build/bin/bork.exe "$(MSIX_STAGE)/bork.exe"
	cp frontend/packaging/windows/*.png "$(MSIX_STAGE)/Assets/"
	MSIX_VERSION="$(MSIX_VERSION)" powershell.exe -NoProfile -Command '$$manifest = [xml](Get-Content -Raw "frontend/packaging/windows/AppxManifest.xml"); $$manifest.Package.Identity.Version = $$env:MSIX_VERSION; $$manifest.Save([IO.Path]::GetFullPath("$(MSIX_STAGE)/AppxManifest.xml"))'
	powershell.exe -NoProfile -Command '& "$(MAKEAPPX)" pack /d "$(MSIX_STAGE)" /p "$(MSIX_OUTPUT)" /o; exit $$LASTEXITCODE'

dev: prepare-packaging
	$(WAILS_CMD) dev $(TAG_FLAGS) $(DEV_FLAGS)
