# Fastmail MCP — Claude Code Guide

## Project Overview

A single-file Go MCP server (`main.go`) that exposes Fastmail email, contacts, calendars, masked email, Sieve filters, and more as MCP tools via the JMAP API. No external dependencies — stdlib only.

Cross-platform: builds for macOS, Windows, and Linux from a single codebase.

## Build & Install

```bash
go build -o fastmail-mcp .                          # Build for current platform
GOOS=windows GOARCH=amd64 go build -o fastmail-mcp.exe .  # Cross-compile for Windows
./install.sh                                         # Build + install + register with Claude Desktop
```

## Configuration

Set `FASTMAIL_TOKEN` environment variable with a Fastmail API token.
Generate one at: Fastmail → Settings → Privacy & Security → API tokens → New API token.
Required scopes: **Mail, Contacts, Calendars, Submission**.

## Architecture

Single-file Go (`main.go`), no external dependencies. Uses `net/http` for synchronous JMAP HTTP calls.

**Logical sections (in order):**
1. MCP protocol types (`toolDefinition`, `mcpError`)
2. JMAP session discovery + HTTP helpers (`sessionFor`, `jmapCall`, `doHTTPWithRetry`)
3. Capability helpers (mail, contacts, calendar, masked email, vacation, quota)
4. JSON helpers (`getString`, `getMap`, `respData`, `respList`, etc.)
5. Tool implementations — one Go function per tool, grouped by domain
6. Serialization helpers (`emailSummaryDict`, `eventSummaryDict`, etc.)
7. Utility functions (`intParam`, `contains`, `parseBridgeSubject`)
8. Tool definitions (`tools` slice — 39 total)
9. Tool dispatch (`callTool` via `toolHandlers` map)
10. MCP server (`run`, `handleMessage` — JSON-RPC stdio loop)
11. Entry point (`main`)

## JMAP API Pattern

All JMAP calls follow this pattern:
1. Session discovery: `GET https://api.fastmail.com/jmap/session` (cached, includes downloadUrl/uploadUrl)
2. Method calls: `POST` to `session.apiUrl` with `{"using": [...], "methodCalls": [...]}`
3. For listing: two-step `Foo/query` → `Foo/get` using back-references (`#ids`)
4. Rate limiting: automatic retry on 429 with `Retry-After` header

## Tools (72 total, covering 35 JMAP methods)

### Email (9)
- `fm_list_mailboxes` — all mailboxes with role, unread/total counts
- `fm_list_emails` — emails in mailbox; params: mailboxId, limit, offset, onlyUnread
- `fm_get_email` — full email with body, HTML, attachment details (blobId for download)
- `fm_search_emails` — search with text or JMAP filter; params: query, mailboxId, limit, includeSnippets
- `fm_send_email` — send email; params: to, subject, body, cc, replyToId
- `fm_mark_read` — mark read/unread; params: ids, read
- `fm_move_email` — move to mailbox; params: ids, mailboxId
- `fm_delete_email` — move to Trash; params: ids
- `fm_import_email` — import RFC 5322 message from blob; params: blobId, mailboxId

### Thread (1)
- `fm_get_thread` — full conversation thread from any email ID

### Mailbox Management (3)
- `fm_create_mailbox` — create folder; params: name, parentId
- `fm_rename_mailbox` — rename or move folder; params: id, name, parentId
- `fm_delete_mailbox` — delete folder; params: id, deleteContents

### Bridge Inbox (2)
- `fm_list_bridge_messages` — unread emails in Bridge mailbox with parsed structured types
- `fm_ack_bridge_message` — mark read + move to Bridge/Processed

### Calendar (10)
- `fm_list_calendars` — all calendars with name, color, visibility
- `fm_create_calendar` — create calendar; params: name, color, isVisible
- `fm_update_calendar` — update calendar; params: id, name, color, isVisible
- `fm_delete_calendar` — delete calendar and all events; params: id
- `fm_list_events` — events in date range; params: after, before, calendarId, limit
- `fm_get_event` — full event detail (participants, recurrence, alerts, locations)
- `fm_create_event` — create event; params: calendarId, title, start, timeZone, duration, etc.
- `fm_update_event` — update event fields; params: id + any fields
- `fm_delete_event` — delete event; params: id
- `fm_rsvp_event` — respond to invitation; params: id, status, email

