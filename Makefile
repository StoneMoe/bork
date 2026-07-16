.DEFAULT_GOAL := build

WAILS ?= wails
VERSION ?= $(shell node -p "require('./wails.json').info.productVersion")
PLATFORMS ?=
APP_ARGS ?=
BUILD_FLAGS ?=
DEV_FLAGS ?=

ifeq ($(OS),Windows_NT)
OS_BUILD_FLAGS := -windowsconsole
endif

ifneq ($(strip $(PLATFORMS)),)
PLATFORM_FLAGS := -platform "$(PLATFORMS)"
endif

ifneq ($(strip $(APP_ARGS)),)
APP_FLAGS := -appargs "$(APP_ARGS)"
endif

.PHONY: build dev bindings test-frontend typecheck-frontend prepare-packaging

bindings:
	$(WAILS) generate module

test-frontend: bindings
	npm --prefix frontend test

typecheck-frontend: bindings
	npm --prefix frontend run typecheck

prepare-packaging:
	mkdir -p build/darwin
	cp frontend/packaging/darwin/Info.plist build/darwin/Info.plist
	cp frontend/packaging/darwin/Info.dev.plist build/darwin/Info.dev.plist

build: prepare-packaging
	$(WAILS) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" $(OS_BUILD_FLAGS) $(PLATFORM_FLAGS) $(BUILD_FLAGS)

dev: prepare-packaging
	$(WAILS) dev $(APP_FLAGS) $(DEV_FLAGS)
