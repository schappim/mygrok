# mygrok Claude skill

A [Claude](https://claude.com/claude-code) skill that teaches Claude how to drive `mygrok` — running tunnels, managing services, configuring IP rules, onboarding visitors with passkey invites, claiming custom hostnames, and self-updating the binary.

## Install (Claude Code)

Skills live under `~/.claude/skills/<name>/`. To install this one:

```bash
# Clone the repo somewhere stable…
git clone https://github.com/schappim/mygrok.git ~/code/mygrok

# …and symlink the skill into Claude's skill directory:
mkdir -p ~/.claude/skills
ln -s ~/code/mygrok/skills/mygrok ~/.claude/skills/mygrok
```

Restart Claude Code (or start a new session). Claude will autoload the skill when you ask anything tunneling-related — exposing a port, sharing a folder, locking down access, onboarding a passkey user, etc.

If you'd rather copy than symlink (so the skill doesn't track upstream changes):

```bash
cp -R ~/code/mygrok/skills/mygrok ~/.claude/skills/mygrok
```

## What's in here

- `SKILL.md` — the skill body. YAML frontmatter (`name`, `description`) governs when Claude loads it; the rest is Claude-facing reference covering every common workflow.

## Updating

Whenever the CLI grows new admin verbs, edit `SKILL.md` to match (or just re-pull the repo if you symlinked). The authoritative CLI surface is always `mygrok admin help`; the skill mirrors a curated subset for fast lookup.

## Manual fallback

If you don't use Claude Code, you can paste `SKILL.md` straight into any LLM chat as a system / preamble message. It's self-contained.
