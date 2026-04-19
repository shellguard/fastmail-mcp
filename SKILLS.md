# Fastmail MCP — AI Playbooks

This document teaches an AI agent how to orchestrate the 56 fastmail-mcp tools for real workflows. It covers patterns, anti-patterns, and decision trees for common tasks.

**Read this before acting on user requests involving email, contacts, calendars, or mail organization.**

---

## Core Principles

1. **Reconnaissance before action.** Always scan before bulk-modifying. Use `fm_get_mailbox_stats` or `fm_list_email_ids` to understand the landscape before moving, deleting, or reporting.
2. **Report spam, never unsubscribe.** Unsubscribing confirms a live address to spammers. Use `fm_report_spam` instead — it trains Fastmail's Bayesian filter.
3. **Validate before deploying Sieve.** Always `fm_validate_sieve_script` before `fm_set_sieve_script`. A broken Sieve script can silently drop mail.
4. **Prefer recoverable actions.** Use `fm_delete_email` (moves to Trash) over `fm_destroy_email` (permanent) unless the user explicitly asks for permanent deletion.
5. **Batch efficiently.** Most tools accept arrays of IDs. Collect IDs first, then act in bulk — don't loop one-at-a-time.

---

## Playbook: Inbox Cleanup

**When the user says:** "Clean up my inbox", "Organize my email", "I have too many unread emails"

### Step 1: Assess

```
fm_list_mailboxes
→ Find inbox ID, note unread counts across all mailboxes

fm_get_mailbox_stats(mailboxId=inboxId, onlyUnread=true)
→ Returns top 50 senders, top 30 domains, date range, total size
→ This is the single most important call — it tells you what to prioritize
```

### Step 2: Present findings to user

Summarize: "You have 3,200 unread emails. Top senders: newsletter@company.com (847), noreply@social.com (423), notifications@service.com (312). Want me to handle these?"

### Step 3: Act on user direction

For each category the user approves:

- **Newsletters/bulk mail → Archive or delete:**
  ```
  fm_list_email_ids(mailboxId=inboxId, limit=1000, onlyUnread=true)
  → Filter IDs by sender pattern
  fm_archive_email(ids=[...]) or fm_delete_email(ids=[...])
  ```

- **Spam → Report:**
  ```
  fm_report_spam(ids=[...])
  ```

- **Important but read → Mark read:**
  ```
  fm_mark_read(ids=[...], read=true)
  ```

### Step 4: Automate with Sieve

After cleanup, propose Sieve rules to prevent reaccumulation:

```
fm_get_sieve_capabilities()
→ Check which extensions are available

fm_validate_sieve_script(content="require [\"fileinto\"];\nif address :contains \"from\" \"newsletter@\" { fileinto \"Archive\"; }")
→ Verify syntax

fm_set_sieve_script(content=..., name="inbox-org", activate=true)
→ Deploy
```

---

## Playbook: Spam Cleanup

**When the user says:** "I'm getting a lot of spam", "Help me deal with junk mail", "Train my spam filter"

### Step 1: Analyze the Junk folder and Inbox

```
fm_list_mailboxes → find Inbox and Junk IDs

fm_get_mailbox_stats(mailboxId=inboxId)
→ Look for suspicious high-volume senders

fm_get_mailbox_stats(mailboxId=junkId)
→ See what's already being caught — check for false positives
```

### Step 2: Report spam from Inbox

```
fm_list_email_ids(mailboxId=inboxId, limit=1000)
→ Identify spam by sender patterns

fm_report_spam(ids=[...spam IDs...])
→ Moves to Junk + sets $junk keyword (trains filter)
```

### Step 3: Check for false positives in Junk

```
fm_list_email_ids(mailboxId=junkId, limit=500)
→ Look for legitimate senders

fm_report_not_spam(ids=[...legitimate IDs...])
→ Moves to Inbox + sets $notjunk (retrains filter)
```

### Step 4: Report phishing separately

