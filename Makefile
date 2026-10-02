BIN    ?= zk-graph
PREFIX ?= $(HOME)/.local
NVIM   ?= $(HOME)/.config/nvim
# desktop,production: plain `go build` of a Wails v2 app (no wails CLI needed)
# webkit2_41: link webkit2gtk-4.1 (4.0 is gone from current distros)
TAGS   := desktop,production,webkit2_41

.PHONY: build debug test deps install install-plugin clean

build:
	go build -tags $(TAGS) -trimpath -ldflags "-s -w" -o $(BIN) .

# right-click > Inspect Element in the window (WebKit inspector)
debug:
	go build -tags $(TAGS),devtools -o $(BIN) .

test:
	go test ./internal/...

deps:
	go mod tidy

install: build
	install -Dm755 $(BIN) $(PREFIX)/bin/$(BIN)

install-plugin:
	install -Dm644 nvim/lua/zk-graph.lua $(NVIM)/lua/zk-graph.lua

clean:
	rm -f $(BIN)
