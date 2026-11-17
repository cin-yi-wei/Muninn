# Muninn 建置與安裝
VERSION := $(shell git describe --always --dirty --tags 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
PREFIX  ?= $(HOME)/.local

.PHONY: build test install restart

build:
	go build -ldflags "$(LDFLAGS)" -o bin/muninn ./cmd/muninn

test:
	go vet ./...
	go test -race ./...

# 安裝到 $(PREFIX)/bin：服務跑的是安裝好的版本，改程式重編不會影響正在跑的服務。
# 先寫暫存檔再改名，正在執行的舊版不會被寫壞。
install: test build
	mkdir -p $(PREFIX)/bin
	cp bin/muninn $(PREFIX)/bin/.muninn.new
	mv $(PREFIX)/bin/.muninn.new $(PREFIX)/bin/muninn
	@echo "已安裝 $(PREFIX)/bin/muninn（$(VERSION)）；systemctl --user restart muninn 套用"

restart:
	systemctl --user restart muninn
