.DEFAULT_GOAL := build

WAILS ?= wails
WAILS_DIR := cmd/bork
WAILS_CMD := cd $(WAILS_DIR) && $(WAILS)
VERSION ?= $(shell node -p "require('./cmd/bork/wails.json').info.productVersion")
PLATFORMS ?=
TAGS ?=
APP_ARGS ?=
BUILD_FLAGS ?=
DEV_FLAGS ?=

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

HOST_GOOS := $(shell go env GOOS)
BUILD_TARGETS := $(if $(strip $(PLATFORMS)),$(PLATFORMS),$(HOST_GOOS))
BUILD_WINTUN := $(findstring windows,$(BUILD_TARGETS))
BUILD_WINTUN_DEP := $(if $(BUILD_WINTUN),prepare-wintun)
BUILD_TAGS := $(strip $(TAGS) $(if $(BUILD_WINTUN),wintun_embed))
BUILD_TAG_FLAGS := $(if $(BUILD_TAGS),-tags "$(BUILD_TAGS)")
DEV_WINTUN_DEP := $(if $(filter windows,$(HOST_GOOS)),prepare-wintun)
DEV_TAGS := $(strip $(TAGS) $(if $(filter windows,$(HOST_GOOS)),wintun_embed))
DEV_TAG_FLAGS := $(if $(DEV_TAGS),-tags "$(DEV_TAGS)")

ifneq ($(strip $(PLATFORMS)),)
PLATFORM_FLAGS := -platform "$(PLATFORMS)"
endif

ifneq ($(strip $(APP_ARGS)),)
APP_FLAGS := -appargs "$(APP_ARGS)"
endif

.PHONY: build dev bindings frontend-deps typecheck-frontend prepare-packaging prepare-wintun

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

prepare-wintun:
	go run ./cmd/wintun

build: prepare-packaging $(BUILD_WINTUN_DEP)
	$(WAILS_CMD) build -clean -trimpath -ldflags "-s -w -X main.version=$(VERSION)" $(PLATFORM_FLAGS) $(BUILD_TAG_FLAGS) $(BUILD_FLAGS)

dev: prepare-packaging $(DEV_WINTUN_DEP)
	$(WAILS_CMD) dev $(APP_FLAGS) $(DEV_TAG_FLAGS) $(DEV_FLAGS)
