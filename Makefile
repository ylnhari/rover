BINARY  := rover
DIST    := dist
VERSION ?= dev
LDFLAGS := -s -w -X github.com/ylnhari/rover/internal/version.Build=$(VERSION)

PLATFORMS := \
	linux/amd64 \
	linux/arm64 \
	darwin/amd64 \
	darwin/arm64 \
	windows/amd64

.PHONY: build test vet lint clean dist release

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) .

test:
	go test ./... -v -count=1 -timeout 60s

vet:
	go vet ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf $(BINARY) $(BINARY).exe $(DIST)

dist: clean
	mkdir -p $(DIST)
	$(foreach platform,$(PLATFORMS), \
		$(eval GOOS=$(word 1,$(subst /, ,$(platform)))) \
		$(eval GOARCH=$(word 2,$(subst /, ,$(platform)))) \
		$(eval EXT=$(if $(filter windows,$(GOOS)),.exe,)) \
		GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" \
			-o $(DIST)/$(BINARY)-$(GOOS)-$(GOARCH)$(EXT) . && \
		sha256sum $(DIST)/$(BINARY)-$(GOOS)-$(GOARCH)$(EXT) > \
			$(DIST)/$(BINARY)-$(GOOS)-$(GOARCH)$(EXT).sha256 ; \
	)

release: test dist

.DEFAULT_GOAL := build