Phishing gets different treatment than regular spam:
```
fm_report_phishing(ids=[...phishing IDs...])
→ Sets $phishing keyword — Fastmail handles this more aggressively
```

### Step 5: Write Sieve rules for persistent spam patterns

```
fm_get_sieve_capabilities()

fm_validate_sieve_script(content="""
require ["fileinto", "reject", "vnd.cyrus.jmapquery"];

# Block known spam domains
if address :matches "from" ["*@spammy.biz", "*@sketchy.ru"] {
    discard;
    stop;
}

# Auto-junk emails with spam-like subjects
if header :regex "subject" "(?i)(viagra|casino|lottery|congratulations.*winner)" {
    fileinto "Junk";
    stop;
}
""")

fm_set_sieve_script(content=..., name="antispam", activate=true)
```

### Anti-patterns to AVOID

- **Never unsubscribe from spam.** It confirms the address is live and monitored.
- **Never reply to spam.** Same reason.
- **Don't just delete spam — report it.** `fm_delete_email` only moves to Trash. `fm_report_spam` actually trains the filter.
- **Don't use `discard` in Sieve for uncertain cases.** Use `fileinto "Junk"` so the user can review. Reserve `discard` for confirmed spam patterns.

---

## Playbook: Duplicate Removal

**When the user says:** "I have duplicate emails", "Mailbox was imported multiple times", "Clean up duplicates"

### Step 1: Scan

```
fm_find_duplicates(mailboxId=targetId, maxScan=5000)
→ Returns duplicate groups with:
  - count (how many copies)
  - subject, from (for identification)
  - suggestKeep (oldest email ID)
  - suggestDeleteIds (all others)
```

### Step 2: Present and confirm

"Found 142 duplicate groups (1,847 extra copies). Largest: 'Weekly Report' from team@company.com appears 23 times. Want me to remove duplicates, keeping the oldest copy of each?"

### Step 3: Delete duplicates

```
# Collect all suggestDeleteIds from all groups
fm_delete_email(ids=[...all suggestDeleteIds...])
```

### Step 4: Check across multiple mailboxes

If duplicates span mailboxes (common after import), scan each:
```
fm_list_mailboxes → get all mailbox IDs
fm_find_duplicates(mailboxId=...) → for each relevant mailbox
```

---

## Playbook: Sieve Rule Authoring

**When the user says:** "Create a filter for...", "Auto-sort my email", "Block emails from..."

### Step 1: Check capabilities

```
fm_get_sieve_capabilities()
→ Returns supported extensions list
→ Key ones: fileinto, reject, vacation, body, regex, variables,
   imap4flags, editheader, duplicate, vnd.cyrus.jmapquery
```

### Step 2: Check existing scripts

```
fm_list_sieve_scripts()
→ See what's already active — don't overwrite without asking

If a script exists:
  fm_get_sieve_script(id=activeScriptId)
  → Read current rules — merge new rules into existing script
```

### Step 3: Write and validate

```
fm_validate_sieve_script(content=newScript)
→ MUST pass before deploying
```

### Step 4: Deploy

```
fm_set_sieve_script(content=newScript, name="my-filters", activate=true)
```

### Sieve patterns reference

**File to folder:**
```sieve
require ["fileinto"];
if address :contains "from" "github.com" { fileinto "Dev/GitHub"; stop; }
```

**Using JMAP filter syntax (Fastmail-specific, very powerful):**
```sieve
require ["fileinto", "vnd.cyrus.jmapquery"];
if jmapquery "{\"from\":\"@marketing.example.com\",\"hasAttachment\":false}" {
    fileinto "Newsletters";
    stop;
}
```

**Flag and keep in inbox:**
```sieve
require ["imap4flags"];
if address :is "from" "boss@company.com" {
    addflag "\\Flagged";
}
```

**Reject with message:**
```sieve
require ["reject"];
if address :matches "from" "*@blocked-domain.com" {
    reject "Message refused.";
    stop;
}
```

