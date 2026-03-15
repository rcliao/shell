.PHONY: build run test vet clean watch skills install-skills

BINARY := shell
PKG := ./cmd/shell
SKILLS_DIR := $(HOME)/.shell/skills

build:
	go build -o $(BINARY) $(PKG)

run: build
	./$(BINARY) daemon

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f $(BINARY)

watch: build
	./$(BINARY) daemon --watch

init: build
	./$(BINARY) init

# Build skill binaries
skills:
	go build -o skills/web-search/scripts/web-search ./cmd/shell-search
	go build -o skills/generate-image/scripts/generate-image ./cmd/shell-imagen

# Install skills to ~/.shell/skills/
install-skills: skills
	@for skill in web-search generate-image hello weather summarize; do \
		mkdir -p $(SKILLS_DIR)/$$skill/scripts; \
		cp skills/$$skill/SKILL.md $(SKILLS_DIR)/$$skill/; \
		if [ -d skills/$$skill/scripts ]; then \
			cp skills/$$skill/scripts/* $(SKILLS_DIR)/$$skill/scripts/; \
		fi; \
	done
	@echo "Skills installed to $(SKILLS_DIR)"
