---
type: Decision
title: Retirement of Standalone Starter-Pack Distribution Archives
description: Retirement of standalone starter-pack zip and tar.gz archives in favor of okf bootstrap CLI scaffolding.
tags: [architecture, decision, packaging, release, distribution, bootstrap]
generated: { by: agent/mcp, at: "2026-10-12T10:03:55Z" }
status: stable
---

# Retirement of Standalone Starter-Pack Distribution Archives

## Context
Originally, the project packaged standalone starter-pack archives (`okf-starter-pack-v<VERSION>.tar.gz` and `.zip`) via `make dist-bundle` and CI release steps. These bundles contained a pre-generated `knowledge/` scaffold, `AGENTS.md`, and a platform-specific binary built during the GitHub Actions run.

However:
1. `okf bootstrap [dir] --name <Project>` natively creates the identical memory skeleton and agent skill bindings using embedded assets.
2. The release workflow compiled the starter-pack binary on `ubuntu-latest` (Linux AMD64), producing archives unusable for macOS or Windows users.
3. Download metrics showed near-zero adoption compared to direct binary downloads, Homebrew tap installation, and `go install`.

## Decision
- Retire `make dist-bundle` from the build system.
- Remove starter pack archive packaging and upload from `.github/workflows/release.yml`.
- Establish `okf bootstrap` as the single canonical mechanism for project scaffolding.
- Restrict release assets to cross-compiled standalone binaries, Homebrew Formula, and checksums.

## Consequences
- **Positive**: Shorter CI runtimes, zero platform mismatches in release downloads, simplified release verification checklist.
- **Negative**: Users without the `okf` CLI installed cannot download a pre-extracted directory archive; they must obtain the CLI binary or install via package manager first.

## Related Concepts
- [Release Procedure & Distribution Runbook](../convention/release-procedure.md): Release gates and asset checklist
- [5-Layer System Architecture](layers.md): Distribution and tooling layer
