# 0. ADR Template

Date: 2025-12-28

## Status

Accepted

## Context

This project needs a standardized way to document architectural decisions. Architecture Decision Records (ADRs) provide a lightweight, version-controlled method to capture important technical choices and their rationale.

Without ADRs:
- Architectural decisions are lost over time
- New team members don't understand why choices were made
- Decisions are repeated or reversed without context
- Knowledge exists only in people's heads or scattered documentation

## Decision

We will use Architecture Decision Records (ADRs) stored in `docs/adr/` directory with numbered markdown files.

Each ADR will:
- Be numbered sequentially (0001, 0002, etc.)
- Use kebab-case titles (e.g., `0001-use-postgresql.md`)
- Follow a consistent template structure
- Be stored in version control
- Remain immutable once accepted (updated status only)

## Consequences

### Positive
- Architectural knowledge is preserved and accessible
- Decision rationale is documented for future reference
- New team members can understand project evolution
- Decisions can be reviewed and referenced
- Creates accountability for architectural choices

### Negative
- Requires discipline to create ADRs consistently
- Takes time to write comprehensive ADRs
- May accumulate outdated decisions over time

### Risks
- Team might not maintain ADR practice
- ADRs could become outdated if not referenced

## Alternatives Considered

### Option 1: Wiki or Confluence
- Pros: Easy to edit, good for collaboration
- Cons: Not version-controlled, can diverge from code, requires separate tool

### Option 2: Single architecture.md file
- Pros: Simple, all in one place
- Cons: Doesn't scale, hard to track history, merge conflicts

### Option 3: Code comments only
- Pros: Close to code, no extra files
- Cons: Scattered, hard to find, limited context, only covers code-level decisions

## References

- [Architecture Decision Records (ADR) by Michael Nygard](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions)
- [ADR GitHub Organization](https://adr.github.io/)
- [Keep a Changelog](https://keepachangelog.com/)
