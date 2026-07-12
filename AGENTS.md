# Repository Guidelines

This file provides guidance to AI agents when working with code in this repository.

> **Single source of truth:** This file is a concise pointer document.
> All authoritative architecture, coding rules, and conventions
> live in **CLAUDE.md** at the project root. Read that file first.
> Use `Makefile`, `package.json`, and `pnpm-workspace.yaml` as the
> source of truth for the full command list.

## Quick Reference

### Architecture

Go backend + monorepo frontend (pnpm workspaces + Turborepo) with shared packages.

- `server/` - Go backend (Chi router, sqlc, gorilla/websocket)
- `apps/web/` - Next.js frontend (App Router)
- `apps/desktop/` - Electron desktop app
- `packages/core/` - Headless business logic (Zustand stores, React Query hooks, API client)
- `packages/ui/` - Atomic UI components (shadcn/Base UI, zero business logic)
- `packages/views/` - Shared business pages/components
- `packages/tsconfig/` - Shared TypeScript config

### State Management (critical)

- **React Query** owns all server state (issues, members, agents, inbox, workspace list)
- **Zustand** owns client/view state (view filters, drafts, modals, desktop tab state); current workspace identity is route-driven and only mirrored for platform plumbing
- All Zustand stores live in `packages/core/` - never in `packages/views/` or app directories
- WS events update React Query for server data; store writes are only for clearing client-owned pointers with a single responder/self-event guard

### Package Boundaries (hard rules)

- `packages/core/` - zero react-dom, zero localStorage, zero process.env
- `packages/ui/` - zero `@multica/core` imports
- `packages/views/` - zero `next/*`, zero `react-router-dom`, use `NavigationAdapter` for routing
- `apps/web/platform/` - only place for Next.js APIs

### Workbench Connector Boundary (hard rules)

Multica owns the Connector control plane and unified work model. External Connector runtimes own source-system protocol implementations.

- Multica may own generic Connector registration, lifecycle, capabilities, machine authentication, health metadata, normalized ingest, External Record/Issue binding, templates, Agent routing, and generic delivery contracts.
- A Connector is the required managed bridge to an external system. Register each external runtime and its capabilities in Multica, then bind its ingest and Agent tools through source-neutral contracts.
- Do not add source-specific migrations, handlers, pollers, checkpoints, leases, API clients, workflow engines, or approval logic for Ferry, Jira, ZenTao, GitLab, or similar systems to Multica Core.
- External API login, polling/webhooks, pagination/cursors, field conversion, and official API operations belong in a lightweight external Connector Runtime, MCP server, or Connector Service. A simple connector may remain a single script.
- Agents are the runners. They call constrained Connector tools; Multica must not rebuild the external system's workflow.
- Source credentials stay inside the trusted Connector Runtime or credential-isolating Tool Gateway. Never put reusable external credentials or Multica connector machine tokens in prompts, Issue content, Agent `mcp_config`, or Agent process environments.
- Tool authorization must be enforced by the Connector/Gateway using workspace, connector, task, tool, operation, resource, and risk policy. Prompt instructions are not authorization.
- Adding a data source should normally require no Multica schema or worker changes: inspect its official API, implement the common Connector ingest/tool contract, and register it.
- Reuse the external system as the authority. Multica provides the management entry point and bridge; it must not become a shadow implementation of that system.
- Before adding source-specific backend code, stop and obtain explicit architectural approval. Existing planning documents do not override this boundary.

### Commands

```bash
make dev              # Auto-setup + start everything
pnpm typecheck        # TypeScript check
pnpm test             # TS unit tests (Vitest)
make test             # Go tests
make check            # Full verification pipeline
```

See CLAUDE.md for the authoritative rules and common commands.
