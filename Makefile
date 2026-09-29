PLUGIN_NAME := ocfree
GO ?= go
DIST := dist

UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
	PLUGIN_EXT := dylib
else
	PLUGIN_EXT := so
endif

.PHONY: all build test verify clean

all: verify build

build:
	@mkdir -p $(DIST)
	CGO_ENABLED=1 $(GO) build -trimpath -buildmode=c-shared -o $(DIST)/$(PLUGIN_NAME).$(PLUGIN_EXT) .
	@rm -f $(DIST)/$(PLUGIN_NAME).h

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

verify: build
	@nm -D $(DIST)/$(PLUGIN_NAME).$(PLUGIN_EXT) | grep -q ' T cliproxy_plugin_init$$' \
		|| { echo "cliproxy_plugin_init is not exported"; exit 1; }
	@echo "ok: $(DIST)/$(PLUGIN_NAME).$(PLUGIN_EXT) exports cliproxy_plugin_init"

clean:
	rm -rf $(DIST)
