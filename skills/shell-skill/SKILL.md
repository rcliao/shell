---
name: shell-skill
description: Load full skill instructions on-demand
usage: scripts/shell-skill load <skill-name>  # returns full SKILL.md body
allowed-tools: Bash
tier: core
---

# Skill Loader

Load the full instructions for a specific skill. Use this when you need detailed usage info for a skill listed in the catalog.

## Usage

```bash
scripts/shell-skill load <name>
```

Returns the full SKILL.md body including usage instructions, examples, and script paths.