### Contacts (5)
- `fm_list_contacts` — list/search contacts; params: limit, search
- `fm_get_contact` — contact by ID with full details
- `fm_create_contact` — create contact; params: firstName, lastName, emails, phones, company, notes
- `fm_update_contact` — update contact fields; params: id + any fields
- `fm_delete_contact` — delete contact; params: id

### Address Books (3)
- `fm_list_address_books` — list contact address books
- `fm_create_address_book` — create address book; params: name
- `fm_delete_address_book` — delete address book; params: id

### Identity (2)
- `fm_list_identities` — sending identities (email addresses)
- `fm_update_identity` — update identity; params: id, name, textSignature, htmlSignature, replyTo, bcc

### Masked Email (3)
- `fm_list_masked_emails` — list all masked email aliases; params: state (filter)
- `fm_create_masked_email` — create alias; params: forDomain, description
- `fm_update_masked_email` — enable/disable/update alias; params: id, state, description

### Vacation Response (2)
- `fm_get_vacation_response` — current auto-responder settings
- `fm_set_vacation_response` — configure auto-responder; params: isEnabled, subject, textBody, fromDate, toDate

### Spam Reporting (3)
- `fm_report_spam` — move to Junk + set `$junk` keyword to train filter (batch)
- `fm_report_phishing` — move to Junk + set `$phishing` + `$junk` keywords (batch)
- `fm_report_not_spam` — move to Inbox + set `$notjunk`, remove `$junk`/`$phishing` (batch)

### Archive & Lifecycle (3)
- `fm_archive_email` — move to Archive mailbox (batch)
- `fm_destroy_email` — permanent delete, bypasses Trash (batch, irreversible)
- `fm_unsnooze_email` — clear snooze, return email immediately

### Snooze & Flags (2)
- `fm_snooze_email` — snooze email; params: id, until, mailboxId
- `fm_flag_email` — set/remove keywords; params: ids, keyword, set

### Quota (1)
- `fm_get_quota` — storage usage and limits

### Newsletter / Mailing List (2)
- `fm_detect_newsletters` — scan mailbox for List-Id/List-Unsubscribe headers, aggregate by list
- `fm_unsubscribe_list` — RFC 8058 one-click unsubscribe for TRUSTED senders (not for spam!)

### Draft Management (2)
- `fm_create_draft` — save email as draft without sending
- `fm_list_drafts` — list saved drafts

### Forwarding (1)
- `fm_forward_email` — forward an email with optional comment; params: emailId, to, comment

### Follow-up (1)
- `fm_find_unreplied` — find sent emails with no reply; params: daysOld, maxScan

### Sender Analysis (1)
- `fm_analyze_sender` — full sender profile: email count, date range, mailbox distribution, list headers, auth results, top subjects

### Agentic Workflow (5)
- `fm_list_email_ids` — lightweight scan: IDs + from + subject + date only (up to 1000/call)
- `fm_batch_get_emails` — fetch up to 50 emails by ID with bodies in one call
- `fm_get_mailbox_stats` — sender/domain frequency, date range, size stats (scans up to 1000)
- `fm_get_sieve_capabilities` — server's supported Sieve extensions and limits
- `fm_find_duplicates` — scan mailbox for duplicate emails by Message-ID (or subject+from+date fallback), returns groups with suggested keep/delete IDs

### Sieve Filters (6)
- `fm_list_sieve_scripts` — list all Sieve scripts with name and active status
- `fm_get_sieve_script` — get script by ID with full source code
- `fm_set_sieve_script` — create or update a script; params: content, name, id (for update), activate
- `fm_delete_sieve_script` — delete a script (auto-deactivates if active)
- `fm_activate_sieve_script` — activate a script by ID, or deactivate all (omit id)
- `fm_validate_sieve_script` — validate syntax without saving; params: content

