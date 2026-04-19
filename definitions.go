package main

// ── Tool Dispatch Type ─────────────────────────────────────────────────────

type toolFunc func(m) (any, error)

// ── Tool Definitions ────────────────────────────────────────────────────────

var tools = []toolDefinition{
	// Email
	{
		Name:        "fm_list_mailboxes",
		Description: "List all Fastmail mailboxes with name, role, unread count, and total count.",
		InputSchema: m{"type": "object", "properties": m{}, "required": []string{}},
	},
	{
		Name:        "fm_list_emails",
		Description: "List emails in a mailbox. Returns subject, from, date, preview, read/flagged status.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"mailboxId":  m{"type": "string", "description": "Mailbox ID (get from fm_list_mailboxes)"},
				"limit":      m{"type": "integer", "description": "Max emails to return (default 20, max 200)"},
				"offset":     m{"type": "integer", "description": "Offset for pagination (default 0)"},
				"onlyUnread": m{"type": "boolean", "description": "Only return unread emails (default false)"},
			},
			"required": []string{"mailboxId"},
		},
	},
	{
		Name:        "fm_get_email",
		Description: "Get full email by ID. Returns subject, from, to, cc, date, text body, HTML body, and attachment names.",
		InputSchema: m{
			"type":       "object",
			"properties": m{"id": m{"type": "string", "description": "Email ID"}},
			"required":   []string{"id"},
		},
	},
	{
		Name:        "fm_search_emails",
		Description: `Search emails across mailboxes. Query can be a plain text string or a JMAP filter object as JSON string (e.g. {"from":"alice@example.com", "subject":"invoice"}).`,
		InputSchema: m{
			"type": "object",
			"properties": m{
				"query":           m{"type": "string", "description": "Search query (text or JSON JMAP filter)"},
				"mailboxId":       m{"type": "string", "description": "Optional: limit search to this mailbox"},
				"limit":           m{"type": "integer", "description": "Max results (default 20, max 200)"},
				"includeSnippets": m{"type": "boolean", "description": "Include highlighted search snippets (default false)"},
			},
			"required": []string{"query"},
		},
	},
	{
		Name:        "fm_send_email",
		Description: "Send an email. The 'to' field accepts [{name, email}] objects or plain email strings.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"to":        m{"type": "array", "description": `Recipients: [{name?, email}] or ["email"]`, "items": m{}},
				"subject":   m{"type": "string", "description": "Email subject"},
				"body":      m{"type": "string", "description": "Plain text email body"},
				"cc":        m{"type": "array", "description": "CC recipients (same format as to)", "items": m{}},
				"replyToId": m{"type": "string", "description": "Email ID to reply to (sets In-Reply-To/References headers)"},
			},
			"required": []string{"to", "subject", "body"},
		},
	},
	{
		Name:        "fm_mark_read",
		Description: "Mark email(s) as read or unread.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"ids":  m{"type": "array", "description": "Email IDs to update", "items": m{"type": "string"}},
				"read": m{"type": "boolean", "description": "true = mark read, false = mark unread (default true)"},
			},
			"required": []string{"ids"},
		},
	},
	{
		Name:        "fm_move_email",
		Description: "Move email(s) to a different mailbox.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"ids":       m{"type": "array", "description": "Email IDs to move", "items": m{"type": "string"}},
				"mailboxId": m{"type": "string", "description": "Destination mailbox ID"},
			},
			"required": []string{"ids", "mailboxId"},
		},
	},
	{
		Name:        "fm_delete_email",
		Description: "Move email(s) to Trash.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"ids": m{"type": "array", "description": "Email IDs to trash", "items": m{"type": "string"}},
			},
			"required": []string{"ids"},
		},
	},

	// Bridge Inbox
	{
		Name:        "fm_list_bridge_messages",
		Description: "List unread emails in the Bridge mailbox. Parses structured subjects like [TASK], [NOTE], [EVENT]. Returns bridgeType and bridgeDescription fields when subject matches convention.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"mailboxName": m{"type": "string", "description": "Bridge mailbox name (default: 'Bridge')"},
			},
			"required": []string{},
		},
	},
	{
		Name:        "fm_ack_bridge_message",
		Description: "Acknowledge a bridge message: mark as read and move to Bridge/Processed. Provide either 'ids' (array) or 'id' (single string).",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"ids":                  m{"type": "array", "description": "Email IDs to acknowledge", "items": m{"type": "string"}},
				"id":                   m{"type": "string", "description": "Single email ID (alternative to ids)"},
				"mailboxName":          m{"type": "string", "description": "Bridge mailbox name (default: 'Bridge')"},
				"processedMailboxName": m{"type": "string", "description": "Processed subfolder name (default: 'Bridge/Processed')"},
			},
			"required": []string{},
			"anyOf": []m{
				{"required": []string{"ids"}},
				{"required": []string{"id"}},
			},
		},
	},

	// Contacts
	{
		Name:        "fm_list_contacts",
		Description: "List contacts. Optionally search by name or email substring.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"limit":  m{"type": "integer", "description": "Max contacts to return (default 50, max 200)"},
				"search": m{"type": "string", "description": "Search by name or email"},
			},
			"required": []string{},
		},
	},
	{
		Name:        "fm_get_contact",
		Description: "Get a contact by ID with full details (name, emails, phones, online).",
		InputSchema: m{
			"type":       "object",
			"properties": m{"id": m{"type": "string", "description": "Contact ID"}},
			"required":   []string{"id"},
		},
	},

	// Identity
	{
		Name:        "fm_list_identities",
		Description: "List sending identities (email addresses available for sending).",
		InputSchema: m{"type": "object", "properties": m{}, "required": []string{}},
	},
	{
		Name:        "fm_update_identity",
		Description: "Update a sending identity (name, signature, replyTo, bcc).",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"id":            m{"type": "string", "description": "Identity ID"},
				"name":          m{"type": "string", "description": "Display name"},
				"textSignature": m{"type": "string", "description": "Plain text signature"},
				"htmlSignature": m{"type": "string", "description": "HTML signature"},
				"replyTo":       m{"type": "array", "description": "Reply-To addresses [{name?, email}]", "items": m{}},
				"bcc":           m{"type": "array", "description": "Auto-BCC addresses [{name?, email}]", "items": m{}},
			},
			"required": []string{"id"},
		},
	},

	// Thread
	{
		Name:        "fm_get_thread",
		Description: "Get all emails in a conversation thread. Pass any email ID to get the full thread.",
		InputSchema: m{
			"type":       "object",
			"properties": m{"id": m{"type": "string", "description": "Any email ID in the thread"}},
			"required":   []string{"id"},
		},
	},

	// Mailbox Management
	{
		Name:        "fm_create_mailbox",
		Description: "Create a new mailbox (folder).",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"name":     m{"type": "string", "description": "Mailbox name"},
				"parentId": m{"type": "string", "description": "Parent mailbox ID for nesting (optional)"},
			},
			"required": []string{"name"},
		},
	},
	{
		Name:        "fm_rename_mailbox",
		Description: "Rename or move a mailbox.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"id":       m{"type": "string", "description": "Mailbox ID"},
				"name":     m{"type": "string", "description": "New name"},
				"parentId": m{"type": "string", "description": "New parent ID (null for top-level)"},
			},
			"required": []string{"id"},
		},
	},
	{
		Name:        "fm_delete_mailbox",
		Description: "Delete a mailbox. Set deleteContents=true to also remove emails inside.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"id":             m{"type": "string", "description": "Mailbox ID to delete"},
				"deleteContents": m{"type": "boolean", "description": "Also delete emails in the mailbox (default false)"},
			},
			"required": []string{"id"},
		},
	},

	// Vacation Response
	{
		Name:        "fm_get_vacation_response",
		Description: "Get the current auto-responder (vacation response) settings.",
		InputSchema: m{"type": "object", "properties": m{}, "required": []string{}},
	},
	{
		Name:        "fm_set_vacation_response",
		Description: "Enable, disable, or configure the auto-responder (vacation response).",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"isEnabled": m{"type": "boolean", "description": "Enable or disable auto-reply"},
				"subject":   m{"type": "string", "description": "Auto-reply subject"},
				"textBody":  m{"type": "string", "description": "Auto-reply plain text body"},
				"htmlBody":  m{"type": "string", "description": "Auto-reply HTML body"},
				"fromDate":  m{"type": "string", "description": "Start date (UTC, e.g. 2026-04-20T00:00:00Z). Null for immediate."},
				"toDate":    m{"type": "string", "description": "End date (UTC). Null for indefinite."},
			},
			"required": []string{},
		},
	},

	// Calendar
	{
		Name:        "fm_list_calendars",
		Description: "List all calendars with name, color, and visibility.",
		InputSchema: m{"type": "object", "properties": m{}, "required": []string{}},
	},
	{
		Name:        "fm_list_events",
		Description: "List calendar events in a date range.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"after":      m{"type": "string", "description": "Start of range (UTC, e.g. 2026-04-18T00:00:00Z)"},
				"before":     m{"type": "string", "description": "End of range (UTC)"},
				"calendarId": m{"type": "string", "description": "Limit to this calendar (optional)"},
				"limit":      m{"type": "integer", "description": "Max events (default 50, max 200)"},
			},
			"required": []string{"after", "before"},
		},
	},
	{
		Name:        "fm_get_event",
		Description: "Get full calendar event details by ID.",
		InputSchema: m{
			"type":       "object",
			"properties": m{"id": m{"type": "string", "description": "Event ID"}},
			"required":   []string{"id"},
		},
	},
	{
		Name:        "fm_create_event",
		Description: "Create a calendar event.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"calendarId":      m{"type": "string", "description": "Calendar ID to create in"},
				"title":           m{"type": "string", "description": "Event title"},
				"start":           m{"type": "string", "description": "Start time (e.g. 2026-04-18T14:00:00)"},
				"timeZone":        m{"type": "string", "description": "IANA timezone (e.g. America/New_York)"},
				"duration":        m{"type": "string", "description": "ISO 8601 duration (e.g. PT1H, P1D)"},
				"showWithoutTime": m{"type": "boolean", "description": "All-day event (default false)"},
				"description":     m{"type": "string", "description": "Event description"},
				"location":        m{"type": "string", "description": "Location name"},
				"status":          m{"type": "string", "description": "confirmed, tentative, or cancelled"},
				"participants":    m{"type": "object", "description": "Participants map (JSCalendar format)"},
				"alerts":          m{"type": "object", "description": "Alerts map (JSCalendar format)"},
				"recurrenceRules": m{"type": "array", "description": "Recurrence rules (JSCalendar format)"},
			},
			"required": []string{"calendarId", "title", "start"},
		},
	},
	{
		Name:        "fm_update_event",
		Description: "Update a calendar event. Only provided fields are changed.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"id":              m{"type": "string", "description": "Event ID"},
				"title":           m{"type": "string", "description": "New title"},
				"start":           m{"type": "string", "description": "New start time"},
				"timeZone":        m{"type": "string", "description": "New timezone"},
				"duration":        m{"type": "string", "description": "New duration"},
				"showWithoutTime": m{"type": "boolean", "description": "All-day event"},
				"description":     m{"type": "string", "description": "New description"},
				"location":        m{"type": "string", "description": "New location"},
				"status":          m{"type": "string", "description": "New status"},
				"calendarId":      m{"type": "string", "description": "Move to different calendar"},
				"participants":    m{"type": "object", "description": "Updated participants"},
				"alerts":          m{"type": "object", "description": "Updated alerts"},
				"recurrenceRules": m{"type": "array", "description": "Updated recurrence"},
			},
			"required": []string{"id"},
		},
	},
	{
		Name:        "fm_delete_event",
		Description: "Delete a calendar event.",
		InputSchema: m{
			"type":       "object",
			"properties": m{"id": m{"type": "string", "description": "Event ID"}},
			"required":   []string{"id"},
		},
	},
	// Calendar Management
	{
		Name:        "fm_create_calendar",
		Description: "Create a new calendar.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"name":      m{"type": "string", "description": "Calendar name"},
				"color":     m{"type": "string", "description": "CSS color (e.g. #FF5733)"},
				"isVisible": m{"type": "boolean", "description": "Show in calendar UI (default true)"},
				"sortOrder": m{"type": "integer", "description": "Display order"},
			},
			"required": []string{"name"},
		},
	},
	{
		Name:        "fm_update_calendar",
		Description: "Update a calendar's name, color, or visibility.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"id":        m{"type": "string", "description": "Calendar ID"},
				"name":      m{"type": "string", "description": "New name"},
				"color":     m{"type": "string", "description": "New color"},
				"isVisible": m{"type": "boolean", "description": "Visibility"},
				"sortOrder": m{"type": "integer", "description": "Display order"},
			},
			"required": []string{"id"},
		},
	},
	{
		Name:        "fm_delete_calendar",
		Description: "Delete a calendar and all its events.",
		InputSchema: m{
			"type":       "object",
			"properties": m{"id": m{"type": "string", "description": "Calendar ID"}},
			"required":   []string{"id"},
		},
	},
	{
		Name:        "fm_rsvp_event",
		Description: "Respond to a calendar event invitation (accept, decline, tentative).",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"id":     m{"type": "string", "description": "Event ID"},
				"status": m{"type": "string", "description": "accepted, declined, tentative, or needs-action"},
				"email":  m{"type": "string", "description": "Your email (to match participant entry). Optional if only one attendee."},
			},
			"required": []string{"id", "status"},
		},
	},

	// Masked Email
	{
		Name:        "fm_list_masked_emails",
		Description: "List all masked email aliases (Fastmail anonymous addresses).",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"state": m{"type": "string", "description": "Filter by state: enabled, disabled, pending, deleted (optional)"},
			},
			"required": []string{},
		},
	},
	{
		Name:        "fm_create_masked_email",
		Description: "Create a new masked email alias. Returns the generated address.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"forDomain":   m{"type": "string", "description": "Domain/website this alias is for (optional)"},
				"description": m{"type": "string", "description": "Description/note (optional)"},
			},
			"required": []string{},
		},
	},
	{
		Name:        "fm_update_masked_email",
		Description: "Update a masked email alias (enable, disable, change description).",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"id":          m{"type": "string", "description": "Masked email ID"},
				"state":       m{"type": "string", "description": "New state: enabled, disabled, deleted"},
				"description": m{"type": "string", "description": "New description"},
				"forDomain":   m{"type": "string", "description": "New associated domain"},
			},
			"required": []string{"id"},
		},
	},

	// Snooze & Flag
	{
		Name:        "fm_snooze_email",
		Description: "Snooze an email to reappear later. Uses Fastmail's snooze extension.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"id":        m{"type": "string", "description": "Email ID to snooze"},
				"until":     m{"type": "string", "description": "When to unsnooze (UTC datetime, e.g. 2026-04-19T09:00:00Z)"},
				"mailboxId": m{"type": "string", "description": "Mailbox to return to (default: Inbox)"},
			},
			"required": []string{"id", "until"},
		},
	},
	{
		Name:        "fm_flag_email",
		Description: "Set or remove a keyword/flag on email(s). Common keywords: $flagged (star), $answered, $forwarded, or any custom keyword.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"ids":     m{"type": "array", "description": "Email IDs", "items": m{"type": "string"}},
				"keyword": m{"type": "string", "description": "Keyword to set/remove (default: $flagged)"},
				"set":     m{"type": "boolean", "description": "true = add keyword, false = remove (default true)"},
			},
			"required": []string{"ids"},
		},
	},

	// Quota
	{
		Name:        "fm_get_quota",
		Description: "Get account storage quota (used and limit).",
		InputSchema: m{"type": "object", "properties": m{}, "required": []string{}},
	},

	// Attachment/Blob
	{
		Name:        "fm_download_attachment",
		Description: "Get a download URL for an email attachment. Use blobId from fm_get_email's attachments array.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"blobId": m{"type": "string", "description": "Blob ID of the attachment"},
				"name":   m{"type": "string", "description": "Filename (optional)"},
				"type":   m{"type": "string", "description": "MIME type (optional, default application/octet-stream)"},
			},
			"required": []string{"blobId"},
		},
	},

	// Contact CRUD
	{
		Name:        "fm_create_contact",
		Description: "Create a new contact. Accepts simple fields (firstName, lastName, emails as strings) or full JSContact format.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"firstName": m{"type": "string", "description": "Given name"},
				"lastName":  m{"type": "string", "description": "Surname"},
				"fullName":  m{"type": "string", "description": "Full display name (alternative to first/last)"},
				"name":      m{"type": "object", "description": "Full JSContact name object (alternative to simple fields)"},
				"emails":    m{"type": "array", "description": "Email addresses: [\"addr\"] or [{address, label}]", "items": m{}},
				"phones":    m{"type": "array", "description": "Phone numbers: [\"number\"] or [{number, label}]", "items": m{}},
				"company":   m{"type": "string", "description": "Company/organization name"},
				"notes":     m{"type": "string", "description": "Free-text notes"},
			},
			"required": []string{},
		},
	},
	{
		Name:        "fm_update_contact",
		Description: "Update a contact. Only provided fields are changed.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"id":        m{"type": "string", "description": "Contact ID"},
				"firstName": m{"type": "string", "description": "Given name"},
				"lastName":  m{"type": "string", "description": "Surname"},
				"fullName":  m{"type": "string", "description": "Full display name"},
				"name":      m{"type": "object", "description": "Full JSContact name object"},
				"emails":    m{"type": "array", "description": "Email addresses", "items": m{}},
				"phones":    m{"type": "array", "description": "Phone numbers", "items": m{}},
				"company":   m{"type": "string", "description": "Company/organization"},
				"notes":     m{"type": "string", "description": "Notes"},
			},
			"required": []string{"id"},
		},
	},
	{
		Name:        "fm_delete_contact",
		Description: "Delete a contact.",
		InputSchema: m{
			"type":       "object",
			"properties": m{"id": m{"type": "string", "description": "Contact ID"}},
			"required":   []string{"id"},
		},
	},

	// Address Books
	{
		Name:        "fm_list_address_books",
		Description: "List contact address books.",
		InputSchema: m{"type": "object", "properties": m{}, "required": []string{}},
	},
	{
		Name:        "fm_create_address_book",
		Description: "Create a new contact address book.",
		InputSchema: m{
			"type":       "object",
			"properties": m{"name": m{"type": "string", "description": "Address book name"}},
			"required":   []string{"name"},
		},
	},
	{
		Name:        "fm_delete_address_book",
		Description: "Delete a contact address book and all contacts in it.",
		InputSchema: m{
			"type":       "object",
			"properties": m{"id": m{"type": "string", "description": "Address book ID"}},
			"required":   []string{"id"},
		},
	},

	// Email Delivery Tracking
	{
		Name:        "fm_get_email_submission",
		Description: "Check delivery status of sent emails. Pass an ID for a specific submission, or omit to list recent submissions.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"id":    m{"type": "string", "description": "Submission ID (omit to list recent)"},
				"limit": m{"type": "integer", "description": "Max results when listing (default 20, max 100)"},
			},
			"required": []string{},
		},
	},

	// Email Parse
	{
		Name:        "fm_parse_email",
		Description: "Parse an uploaded .eml blob into structured email fields (from, to, subject, body, attachments) without importing it into a mailbox.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"blobId": m{"type": "string", "description": "Blob ID of the uploaded RFC 5322 .eml file"},
			},
			"required": []string{"blobId"},
		},
	},

	// MDN (Read Receipts)
	{
		Name:        "fm_send_read_receipt",
		Description: "Send a read receipt (MDN) acknowledging that an email was read.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"forEmailId": m{"type": "string", "description": "Email ID to acknowledge"},
				"subject":    m{"type": "string", "description": "Custom subject for the receipt (optional)"},
				"textBody":   m{"type": "string", "description": "Custom body text (optional)"},
			},
			"required": []string{"forEmailId"},
		},
	},
	{
		Name:        "fm_parse_read_receipt",
		Description: "Parse a received read receipt (MDN) to extract disposition and original message info.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"blobId": m{"type": "string", "description": "Blob ID of the MDN email"},
			},
			"required": []string{"blobId"},
		},
	},

	// Email Import
	{
		Name:        "fm_import_email",
		Description: "Import a raw RFC 5322 email message from a previously uploaded blob.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"blobId":     m{"type": "string", "description": "Blob ID of the uploaded RFC 5322 message"},
				"mailboxId":  m{"type": "string", "description": "Mailbox to import into"},
				"keywords":   m{"type": "object", "description": "Keywords to set (e.g. {\"$seen\": true})"},
				"receivedAt": m{"type": "string", "description": "Override received date (UTC datetime)"},
			},
			"required": []string{"blobId", "mailboxId"},
		},
	},

	// Agentic Workflow
	{
		Name:        "fm_list_email_ids",
		Description: "Lightweight mailbox scan — returns only IDs, sender, subject, date, read/flag status. Up to 1000 per call. Use for fast triage before batch operations.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"mailboxId":  m{"type": "string", "description": "Mailbox ID to scan"},
				"limit":      m{"type": "integer", "description": "Max emails (default 100, max 1000)"},
				"offset":     m{"type": "integer", "description": "Offset for pagination"},
				"onlyUnread": m{"type": "boolean", "description": "Only unread emails (default false)"},
			},
			"required": []string{"mailboxId"},
		},
	},
	{
		Name:        "fm_batch_get_emails",
		Description: "Fetch multiple emails by ID with text bodies in one call. Max 50 per batch.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"ids":         m{"type": "array", "description": "Email IDs to fetch (max 50)", "items": m{"type": "string"}},
				"includeHTML": m{"type": "boolean", "description": "Also include HTML bodies (default false)"},
			},
			"required": []string{"ids"},
		},
	},
	{
		Name:        "fm_get_mailbox_stats",
		Description: "Aggregate statistics for a mailbox: top senders, top domains, date range, size. Scans up to 1000 emails for a statistical overview — ideal for planning cleanup and Sieve rules. Use 'after' to limit scan range on large mailboxes.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"mailboxId":  m{"type": "string", "description": "Mailbox ID to analyze"},
				"maxScan":    m{"type": "integer", "description": "Max emails to scan (default 500, max 1000)"},
				"onlyUnread": m{"type": "boolean", "description": "Only analyze unread emails (default false)"},
				"after":      m{"type": "string", "description": "Only scan emails after this date (UTC, e.g. 2026-01-01T00:00:00Z). Recommended for large mailboxes."},
			},
			"required": []string{"mailboxId"},
		},
	},
	{
		Name:        "fm_get_sieve_capabilities",
		Description: "Get the server's supported Sieve extensions and limits. Use before writing Sieve scripts to know which features are available (e.g. vnd.cyrus.jmapquery, regex, editheader).",
		InputSchema: m{"type": "object", "properties": m{}, "required": []string{}},
	},
	// Spam Reporting
	{
		Name:        "fm_report_spam",
		Description: "Report email(s) as spam. Moves to Junk, sets $junk keyword to train Fastmail's spam filter, and marks as read. Prefer this over unsubscribing — unsubscribing confirms a live address to spammers.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"ids": m{"type": "array", "description": "Email IDs to report as spam", "items": m{"type": "string"}},
			},
			"required": []string{"ids"},
		},
	},
	{
		Name:        "fm_report_phishing",
		Description: "Report email(s) as phishing. Moves to Junk, sets $phishing and $junk keywords. Use for fraudulent emails impersonating legitimate senders.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"ids": m{"type": "array", "description": "Email IDs to report as phishing", "items": m{"type": "string"}},
			},
			"required": []string{"ids"},
		},
	},
	{
		Name:        "fm_report_not_spam",
		Description: "Report email(s) as not spam (false positive). Moves out of Junk, removes $junk/$phishing keywords, sets $notjunk to train the filter.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"ids":       m{"type": "array", "description": "Email IDs to report as not spam", "items": m{"type": "string"}},
				"mailboxId": m{"type": "string", "description": "Destination mailbox (default: Inbox)"},
			},
			"required": []string{"ids"},
		},
	},

	// Archive & Destroy
	{
		Name:        "fm_archive_email",
		Description: "Move email(s) to the Archive mailbox.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"ids": m{"type": "array", "description": "Email IDs to archive", "items": m{"type": "string"}},
			},
			"required": []string{"ids"},
		},
	},
	{
		Name:        "fm_destroy_email",
		Description: "Permanently delete email(s), bypassing Trash. Cannot be undone. Use fm_delete_email for recoverable deletion.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"ids": m{"type": "array", "description": "Email IDs to permanently delete", "items": m{"type": "string"}},
			},
			"required": []string{"ids"},
		},
	},
	{
		Name:        "fm_unsnooze_email",
		Description: "Clear the snooze on an email, returning it to its current mailbox immediately.",
		InputSchema: m{
			"type":       "object",
			"properties": m{"id": m{"type": "string", "description": "Email ID to unsnooze"}},
			"required":   []string{"id"},
		},
	},

	{
		Name:        "fm_find_duplicates",
		Description: "Scan a mailbox for duplicate emails. Groups by Message-ID header (or subject+from+date fallback). Returns duplicate groups with suggested IDs to keep/delete. Use with fm_delete_email to remove duplicates.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"mailboxId": m{"type": "string", "description": "Mailbox ID to scan for duplicates"},
				"maxScan":   m{"type": "integer", "description": "Max emails to scan (default 1000, max 5000)"},
			},
			"required": []string{"mailboxId"},
		},
	},

	// Newsletter / Mailing List
	{
		Name:        "fm_detect_newsletters",
		Description: "Scan a mailbox for newsletters and mailing lists by detecting List-Id/List-Unsubscribe headers. Returns aggregated list with sender, count, and whether RFC 8058 one-click unsubscribe is supported. Use 'after' to limit scan range on large mailboxes.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"mailboxId": m{"type": "string", "description": "Mailbox ID to scan"},
				"maxScan":   m{"type": "integer", "description": "Max emails to scan (default 500, max 2000)"},
				"after":     m{"type": "string", "description": "Only scan emails after this date (UTC, e.g. 2025-01-01T00:00:00Z). Recommended for large mailboxes."},
			},
			"required": []string{"mailboxId"},
		},
	},
	{
		Name:        "fm_unsubscribe_list",
		Description: "Unsubscribe from a mailing list using RFC 8058 one-click List-Unsubscribe-Post. Only works with TRUSTED senders that support the standard. For untrusted/spam senders, use fm_report_spam instead — unsubscribing from spam confirms your address is live.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"emailId": m{"type": "string", "description": "An email ID from the list to unsubscribe from"},
			},
			"required": []string{"emailId"},
		},
	},

	// Draft Management
	{
		Name:        "fm_create_draft",
		Description: "Save an email as a draft without sending. Returns the draft ID for later editing or sending.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"to":      m{"type": "array", "description": "Recipients (optional for drafts)", "items": m{}},
				"cc":      m{"type": "array", "description": "CC recipients", "items": m{}},
				"subject": m{"type": "string", "description": "Subject"},
				"body":    m{"type": "string", "description": "Plain text body"},
			},
			"required": []string{},
		},
	},
	{
		Name:        "fm_list_drafts",
		Description: "List saved email drafts.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"limit": m{"type": "integer", "description": "Max drafts to return (default 20, max 200)"},
			},
			"required": []string{},
		},
	},

	// Email Forwarding
	{
		Name:        "fm_forward_email",
		Description: "Forward an email to new recipients. Includes the original message in the body.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"emailId": m{"type": "string", "description": "Email ID to forward"},
				"to":      m{"type": "array", "description": "Recipients to forward to", "items": m{}},
				"comment": m{"type": "string", "description": "Optional message to add above the forwarded content"},
				"subject": m{"type": "string", "description": "Custom subject (default: 'Fwd: <original subject>')"},
			},
			"required": []string{"emailId", "to"},
		},
	},

	// Follow-up Finder
	{
		Name:        "fm_find_unreplied",
		Description: "Find sent emails that haven't received a reply. Scans Sent mailbox and checks thread sizes to identify unreplied conversations.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"maxScan": m{"type": "integer", "description": "Max sent emails to scan (default 200, max 500)"},
				"daysOld": m{"type": "integer", "description": "Only check emails sent within this many days (default 3, max 90)"},
			},
			"required": []string{},
		},
	},

	// Sender Analysis
	{
		Name:        "fm_analyze_sender",
		Description: "Analyze a sender: email count, date range, mailbox distribution, read/flag rates, mailing list headers, authentication results, and top subjects. Useful for deciding whether to report spam, unsubscribe, or create a Sieve rule.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"email":   m{"type": "string", "description": "Sender email address to analyze"},
				"maxScan": m{"type": "integer", "description": "Max emails to scan (default 200, max 500)"},
			},
			"required": []string{"email"},
		},
	},

	// Sieve Filter Management
	{
		Name:        "fm_list_sieve_scripts",
		Description: "List all Sieve filter scripts with name and active status.",
		InputSchema: m{"type": "object", "properties": m{}, "required": []string{}},
	},
	{
		Name:        "fm_get_sieve_script",
		Description: "Get a Sieve script by ID, including the full script source code.",
		InputSchema: m{
			"type":       "object",
			"properties": m{"id": m{"type": "string", "description": "Script ID"}},
			"required":   []string{"id"},
		},
	},
	{
		Name: "fm_set_sieve_script",
		Description: `Create or update a Sieve filter script. Provide the full Sieve source in 'content'.
If 'id' is provided, updates that script; otherwise creates a new one. Set activate=true to make it the active script.
Fastmail Sieve supports: fileinto, reject, vacation, body, regex, variables, imap4flags, editheader, duplicate,
vnd.cyrus.jmapquery (use JMAP filter syntax in Sieve!), vnd.cyrus.snooze, and many more.`,
		InputSchema: m{
			"type": "object",
			"properties": m{
				"content":  m{"type": "string", "description": "Full Sieve script source code"},
				"name":     m{"type": "string", "description": "Script name (must be unique)"},
				"id":       m{"type": "string", "description": "Script ID to update (omit to create new)"},
				"activate": m{"type": "boolean", "description": "Activate this script after saving (default false)"},
			},
			"required": []string{"content"},
		},
	},
	{
		Name:        "fm_delete_sieve_script",
		Description: "Delete a Sieve script. Automatically deactivates it first if active.",
		InputSchema: m{
			"type":       "object",
			"properties": m{"id": m{"type": "string", "description": "Script ID to delete"}},
			"required":   []string{"id"},
		},
	},
	{
		Name:        "fm_activate_sieve_script",
		Description: "Activate a Sieve script (only one can be active). Omit id to deactivate all scripts.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"id": m{"type": "string", "description": "Script ID to activate (omit to deactivate all)"},
			},
			"required": []string{},
		},
	},
	{
		Name:        "fm_validate_sieve_script",
		Description: "Validate Sieve script syntax without saving. Returns valid=true or error details.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"content": m{"type": "string", "description": "Sieve script source to validate"},
			},
			"required": []string{"content"},
		},
	},
}

