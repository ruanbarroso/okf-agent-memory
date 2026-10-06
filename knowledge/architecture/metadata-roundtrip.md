---
type: Decision
title: Safe Unknown Metadata Round-Trip
description: Unknown frontmatter keys are serialized deterministically with safe quoting and JSON-compatible scalar and collection preservation.
tags: [metadata, parser, serialization, security]
generated: { by: agent/mcp, at: "2026-10-04T14:24:18Z" }
---

# Safe Unknown Metadata Round-Trip

Unknown fields preserve string content and JSON-compatible value kinds while canonicalizing top-level key order. Quoted delimiters do not split flow values. See [Bundle Isolation and Mutation Security Boundaries](security-boundaries.md).

## List-valued fields

Unknown list fields are parsed into typed `[]any` values, so they stay lists after `okf update`:
- Flow lists (`[a, 'b, c', 3]`, nested lists included) and block lists of scalars (`- a`, uniform indentation) are parsed item by item. Items keep JSON scalar types (numbers, booleans); other items are unquoted strings. A plain `- https://x` is a scalar, while `- key: value` is a mapping.
- On write, lists are emitted as JSON flow sequences, which are valid YAML. Block lists therefore change formatting but not meaning.
- Block structures that are not scalar lists (lists of mappings, nested mappings, nested lists, uneven indentation) are kept as verbatim lines and written back unchanged.
- Known limitation: a flow mapping inside a flow list (`[{a: b}]`) is still read as a string.