**Vacation auto-responder (alternative to fm_set_vacation_response):**
```sieve
require ["vacation"];
vacation :days 7 :subject "Out of Office"
    "I'm currently unavailable. I'll respond when I return.";
```

**Discard silently (use sparingly):**
```sieve
if header :contains "X-Spam-Score" "10" { discard; stop; }
```

**Deduplicate incoming (prevent future duplicates):**
```sieve
require ["duplicate"];
if duplicate { discard; stop; }
```

### Important Sieve rules

- `stop;` prevents further rule processing — always include it after terminal actions.
- Rules are evaluated top-to-bottom. Put specific rules before general ones.
- `require` statements must be at the top, before any rules.
- Multiple `require` extensions can be combined: `require ["fileinto", "reject", "regex"];`
- When merging with existing scripts, preserve all existing `require` statements and add new ones.

---

## Playbook: Calendar Management

**When the user says:** "What's on my calendar?", "Schedule a meeting", "Cancel the event"

### Viewing

```
fm_list_calendars()
→ Find calendar IDs, see which are visible

fm_list_events(after="2026-04-18T00:00:00Z", before="2026-04-25T00:00:00Z")
→ This week's events

fm_get_event(id=eventId)
→ Full details including participants, recurrence, alerts
```

### Creating

```
fm_create_event(
    calendarId="...",
    title="Team standup",
    start="2026-04-21T09:00:00",
    timeZone="America/New_York",
    duration="PT30M",
    description="Daily sync"
)
```

**All-day events:**
```
fm_create_event(
    calendarId="...",
    title="Company holiday",
    start="2026-04-21",
    showWithoutTime=true,
    duration="P1D"
)
```

### Responding to invitations

```
fm_rsvp_event(id=eventId, status="accepted")
→ status: accepted, declined, tentative, needs-action
```

### Duration format (ISO 8601)

- `PT30M` = 30 minutes
- `PT1H` = 1 hour
- `PT1H30M` = 1.5 hours
- `P1D` = 1 day
- `P1W` = 1 week

---

## Playbook: Masked Email (Privacy Aliases)

**When the user says:** "Create a throwaway email for this site", "I need a masked address", "Manage my aliases"

### Create for a new signup

```
fm_create_masked_email(forDomain="shopping-site.com", description="Shopping site account")
→ Returns generated address like abc123@fastmail.com
```

### Disable when done

```
fm_list_masked_emails()
→ Find the alias

fm_update_masked_email(id=aliasId, state="disabled")
→ Emails to this address will now bounce
```

### Investigate which alias is leaking

```
fm_list_masked_emails(state="enabled")
→ Check lastMessageAt dates — recent activity on old aliases suggests a data breach/sale

fm_search_emails(query="{\"to\":\"alias@fastmail.com\"}")
→ See what's coming in on that alias
```

---

## Playbook: Contact Management

**When the user says:** "Add a contact", "Update their phone number", "Find John's email"

### Search first, create if needed

```
fm_list_contacts(search="John Smith")
→ Check if contact already exists

If not found:
fm_create_contact(firstName="John", lastName="Smith", emails=["john@example.com"], phones=["555-0123"], company="Acme Inc")
```

### Simple field formats

Contacts accept both simple and JSContact formats:
- **Emails:** `["john@example.com"]` or `[{"address": "john@example.com", "label": "work"}]`
- **Phones:** `["555-0123"]` or `[{"number": "555-0123", "label": "mobile"}]`

---

## Playbook: Thread Investigation

**When the user says:** "Show me the full conversation", "What was the context of this email?"

```
fm_get_thread(id=emailId)
→ Returns all emails in the conversation thread, oldest to newest, with bodies
→ The input is any email ID in the thread — it resolves the full thread automatically
```

---

## Playbook: Attachment Handling

**When the user says:** "Download the attachment", "What files are in this email?"

### Step 1: Get email with attachment details

```
fm_get_email(id=emailId)
→ Response includes attachments array: [{blobId, name, type, size}, ...]
```

### Step 2: Get download URL

