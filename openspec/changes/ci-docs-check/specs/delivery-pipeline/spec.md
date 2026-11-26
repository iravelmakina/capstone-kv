# Spec Delta

## ADDED Requirements

### Requirement: Documentation changes are checked
A push or pull request that changes documentation (`docs/`, `openspec/` or Markdown files)
SHALL run a check that validates OpenSpec strictly and verifies that every relative
Markdown link points to an existing file, without AWS credentials.

#### Scenario: Docs-only push
- **WHEN** a push changes only documentation
- **THEN** the docs check runs and `ci-cd` does not start

#### Scenario: Broken link
- **WHEN** a tracked Markdown file links, outside code, to a path that doesn't exist
- **THEN** the docs check fails and names the file and the link

#### Scenario: Outside a lab session
- **WHEN** the repository's AWS secrets have expired
- **THEN** the docs check still runs to completion
