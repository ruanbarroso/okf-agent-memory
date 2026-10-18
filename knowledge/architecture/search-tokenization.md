---
type: Decision
title: Unicode and Deterministic Search
description: "Search tokenizes Unicode letters and digits, enforces symmetrical tokenized DF/TF with exact matching for short stems, and caps metadata frequencies."
tags: [search, unicode, determinism, bm25]
generated: { by: agent/mcp, at: "2026-10-12T09:33:58Z" }
---

# Unicode and Deterministic Search

Token boundaries use Unicode character classes. Equal rounded scores are ordered by concept ID. See [Go Single-Binary CLI and MCP Architecture Decision](tooling-decision.md).

## BM25 Scoring Symmetry & Field Normalization

Document Frequency (DF) and Term Frequency (TF) share unified tokenization and matching rules:
* Short query tokens (< 4 runes) require exact token matches to prevent DF/TF collapse on common acronyms (e.g. `ci`, `ui`, `id`) and prefix false positives on short stems (e.g. `log` vs. `login`).
* Tokens with length >= 4 runes support prefix matching.
* Metadata fields enforce bounded TF saturation caps (Title: 2.0, Tags: 2.0, Description: 3.0, ID: 2.0, Body: 5.0) to prevent keyword stuffing.