```
fm_download_attachment(blobId="...", name="report.pdf", type="application/pdf")
→ Returns a download URL (credential-bearing — don't share publicly)
```

---

## Playbook: Vacation / Out of Office

**When the user says:** "Set up my out-of-office", "Turn off auto-reply"

### Enable

```
fm_set_vacation_response(
    isEnabled=true,
    subject="Out of Office",
    textBody="I'm away until April 28. For urgent matters, contact backup@company.com.",
    fromDate="2026-04-21T00:00:00Z",
    toDate="2026-04-28T00:00:00Z"
)
```

### Check current status

```
fm_get_vacation_response()
```

### Disable

```
fm_set_vacation_response(isEnabled=false)
```

---

## Playbook: Mailbox Organization

**When the user says:** "Create a folder for...", "Reorganize my mailboxes"

### Create nested structure

```
fm_create_mailbox(name="Projects")
→ Returns ID, e.g. "mb-projects"

fm_create_mailbox(name="Active", parentId="mb-projects")
fm_create_mailbox(name="Archived", parentId="mb-projects")
→ Creates Projects/Active and Projects/Archived
```

### Move mailbox

```
fm_rename_mailbox(id=mailboxId, parentId=newParentId)
→ Moves it under a different parent (pass null for top-level)
```

### Delete empty mailbox

```
fm_delete_mailbox(id=mailboxId)
→ Fails if mailbox contains emails

fm_delete_mailbox(id=mailboxId, deleteContents=true)
→ Deletes mailbox AND all emails inside — use with caution
```

---

## Playbook: Quota Management

**When the user says:** "How much storage am I using?", "Am I running low on space?"

```
fm_get_quota()
→ Returns used/hardLimit in bytes
→ Convert to human-readable: used/1073741824 = GB
```

If storage is tight, combine with cleanup:
1. `fm_get_mailbox_stats` on largest mailboxes to find what's consuming space
2. `fm_find_duplicates` to remove duplicate copies
3. `fm_destroy_email` on confirmed junk to reclaim space (Trash still uses quota)

---

## Decision Trees

### "Should I delete, archive, or report this email?"

```
Is it spam/unwanted commercial email?
  → fm_report_spam (trains filter)

Is it a phishing/fraud attempt?
  → fm_report_phishing

Is it a legitimate email the user is done with?
  → fm_archive_email

Is it in Trash and needs permanent removal?
  → fm_destroy_email

Is it in Junk but actually legitimate?
  → fm_report_not_spam
```

### "Should I use Sieve or a manual action?"

```
Is this a one-time cleanup? → Manual (fm_move_email, fm_report_spam, etc.)
Will this pattern recur?    → Sieve rule (fm_set_sieve_script)
Is the pattern complex?     → Sieve with vnd.cyrus.jmapquery
```

### "How do I handle a large mailbox (10,000+ emails)?"

```
1. fm_get_mailbox_stats → understand the landscape (one call)
2. fm_list_email_ids with offset pagination → scan in batches of 1000
3. fm_batch_get_emails → read specific emails (50 at a time) only when needed
4. Bulk action tools → operate on collected ID arrays
5. fm_set_sieve_script → automate for the future
```

---

## Playbook: Newsletter Management

**When the user says:** "Manage my newsletters", "Too many mailing lists", "Unsubscribe from..."

### Step 1: Detect all newsletters

```
fm_detect_newsletters(mailboxId=inboxId, maxScan=2000)
→ Returns newsletters sorted by volume:
  { from, name, count, listId, unsubscribeHeader, canOneClickUnsubscribe, sampleIds }
```

### Step 2: Categorize with the user

Present the list and ask which category each falls into:
- **Keep + auto-file** → Sieve rule to fileinto a folder
- **Keep in inbox** → no action
- **Legitimate but unwanted** → `fm_unsubscribe_list` (RFC 8058 one-click)
- **Spam/unwanted** → `fm_report_spam` (never unsubscribe from spam!)

