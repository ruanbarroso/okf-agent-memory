---
type: Decision
title: Bundle Isolation and Mutation Security Boundaries
description: "Defensive security architecture enforcing canonical bundle boundaries, symlink containment, path traversal prevention, and frontmatter injection defense."
generated: { by: agent/mcp, at: "2026-10-04T14:21:30Z" }
---

# Bundle Isolation and Mutation Security Boundaries

## Context & Problem Statement

OKF Agent Memory is designed to be written, updated, and queried by autonomous AI agents operating across IDEs and CI/CD pipelines. Because AI agent tool calls may execute untrusted or adversarial prompts, the deterministic tooling layer must treat all mutation arguments and bundle directories as untrusted inputs.

## Security Controls & Defense-in-Depth

The tooling layer enforces defensive confinement across four critical choke-points:

### 1. Canonical Bundle Confinement (`ensureWithinRoot`)
All file reads in `LoadBundle` and writes in `SaveConcept`, `UpdateParentIndex`, and `AppendLogEntry` resolve symlinks and compare canonical paths against the bundle root via `filepath.Rel`.
- Traversal via relative parent references (`..`) or absolute paths escaping the bundle directory is strictly denied.
- Root-reserved files (`index.md`, `log.md`, `AGENTS.md`) cannot be overwritten as arbitrary concept documents regardless of letter case (`strings.EqualFold`). Subdirectory concepts (e.g. `architecture/agents.md`) remain permissible.

### 2. Symlink Escape Prevention
To defend against Local File Inclusion (LFI) and Arbitrary File Overwrite:
- `LoadBundle` inspects symlinks with `ensureWithinRoot`. Any symlink whose resolved target escapes the canonical bundle root triggers an immediate traversal error.
- Symlinks must point strictly to markdown (`.md`) files within the bundle; symlinks to directories or non-markdown assets are rejected.
- Writing through symlinks pointing outside the bundle is blocked.

#### Symlinked bundle roots
The bundle root itself may be a symlink, because users legitimately link a bundle into place. The target is confined by scope:
- **Project and vendor bundles** (`LoadBundle`): the root symlink must resolve inside the directory that contains the link. A repository that ships `knowledge -> /elsewhere` is therefore rejected, with or without a trailing slash.
- **User and system scope roots** (`~/.okf`, `/etc/okf`, `OKF_USER_DIR`, `OKF_SYSTEM_DIR`; internal `loadTrustedBundle`): the root is configured by the user, not by the repository, so it may be a symlink to any location. Symlinks inside these bundles stay confined by rule 2.
- A `knowledge/` subdirectory symlink is always confined to its parent root, including in trusted scopes.
- The walk always runs over the resolved root. Walking the unresolved path loaded an empty bundle silently, because `filepath.WalkDir` does not follow a symlink at its root.

### 3. Frontmatter & Metadata Injection Defense (`sanitizeConceptMetadata`)
Metadata fields (`type`, `title`, `description`, `actor`) are strictly validated prior to serialization:
- Newline characters (`\r`, `\n`) are rejected in scalar metadata fields, preventing YAML attribute smuggling or forged verification states (`verified.by: human`).
- The YAML document delimiter (`---`) is forbidden within metadata values.
- Special scalar characters (`:`, `&`, `"`, etc.) are quoted safely in YAML serialization without escaping HTML entities.

### 4. MCP Server Root Confinement (`resolveBundleDir`)
The stdio Model Context Protocol (MCP) server confines dynamic bundle switching:
- The server initializes with a canonical `rootDir` (governed by project workspace or `OKF_MCP_ROOT`).
- Multi-bundle projects can dynamically select sub-bundles (e.g. `bundle="examples/software"`), but any request escaping the server root is denied before loading.
- Required MCP mutation arguments (`concept_id`, `type`, `title`, `description`) are strictly non-empty.

### 5. Collaborative File Permissions (`0o644` / `0o755`)
OKF bundles are designed for shared Git repository version control:
- Files are created with `0o644` (rw-r--r--) and directories with `0o755` (rwxr-xr-x).
- Static analysis rules intended for secret credential files (such as `gosec G301/G306` requiring `0600`/`0750`) are deliberately excluded, ensuring bundle readability across multi-user environments, CI/CD runners, and Git sub-processes.

### 6. Human Verification Provenance (`ensureNoForgedHumanVerification`)
`SaveConcept` refuses to let a non-human actor add a human verification:
- A human identity is `human`, `human:*`, or `human/*`, compared case-insensitively (`IsHumanIdentity`). The `verified.by=human` filter uses the same definition, so the guard and the filter cannot disagree.
- An agent may preserve `verified` entries already recorded in the concept file. Adding one, or altering the timestamp of an existing one, is rejected. A new concept inherits nothing from an existing file at its path.
- Scope of the guarantee: the actor is self-declared. The MCP server fixes it to `agent/mcp`, so the guard is effective there. A CLI user can still pass `--actor human/...`, so for the CLI it guards against accidental or injected forgery, not against a deliberate caller.

## Relationships

- Defined as part of Layer 4 in [layers](layers.md).
- Complements the deterministic single-binary tooling in [tooling-decision](tooling-decision.md).

# Related Concepts
- [MCP Tool Security & Untrusted Agent Input](../convention/mcp-agent-safety.md): Behavioral MCP guidelines complement deterministic boundaries
- [Automated Security Auditing & Jules Remediation Workflow](../convention/security-audit.md): Proactive adversarial auditing and verification pipeline
