BINARY   := elasticdata-inandout
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64

.PHONY: build test release clean

# 构建当前平台
build:
	go build -o $(BINARY) .

test:
	go vet ./... && go test ./...

# 交叉编译多平台多架构,产物输出到 dist/
release:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		ext=$$([ "$$os" = "windows" ] && echo .exe); \
		echo "==> $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -ldflags="-s -w" \
			-o dist/$(BINARY)-$$os-$$arch$$ext .; \
	done
	@ls -lh dist/

clean:
	rm -rf dist $(BINARY)