### Step 3: For legitimate unsubscribes

```
# Only if canOneClickUnsubscribe=true
fm_unsubscribe_list(emailId=sampleId)
→ Sends RFC 8058 POST to the sender's unsubscribe endpoint
```

If `canOneClickUnsubscribe=false`, tell the user they need to visit the URL manually or use `fm_report_spam` if it's unwanted.

### Step 4: For spam newsletters

```
fm_report_spam(ids=[...all IDs from that sender...])
→ Trains filter + moves to Junk
```

### Step 5: Auto-file wanted newsletters with Sieve

```
fm_set_sieve_script(content="""
require ["fileinto"];
if address :contains "from" "newsletter@trusted.com" { fileinto "Newsletters"; stop; }
if address :contains "from" "digest@service.com" { fileinto "Newsletters"; stop; }
""", name="newsletter-filing", activate=true)
```

### Decision tree: Unsubscribe vs Report Spam

```
Is this a legitimate company you signed up for?
  AND does canOneClickUnsubscribe=true?
    → fm_unsubscribe_list (safe RFC 8058 standard)
  AND canOneClickUnsubscribe=false?
    → Tell user to visit the unsubscribe URL manually

Is this spam you never signed up for?
  → fm_report_spam (NEVER unsubscribe — confirms your address)

Is this a legitimate sender but you want to keep receiving?
  → fm_set_sieve_script to auto-file into a folder
```

---

## Playbook: Sender Investigation

**When the user says:** "Who is sending me this?", "Is this sender legitimate?", "Should I block this sender?"

```
fm_analyze_sender(email="sender@example.com")
→ Returns:
  - totalEmails, date range (how long they've been emailing)
  - readCount/unreadCount (does the user engage?)
  - mailboxes (are they in Inbox? Junk? Archive?)
  - isMailingList (has List-Id/List-Unsubscribe headers)
  - authenticationResults (SPF/DKIM pass/fail — spam indicator)
  - topSubjects (subject patterns)
```

Based on results:
- **High volume, never read, no List-Id** → spam → `fm_report_spam`
- **Has List-Id, user reads some** → wanted newsletter → Sieve auto-file
- **Has List-Id, user never reads** → unwanted → `fm_unsubscribe_list` or `fm_report_spam`
- **Low volume, always read** → important sender → no action
- **Authentication failures** → phishing risk → `fm_report_phishing`

---

## Playbook: Draft Management

**When the user says:** "Save this as a draft", "Show my drafts", "I'll finish this later"

### Save a draft

```
fm_create_draft(to=["recipient@example.com"], subject="Meeting notes", body="Draft content...")
→ Returns draft ID
```

### List and review drafts

```
fm_list_drafts(limit=20)
→ Returns drafts with preview text
```

### To send a draft, use fm_send_email with the same content (JMAP doesn't have a "send draft" method — you create a new submission).

---

## Playbook: Email Forwarding

**When the user says:** "Forward this to...", "Send this email to someone else"

```
fm_forward_email(emailId="...", to=["colleague@company.com"], comment="FYI — see below")
→ Sends with "Fwd: <original subject>" and includes original headers + body
```

---

## Playbook: Follow-up Tracking

**When the user says:** "What emails haven't been replied to?", "Who owes me a response?"

```
fm_find_unreplied(daysOld=7)
→ Returns sent emails from the last 7 days where the thread has no reply:
  { to, subject, sentAt, daysSince }
```

Present as a follow-up list: "These 5 emails from the past week haven't received replies..."

---

## Rate Limiting Notes

- Fastmail's JMAP API has rate limits. The MCP server auto-retries on HTTP 429 (up to 2 retries with Retry-After).
- Avoid unnecessary calls. Use `fm_get_mailbox_stats` instead of fetching all emails to count senders.
- `fm_list_email_ids` is cheaper than `fm_list_emails` (no preview/body data).
- Batch IDs into single calls rather than looping: `fm_delete_email(ids=[...100 IDs...])` not 100 separate calls.
