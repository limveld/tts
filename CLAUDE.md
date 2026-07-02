# CLAUDE.md

Guidance for AI agents working in this repository.

## Agent skills

### Issue tracker

Issues and PRDs are tracked as local markdown files under `.scratch/<feature-slug>/` (no external PR triage surface). See `docs/agents/issue-tracker.md`.

### Triage labels

Uses the default triage vocabulary (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`), recorded as a `Status:` line in each issue file. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `CONTEXT.md` + `docs/adr/` at the repo root. See `docs/agents/domain.md`.
