# Fastmail MCP — Claude Code Guide

## Project Overview

A single-file Swift MCP server (`Sources/fastmail-mcp/fastmail_mcp.swift`) that exposes Fastmail email, contacts, and identities as MCP tools via the JMAP API. No external dependencies.

## Build & Install

```bash
swift build -c release        # Binary: .build/release/fastmail-mcp
./install.sh                  # Build + install to /usr/local/bin + register with Claude Desktop
```

## Configuration

Set `FASTMAIL_TOKEN` environment variable with a Fastmail API token.
Generate one at: Fastmail → Settings → Privacy & Security → API tokens → New API token.
Required scopes: Mail, Contacts, Submission.

## Architecture

Single-file Swift, no external dependencies. Uses URLSession + DispatchSemaphore for synchronous JMAP HTTP calls.

**Logical sections (in order):**
1. MCP protocol types (`ToolDefinition`, `MCPError`)
2. JMAP session discovery + HTTP helpers (`sessionFor`, `jmapCall`, `syncHTTP`)
3. Tool implementations (one Swift function per tool)
4. Serialization helpers (`emailSummaryDict`, `contactSummaryDict`, etc.)
5. Tool definitions (`tools` array)
6. Tool dispatch (`callTool`)
7. MCP server (`MCPServer` class — JSON-RPC stdio loop)
8. Entry point

## JMAP API Pattern

All JMAP calls follow this pattern:
1. Session discovery: `GET https://api.fastmail.com/jmap/session` (cached)
2. Method calls: `POST` to `session.apiUrl` with `{"using": [...], "methodCalls": [...]}`
3. For listing: two-step `Foo/query` → `Foo/get` using back-references (`#ids`)
4. Rate limiting: automatic retry on 429 with `Retry-After` header

## Tools (14 total)

### Email (8)
- `fm_list_mailboxes` — all mailboxes with role, unread/total counts
- `fm_list_emails` — emails in mailbox; params: mailboxId, limit, offset, onlyUnread
- `fm_get_email` — full email by ID with body, HTML, attachments
- `fm_search_emails` — search with text or JMAP filter JSON; params: query, mailboxId, limit
- `fm_send_email` — send email; params: to, subject, body, cc, replyToId
- `fm_mark_read` — mark read/unread; params: ids, read
- `fm_move_email` — move to mailbox; params: ids, mailboxId
- `fm_delete_email` — move to Trash; params: ids

### Bridge Inbox (2)
- `fm_list_bridge_messages` — unread emails in Bridge mailbox with parsed structured types
- `fm_ack_bridge_message` — mark read + move to Bridge/Processed

### Contacts (2)
- `fm_list_contacts` — list/search contacts; params: limit, search
- `fm_get_contact` — contact by ID with full details

### Identity (1)
- `fm_list_identities` — sending identities (email addresses)

## Bridge Message Convention

The bridge inbox is a designated Fastmail mailbox (default name: "Bridge") for structured messages that Claude can process as actionable items.

### Subject format
```
[TYPE] description
```

Supported types:
- `[TASK]` — actionable task, e.g. `[TASK] Buy groceries`
- `[EVENT]` — calendar event, e.g. `[EVENT] Doctor appointment 2026-03-15 14:00`
- `[NOTE]` — note/reminder, e.g. `[NOTE] Remember to call Mom`

### Setup
1. Create a mailbox called "Bridge" in Fastmail
2. Create a subfolder "Processed" under Bridge (Bridge/Processed)
3. Send structured emails to yourself with `[TYPE] description` subjects
4. Use `fm_list_bridge_messages` to read and `fm_ack_bridge_message` to process

### How it works
- `fm_list_bridge_messages` reads unread emails from the Bridge mailbox
- If the subject matches `[TYPE] description`, returns `bridgeType` and `bridgeDescription` fields
- Body is free text — returned as-is for the caller to interpret
- `fm_ack_bridge_message` marks the message read and moves to Bridge/Processed

## Adding a New Tool

1. Add Swift function in the tool implementations section
2. Add `ToolDefinition` to the `tools` array
3. Add `case` to `callTool()`
4. `swift build -c release`

## Key Design Notes

- Session is cached for process lifetime (no re-discovery per call)
- 429 rate limit: retries up to 2 times with `Retry-After` delay (capped at 30s)
- `fm_send_email` accepts both `[{name, email}]` objects and plain `["email"]` string arrays for to/cc
- `fm_search_emails` query can be plain text (becomes `{"text": query}`) or a JSON JMAP filter string
- Contacts use `https://www.fastmail.com/dev/contacts` capability and `ContactCard/query+get`
- Sending uses `urn:ietf:params:jmap:submission` capability with `Email/set` + `EmailSubmission/set`