### Delivery Tracking (1)
- `fm_get_email_submission` — check delivery status of sent emails; params: id (or list recent)

### Email Parse (1)
- `fm_parse_email` — parse an uploaded .eml blob into structured fields without importing

### Read Receipts / MDN (2)
- `fm_send_read_receipt` — send read receipt for an email; params: forEmailId
- `fm_parse_read_receipt` — parse a received read receipt blob

### Attachment (1)
- `fm_download_attachment` — get download URL for attachment; params: blobId, name, type

## JMAP Capabilities Used

| Domain | Capability URN |
|--------|---------------|
| Core | `urn:ietf:params:jmap:core` |
| Mail | `urn:ietf:params:jmap:mail` |
| Submission | `urn:ietf:params:jmap:submission` |
| Vacation | `urn:ietf:params:jmap:vacationresponse` |
| Quota | `urn:ietf:params:jmap:quota` |
| Contacts | `https://www.fastmail.com/dev/contacts` |
| Calendars | `https://www.fastmail.com/dev/calendars` |
| Masked Email | `https://www.fastmail.com/dev/maskedemail` |
| Sieve | `urn:ietf:params:jmap:sieve` (RFC 9661) |
| MDN | `urn:ietf:params:jmap:mdn` (RFC 9007) |

## AI Playbooks

See **SKILLS.md** for agentic workflow playbooks — step-by-step patterns for inbox cleanup, spam management, duplicate removal, Sieve authoring, calendar management, and more. That file is the AI-facing "how to use these tools together" guide.

## Adding a New Tool

1. Add a Go function with signature `func myTool(params m) (any, error)` in the tool implementations section
2. Add a `toolDefinition` to the `tools` slice
3. Add an entry to the `toolHandlers` map
4. `go build .`

## Key Design Notes

- Session is cached for process lifetime (no re-discovery per call), protected by `sync.Mutex`
- Session also caches `downloadUrl` and `uploadUrl` templates for blob operations
- 429 rate limit: retries up to 2 times with `Retry-After` delay (capped at 30s)
- `fm_send_email` moves sent mail to the Sent folder via `onSuccessUpdateEmail`
- `fm_search_emails` supports `includeSnippets` for highlighted search results via `SearchSnippet/get`
- `fm_get_email` returns attachment details with `blobId` for use with `fm_download_attachment`
- Calendar tools use JSCalendar format (RFC 8984) for events
- Contact tools accept both simple fields (`firstName`, `emails` as strings) and full JSContact format
- Masked email uses Fastmail's proprietary extension (`MaskedEmail/get`, `MaskedEmail/set`)
- `fm_report_spam` sets `$junk` keyword which trains Fastmail's Bayesian spam filter (Cyrus)
- `fm_report_phishing` sets both `$phishing` and `$junk` — flagged separately for Fastmail's abuse team
- Never unsubscribe from spam — use `fm_report_spam` instead (unsubscribing confirms a live address)
- `fm_destroy_email` uses `Email/set destroy` for permanent deletion (vs `fm_delete_email` which moves to Trash)
- `fm_get_mailbox_stats` paginates internally (up to 5 JMAP calls) to scan up to 1000 emails
- `fm_list_email_ids` returns minimal fields for fast triage (up to 1000/call, vs 200 for full emails)
- `fm_batch_get_emails` caps at 50 per call with 256KB body limit per email
- Sieve tools use blob upload/download for script content (RFC 9661 pattern)
- Sieve scripts support `vnd.cyrus.jmapquery` for JMAP filter syntax inside Sieve rules
- Only one Sieve script can be active at a time; activation is atomic via `onSuccessActivateScript`
- Snooze uses Fastmail's proprietary `snoozed` property on Email
- Limit params are capped at 200 to prevent oversized JMAP responses
- JSON-RPC notifications (no `id`) never receive responses
- Proper JSON-RPC error codes: -32600 (invalid request), -32601 (method not found), -32602 (invalid params)
- Max input line size: 10MB