// ── Tool Handler Map ───────────────────────────────────────────────────────

var toolHandlers = map[string]toolFunc{
	// Email
	"fm_list_mailboxes":       listMailboxes,
	"fm_list_emails":          listEmails,
	"fm_get_email":            getEmail,
	"fm_search_emails":        searchEmails,
	"fm_send_email":           sendEmail,
	"fm_mark_read":            markRead,
	"fm_move_email":           moveEmail,
	"fm_delete_email":         deleteEmail,
	// Thread
	"fm_get_thread":           getThread,
	// Mailbox Management
	"fm_create_mailbox":       createMailbox,
	"fm_rename_mailbox":       renameMailbox,
	"fm_delete_mailbox":       deleteMailbox,
	// Bridge
	"fm_list_bridge_messages": listBridgeMessages,
	"fm_ack_bridge_message":   ackBridgeMessage,
	// Calendar
	"fm_list_calendars":       listCalendars,
	"fm_list_events":          listEvents,
	"fm_get_event":            getEvent,
	"fm_create_event":         createEvent,
	"fm_update_event":         updateEvent,
	"fm_delete_event":         deleteEvent,
	"fm_rsvp_event":           rsvpEvent,
	// Contacts
	"fm_list_contacts":        listContacts,
	"fm_get_contact":          getContact,
	"fm_create_contact":       createContact,
	"fm_update_contact":       updateContact,
	"fm_delete_contact":       deleteContact,
	"fm_list_address_books":   listAddressBooks,
	// Identity
	"fm_list_identities":      listIdentities,
	"fm_update_identity":      updateIdentity,
	// Masked Email
	"fm_list_masked_emails":   listMaskedEmails,
	"fm_create_masked_email":  createMaskedEmail,
	"fm_update_masked_email":  updateMaskedEmail,
	// Vacation
	"fm_get_vacation_response": getVacationResponse,
	"fm_set_vacation_response": setVacationResponse,
	// Snooze & Flag
	"fm_snooze_email":         snoozeEmail,
	"fm_flag_email":           flagEmail,
	// Quota
	"fm_get_quota":            getQuota,
	// Attachment
	"fm_download_attachment":  downloadAttachment,
	// Calendar Management
	"fm_create_calendar":      createCalendar,
	"fm_update_calendar":      updateCalendar,
	"fm_delete_calendar":      deleteCalendar,
	// Address Book Management
	"fm_create_address_book":  createAddressBook,
	"fm_delete_address_book":  deleteAddressBook,
	// Delivery Tracking
	"fm_get_email_submission": getEmailSubmission,
	// Email Parse
	"fm_parse_email":          parseEmail,
	// MDN
	"fm_send_read_receipt":    sendMDN,
	"fm_parse_read_receipt":   parseMDN,
	// Import
	"fm_import_email":         importEmail,
	// Agentic Workflow
	"fm_list_email_ids":        listEmailIDs,
	"fm_batch_get_emails":      batchGetEmails,
	"fm_get_mailbox_stats":     getMailboxStats,
	"fm_get_sieve_capabilities": getSieveCapabilities,
	"fm_find_duplicates":        findDuplicates,
	// Spam Reporting
	"fm_report_spam":           reportSpam,
	"fm_report_phishing":       reportPhishing,
	"fm_report_not_spam":       reportNotSpam,
	// Archive & Destroy
	"fm_archive_email":         archiveEmail,
	"fm_destroy_email":         destroyEmail,
	"fm_unsnooze_email":        unsnoozeEmail,
	// Newsletter / Mailing List
	"fm_detect_newsletters":   detectNewsletters,
	"fm_unsubscribe_list":     unsubscribeList,
	// Draft Management
	"fm_create_draft":         createDraft,
	"fm_list_drafts":          listDrafts,
	// Forwarding
	"fm_forward_email":        forwardEmail,
	// Follow-up
	"fm_find_unreplied":       findUnreplied,
	// Sender Analysis
	"fm_analyze_sender":       analyzeSender,
	// Sieve
	"fm_list_sieve_scripts":   listSieveScripts,
	"fm_get_sieve_script":     getSieveScript,
	"fm_set_sieve_script":     setSieveScript,
	"fm_delete_sieve_script":  deleteSieveScript,
	"fm_activate_sieve_script": activateSieveScript,
	"fm_validate_sieve_script": validateSieveScript,
}
