.PHONY: build run test vet clean watch skills install-skills bench bench-pika bench-umb bench-cv bench-all verify-no-pii

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
	rm -f $(BINARY) shell-bench

# Cycle 144: pre-commit guard — fails if any STAGED file contains real-user
# tokens (family names, persona nicknames, specific health details). Run
# before `git commit` to avoid leaking production conversation data into
# the public repo.
#
# Run on staged files only (so untracked private artifacts like .evolve/cycles/
# don't trip it). To check the full working tree, pass FILES=$$(git ls-files).
verify-no-pii:
	@FILES="$${FILES:-$$(git diff --cached --name-only --diff-filter=ACM)}"; \
	if [ -z "$$FILES" ]; then echo "verify-no-pii: no staged files to check"; exit 0; fi; \
	HITS=$$(echo "$$FILES" | xargs grep -lE 'mami|papi|Jennifer|rcliao01|皮卡|umbreonmini.*妹|Pikamini.*plush|cashew|過敏|蕁麻疹|dairy.*sensit|歐里安|Cream Pan|抹茶.*明太子' 2>/dev/null || true); \
	if [ -n "$$HITS" ]; then \
	  echo "FAIL — staged files contain restricted tokens:"; \
	  echo "$$HITS" | sed 's/^/  /'; \
	  echo ""; \
	  echo "Either move these to gitignored paths (.evolve/cycles/) or sanitize before committing."; \
	  exit 1; \
	fi; \
	echo "verify-no-pii: OK ($$(echo "$$FILES" | wc -l | tr -d ' ') staged file(s) clean)"

# LLM-free agent OS benchmarks. Writes JSON reports under .evolve/cycles/.
BENCH_SINCE ?= 7d
BENCH_DATE  := $(shell date +%Y-%m-%d)

bench: bench-pika bench-umb bench-cv

# Combined 6-dim dashboard (cycle 59): single-command regression detection.
bench-all:
	go build -o shell-bench ./cmd/shell-bench
	./shell-bench all-dims \
	  --out .evolve/cycles/$(BENCH_DATE)-bench-all-dims.json

bench-cv:
	go build -o shell-bench ./cmd/shell-bench
	./shell-bench cv \
	  --out .evolve/cycles/$(BENCH_DATE)-bench-cv.json

bench-pika:
	go build -o shell-bench ./cmd/shell-bench
	./shell-bench all --agent pikamini    --since $(BENCH_SINCE) \
	  --out .evolve/cycles/$(BENCH_DATE)-bench-pikamini.json

bench-umb:
	go build -o shell-bench ./cmd/shell-bench
	./shell-bench all --agent umbreonmini --since $(BENCH_SINCE) \
	  --out .evolve/cycles/$(BENCH_DATE)-bench-umbreonmini.json

watch: build
	./$(BINARY) daemon --watch

init: build
	./$(BINARY) init

# Build skill binaries
skills:
	go build -o skills/web-search/scripts/web-search ./cmd/shell-search
	go build -o skills/generate-image/scripts/generate-image ./cmd/shell-imagen
	go build -o skills/browser/scripts/browser ./cmd/shell-browser

# Install skills to ~/.shell/skills/
install-skills: skills
	@for skill in web-search generate-image browser hello weather summarize shell-pm shell-tunnel shell-relay shell-schedule shell-remember shell-task; do \
		mkdir -p $(SKILLS_DIR)/$$skill/scripts; \
		cp skills/$$skill/SKILL.md $(SKILLS_DIR)/$$skill/; \
		if [ -d skills/$$skill/scripts ]; then \
			cp skills/$$skill/scripts/* $(SKILLS_DIR)/$$skill/scripts/; \
		fi; \
	done
	@echo "Skills installed to $(SKILLS_DIR)"
