SHELL := /bin/bash

APP_NAME := lazy-jumphost
CMD_DIR := ./
DIST_DIR := dist
TAG ?=
RELEASE_FLAGS ?= --generate-notes

.PHONY: build test bundle bundle-darwin bundle-linux bundle-windows archives release clean

build:
	mkdir -p $(DIST_DIR)
	go build -o $(DIST_DIR)/$(APP_NAME) $(CMD_DIR)

test:
	GOCACHE=/tmp/go-cache go test ./...

bundle: bundle-darwin bundle-linux bundle-windows

bundle-darwin:
	mkdir -p $(DIST_DIR)/darwin/amd64 $(DIST_DIR)/darwin/arm64
	GOOS=darwin GOARCH=amd64 go build -o $(DIST_DIR)/darwin/amd64/$(APP_NAME) $(CMD_DIR)
	GOOS=darwin GOARCH=arm64 go build -o $(DIST_DIR)/darwin/arm64/$(APP_NAME) $(CMD_DIR)

bundle-linux:
	mkdir -p $(DIST_DIR)/linux/amd64 $(DIST_DIR)/linux/arm64
	GOOS=linux GOARCH=amd64 go build -o $(DIST_DIR)/linux/amd64/$(APP_NAME) $(CMD_DIR)
	GOOS=linux GOARCH=arm64 go build -o $(DIST_DIR)/linux/arm64/$(APP_NAME) $(CMD_DIR)

bundle-windows:
	mkdir -p $(DIST_DIR)/windows/amd64 $(DIST_DIR)/windows/arm64
	GOOS=windows GOARCH=amd64 go build -o $(DIST_DIR)/windows/amd64/$(APP_NAME).exe $(CMD_DIR)
	GOOS=windows GOARCH=arm64 go build -o $(DIST_DIR)/windows/arm64/$(APP_NAME).exe $(CMD_DIR)

archives: bundle
	tar -C $(DIST_DIR)/darwin/amd64 -czf $(DIST_DIR)/$(APP_NAME)_darwin_amd64.tar.gz $(APP_NAME)
	tar -C $(DIST_DIR)/darwin/arm64 -czf $(DIST_DIR)/$(APP_NAME)_darwin_arm64.tar.gz $(APP_NAME)
	tar -C $(DIST_DIR)/linux/amd64 -czf $(DIST_DIR)/$(APP_NAME)_linux_amd64.tar.gz $(APP_NAME)
	tar -C $(DIST_DIR)/linux/arm64 -czf $(DIST_DIR)/$(APP_NAME)_linux_arm64.tar.gz $(APP_NAME)
	zip -j $(DIST_DIR)/$(APP_NAME)_windows_amd64.zip $(DIST_DIR)/windows/amd64/$(APP_NAME).exe
	zip -j $(DIST_DIR)/$(APP_NAME)_windows_arm64.zip $(DIST_DIR)/windows/arm64/$(APP_NAME).exe
	@cd $(DIST_DIR) && if command -v shasum >/dev/null 2>&1; then shasum -a 256 $(APP_NAME)_* > SHA256SUMS.txt; else sha256sum $(APP_NAME)_* > SHA256SUMS.txt; fi

release: archives
	@if [ -z "$(TAG)" ]; then echo "TAG is required (e.g., make release TAG=v1.0.0)"; exit 1; fi
	gh release create $(TAG) $(DIST_DIR)/$(APP_NAME)_*.tar.gz $(DIST_DIR)/*.zip $(DIST_DIR)/SHA256SUMS.txt --title "$(TAG)" $(RELEASE_FLAGS)

clean:
	rm -rf $(DIST_DIR)
