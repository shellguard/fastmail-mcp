package main

import (
	"encoding/json"
	"fmt"
)

// ── Email Tools ─────────────────────────────────────────────────────────────

func listMailboxes(_ m) (any, error) {
	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"Mailbox/get", m{
			"accountId":  acct,
			"properties": []string{"id", "name", "role", "totalEmails", "unreadEmails", "parentId", "sortOrder"},
		}, "m0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	list, ok := respList(responses[0])
	if !ok {
		return nil, errToolError("Unexpected Mailbox/get response")
	}

	out := make([]m, 0, len(list))
	for _, mb := range list {
		d := m{
			"id":           getString(mb, "id"),
			"name":         getString(mb, "name"),
			"totalEmails":  getFloat(mb, "totalEmails"),
			"unreadEmails": getFloat(mb, "unreadEmails"),
			"sortOrder":    getFloat(mb, "sortOrder"),
		}
		if role := getString(mb, "role"); role != "" {
			d["role"] = role
		}
		if pid := getString(mb, "parentId"); pid != "" {
			d["parentId"] = pid
		}
		out = append(out, d)
	}
	return out, nil
}

func listEmails(params m) (any, error) {
	mailboxID := getString(params, "mailboxId")
	if mailboxID == "" {
		return nil, errInvalidParams("mailboxId is required")
	}
	limit := intParam(params, "limit", 20, 200)
	offset := intParam(params, "offset", 0, 0)
	onlyUnread := getBool(params, "onlyUnread")

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	filter := m{"inMailbox": mailboxID}
	if onlyUnread {
		filter["notKeyword"] = "$seen"
	}

	responses, err := jmapCall([]any{
		[]any{"Email/query", m{
			"accountId":      acct,
			"filter":         filter,
			"sort":           []m{{"property": "receivedAt", "isAscending": false}},
			"position":       offset,
			"limit":          limit,
			"collapseThreads": false,
		}, "q0"},
		[]any{"Email/get", m{
			"accountId": acct,
			"#ids":      m{"resultOf": "q0", "name": "Email/query", "path": "/ids"},
			"properties": []string{"id", "threadId", "mailboxIds", "from", "to", "subject",
				"receivedAt", "preview", "keywords", "size"},
		}, "g0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	if len(responses) < 2 {
		return nil, errToolError("Unexpected Email/query+get response")
	}

	emails, ok := respList(responses[1])
	if !ok {
		return nil, errToolError("Unexpected Email/query+get response")
	}

	mapped := make([]m, len(emails))
	for i, e := range emails {
		mapped[i] = emailSummaryDict(e)
	}

	result := m{"emails": mapped, "offset": offset, "limit": limit}
	if qData, ok := respData(responses[0]); ok {
		if total, ok := qData["total"].(float64); ok {
			result["total"] = int(total)
		}
	}
	return result, nil
}

func getEmail(params m) (any, error) {
	emailID := getString(params, "id")
	if emailID == "" {
		return nil, errInvalidParams("id is required")
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"Email/get", m{
			"accountId": acct,
			"ids":       []string{emailID},
			"properties": []string{"id", "threadId", "mailboxIds", "from", "to", "cc", "bcc",
				"replyTo", "subject", "receivedAt", "sentAt", "preview",
				"keywords", "size", "textBody", "htmlBody", "attachments",
				"bodyValues", "messageId", "inReplyTo", "references"},
			"fetchTextBodyValues": true,
			"fetchHTMLBodyValues": true,
			"maxBodyValueBytes":   1024 * 1024, // 1MB cap per body part
		}, "g0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected Email/get response")
	}

	if notFound := getStringSlice(data, "notFound"); contains(notFound, emailID) {
		return nil, errInvalidParams("Email not found: " + emailID)
	}

	list := getMapSlice(data, "list")
	if len(list) == 0 {
		return nil, errToolError("Unexpected Email/get response")
	}

	return emailDetailDict(list[0]), nil
}

func searchEmails(params m) (any, error) {
	query := getString(params, "query")
	if query == "" {
		return nil, errInvalidParams("query is required")
	}
	limit := intParam(params, "limit", 20, 200)
	mailboxID := getString(params, "mailboxId")
	includeSnippets := getBool(params, "includeSnippets")

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	// Try to parse query as JSON filter, fall back to text filter.
	// Allowlist filter keys to prevent injection of arbitrary JMAP filter operators.
	var filter m
	if err := json.Unmarshal([]byte(query), &filter); err != nil {
		filter = m{"text": query}
	} else {
		filter = sanitizeEmailFilter(filter)
	}

	if mailboxID != "" {
		filter["inMailbox"] = mailboxID
	}

	calls := []any{
		[]any{"Email/query", m{
			"accountId":       acct,
			"filter":          filter,
			"sort":            []m{{"property": "receivedAt", "isAscending": false}},
			"position":        0,
			"limit":           limit,
			"collapseThreads": false,
		}, "q0"},
		[]any{"Email/get", m{
			"accountId": acct,
			"#ids":      m{"resultOf": "q0", "name": "Email/query", "path": "/ids"},
			"properties": []string{"id", "threadId", "mailboxIds", "from", "to", "subject",
				"receivedAt", "preview", "keywords", "size"},
		}, "g0"},
	}

	if includeSnippets {
		calls = append(calls, []any{"SearchSnippet/get", m{
			"accountId":  acct,
			"filter":     filter,
			"#emailIds":  m{"resultOf": "q0", "name": "Email/query", "path": "/ids"},
		}, "s0"})
	}

	responses, err := jmapCall(calls, nil)
	if err != nil {
		return nil, err
	}

	if len(responses) < 2 {
		return nil, errToolError("Unexpected search response")
	}

	emails, ok := respList(responses[1])
	if !ok {
		return nil, errToolError("Unexpected search response")
	}

	// Build snippet map if available
	snippetMap := map[string]m{}
	if includeSnippets && len(responses) > 2 {
		if snippetList, ok := respList(responses[2]); ok {
			for _, s := range snippetList {
				if eid := getString(s, "emailId"); eid != "" {
					snippetMap[eid] = s
				}
			}
		}
	}

	mapped := make([]m, len(emails))
	for i, e := range emails {
		d := emailSummaryDict(e)
		if snip, ok := snippetMap[getString(e, "id")]; ok {
			if subj := getString(snip, "subject"); subj != "" {
				d["snippetSubject"] = subj
			}
			if prev := getString(snip, "preview"); prev != "" {
				d["snippetPreview"] = prev
			}
		}
		mapped[i] = d
	}

	result := m{"emails": mapped, "limit": limit}
	if qData, ok := respData(responses[0]); ok {
		if total, ok := qData["total"].(float64); ok {
			result["total"] = int(total)
		}
	}
	return result, nil
}

func sendEmail(params m) (any, error) {
	toArr := getMapSlice(params, "to")
	if len(toArr) == 0 {
		// Accept simple string array
		if toStrs := getStringSlice(params, "to"); len(toStrs) > 0 {
			adjusted := make(m)
			for k, v := range params {
				adjusted[k] = v
			}
			toObjs := make([]any, len(toStrs))
			for i, s := range toStrs {
				toObjs[i] = m{"email": s}
			}
			adjusted["to"] = toObjs
			return sendEmail(adjusted)
		}
		return nil, errInvalidParams("to is required (array of {name?, email})")
	}
	subject := getString(params, "subject")
	if subject == "" {
		return nil, errInvalidParams("subject is required")
	}
	body := getString(params, "body")
	if body == "" {
		return nil, errInvalidParams("body is required")
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	// Get identities
	submissionCaps := []string{
		"urn:ietf:params:jmap:core",
		"urn:ietf:params:jmap:mail",
		"urn:ietf:params:jmap:submission",
	}
	idResponses, err := jmapCall([]any{
		[]any{"Identity/get", m{"accountId": acct, "properties": []string{"id", "name", "email"}}, "i0"},
	}, submissionCaps)
	if err != nil {
		return nil, err
	}

	identities, ok := respList(idResponses[0])
	if !ok || len(identities) == 0 {
		return nil, errToolError("No sending identity found")
	}
	identity := identities[0]
	identityID := getString(identity, "id")

	fromAddr := m{"name": getString(identity, "name"), "email": getString(identity, "email")}

	// Build email object
	toAny := make([]any, len(toArr))
	for i, t := range toArr {
		toAny[i] = t
	}

	emailObj := m{
		"from":       []any{fromAddr},
		"to":         toAny,
		"subject":    subject,
		"textBody":   []m{{"partId": "body", "type": "text/plain"}},
		"bodyValues": m{"body": m{"value": body, "isEncodingProblem": false, "isTruncated": false}},
		"keywords":   m{"$seen": true, "$draft": true},
		"mailboxIds": m{},
	}

	// Optional cc
	if ccArr := getMapSlice(params, "cc"); len(ccArr) > 0 {
		ccAny := make([]any, len(ccArr))
		for i, c := range ccArr {
			ccAny[i] = c
		}
		emailObj["cc"] = ccAny
	} else if ccStrs := getStringSlice(params, "cc"); len(ccStrs) > 0 {
		ccAny := make([]any, len(ccStrs))
		for i, s := range ccStrs {
			ccAny[i] = m{"email": s}
		}
		emailObj["cc"] = ccAny
	}

	// Optional replyTo
	if replyToID := getString(params, "replyToId"); replyToID != "" {
		origResponses, err := jmapCall([]any{
			[]any{"Email/get", m{
				"accountId":  acct,
				"ids":        []string{replyToID},
				"properties": []string{"messageId", "references", "subject"},
			}, "orig0"},
		}, nil)
		if err == nil && len(origResponses) > 0 {
			if origList, ok := respList(origResponses[0]); ok && len(origList) > 0 {
				orig := origList[0]
				if msgIds := getStringSlice(orig, "messageId"); len(msgIds) > 0 {
					msgID := msgIds[0]
					emailObj["inReplyTo"] = []string{msgID}
					refs := getStringSlice(orig, "references")
					refs = append(refs, msgID)
					emailObj["references"] = refs
				}
			}
		}
	}

	// Find Drafts and Sent mailboxes
	mbResponses, err := jmapCall([]any{
		[]any{"Mailbox/get", m{
			"accountId":  acct,
			"properties": []string{"id", "name", "role"},
		}, "mg0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	var draftsID, sentID string
	if mbList, ok := respList(mbResponses[0]); ok {
		for _, mb := range mbList {
			switch getString(mb, "role") {
			case "drafts":
				draftsID = getString(mb, "id")
			case "sent":
				sentID = getString(mb, "id")
			}
		}
	}
	if draftsID == "" {
		return nil, errToolError("Could not find Drafts mailbox — required for sending")
	}
	emailObj["mailboxIds"] = m{draftsID: true}

	// Build submission args
	createID := "draft"
	submissionArgs := m{
		"accountId": acct,
		"create": m{"sub0": m{
			"identityId": identityID,
			"#emailId": m{
				"resultOf": "c0",
				"name":     "Email/set",
				"path":     "/created/" + createID + "/id",
			},
		}},
	}

	// Move to Sent on success; fall back to destroying if no Sent folder
	if sentID != "" {
		submissionArgs["onSuccessUpdateEmail"] = m{
			"#" + createID: m{
				"mailboxIds":     m{sentID: true},
				"keywords/$draft": nil,
			},
		}
	} else {
		submissionArgs["onSuccessDestroyEmail"] = []string{"#" + createID}
	}

	responses, err := jmapCall([]any{
		[]any{"Email/set", m{
			"accountId": acct,
			"create":    m{createID: emailObj},
		}, "c0"},
		[]any{"EmailSubmission/set", submissionArgs, "s0"},
	}, submissionCaps)
	if err != nil {
		return nil, err
	}

	// Check Email/set creation errors
	if len(responses) > 0 {
		if data, ok := respData(responses[0]); ok {
			if nc, ok := data["notCreated"].(map[string]any); ok {
				if errObj, ok := nc[createID].(map[string]any); ok {
					et, _ := errObj["type"].(string)
					ed, _ := errObj["description"].(string)
					return nil, errToolError(fmt.Sprintf("Failed to create email: %s — %s", et, ed))
				}
			}
		}
	}

	// Check submission errors
	if len(responses) > 1 {
		if data, ok := respData(responses[1]); ok {
			if nc, ok := data["notCreated"].(map[string]any); ok {
				if errObj, ok := nc["sub0"].(map[string]any); ok {
					et, _ := errObj["type"].(string)
					ed, _ := errObj["description"].(string)
					return nil, errToolError(fmt.Sprintf("Failed to submit email: %s — %s", et, ed))
				}
			}
		}
	}

	return m{"status": "sent", "to": toAny, "subject": subject}, nil
}

func markRead(params m) (any, error) {
	ids, err := requireIDs(params, maxBatchIDs)
	if err != nil {
		return nil, err
	}
	read := true
	if v, ok := params["read"].(bool); ok {
		read = v
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	update := m{}
	for _, id := range ids {
		if read {
			update[id] = m{"keywords/$seen": true}
		} else {
			update[id] = m{"keywords/$seen": nil}
		}
	}

	responses, err := jmapCall([]any{
		[]any{"Email/set", m{"accountId": acct, "update": update}, "u0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	failures := checkNotUpdated(responses)
	if len(failures) > 0 && len(failures) == len(ids) {
		return nil, errToolError(fmt.Sprintf("Failed to update all emails: %v", failures))
	}
	result := m{"status": "ok", "ids": ids, "read": read}
	if len(failures) > 0 {
		result["failures"] = failures
	}
	return result, nil
}

func moveEmail(params m) (any, error) {
	ids, err := requireIDs(params, maxBatchIDs)
	if err != nil {
		return nil, err
	}
	mailboxID := getString(params, "mailboxId")
	if mailboxID == "" {
		return nil, errInvalidParams("mailboxId is required")
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	update := m{}
	for _, id := range ids {
		update[id] = m{"mailboxIds": m{mailboxID: true}}
	}

	responses, err := jmapCall([]any{
		[]any{"Email/set", m{"accountId": acct, "update": update}, "u0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	failures := checkNotUpdated(responses)
	if len(failures) > 0 && len(failures) == len(ids) {
		return nil, errToolError(fmt.Sprintf("Failed to move all emails: %v", failures))
	}
	result := m{"status": "ok", "ids": ids, "mailboxId": mailboxID}
	if len(failures) > 0 {
		result["failures"] = failures
	}
	return result, nil
}

func deleteEmail(params m) (any, error) {
	ids, err := requireIDs(params, maxBatchIDs)
	if err != nil {
		return nil, err
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	// Find Trash mailbox
	mbResponses, err := jmapCall([]any{
		[]any{"Mailbox/query", m{"accountId": acct, "filter": m{"role": "trash"}}, "mq0"},
		[]any{"Mailbox/get", m{
			"accountId":  acct,
			"#ids":       m{"resultOf": "mq0", "name": "Mailbox/query", "path": "/ids"},
			"properties": []string{"id"},
		}, "mg0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	if len(mbResponses) < 2 {
		return nil, errToolError("Could not find Trash mailbox")
	}
	trashList, ok := respList(mbResponses[1])
	if !ok || len(trashList) == 0 {
		return nil, errToolError("Could not find Trash mailbox")
	}
	trashID := getString(trashList[0], "id")
	if trashID == "" {
		return nil, errToolError("Could not find Trash mailbox")
	}

	update := m{}
	for _, id := range ids {
		update[id] = m{"mailboxIds": m{trashID: true}}
	}

	responses, err := jmapCall([]any{
		[]any{"Email/set", m{"accountId": acct, "update": update}, "u0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	failures := checkNotUpdated(responses)
	if len(failures) > 0 && len(failures) == len(ids) {
		return nil, errToolError(fmt.Sprintf("Failed to delete all emails: %v", failures))
	}
	result := m{"status": "ok", "ids": ids, "movedTo": "Trash"}
	if len(failures) > 0 {
		result["failures"] = failures
	}
	return result, nil
}

// ── Agentic Workflow Tools ───────────────────────────────────────────────────

// fm_list_email_ids: lightweight scan — returns only IDs + from + subject + date
// for fast mailbox traversal without body content.
func listEmailIDs(params m) (any, error) {
	mailboxID := getString(params, "mailboxId")
	if mailboxID == "" {
		return nil, errInvalidParams("mailboxId is required")
	}
	limit := intParam(params, "limit", 100, 1000) // higher cap for scanning
	offset := intParam(params, "offset", 0, 0)
	onlyUnread := getBool(params, "onlyUnread")

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	filter := m{"inMailbox": mailboxID}
	if onlyUnread {
		filter["notKeyword"] = "$seen"
	}

	responses, err := jmapCall([]any{
		[]any{"Email/query", m{
			"accountId":       acct,
			"filter":          filter,
			"sort":            []m{{"property": "receivedAt", "isAscending": false}},
			"position":        offset,
			"limit":           limit,
			"collapseThreads": false,
			"calculateTotal":  true,
		}, "q0"},
		[]any{"Email/get", m{
			"accountId": acct,
			"#ids":      m{"resultOf": "q0", "name": "Email/query", "path": "/ids"},
			"properties": []string{"id", "from", "subject", "receivedAt", "keywords", "size"},
		}, "g0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	if len(responses) < 2 {
		return nil, errToolError("Unexpected response")
	}

	emails, ok := respList(responses[1])
	if !ok {
		return nil, errToolError("Unexpected response")
	}

	out := make([]m, len(emails))
	for i, e := range emails {
		keywords := getMap(e, "keywords")
		_, isRead := keywords["$seen"]
		_, isFlagged := keywords["$flagged"]
		from := ""
		if addrs := getMapSlice(e, "from"); len(addrs) > 0 {
			from = getString(addrs[0], "email")
			if name := getString(addrs[0], "name"); name != "" {
				from = name + " <" + from + ">"
			}
		}
		out[i] = m{
			"id":         getString(e, "id"),
			"from":       from,
			"subject":    getString(e, "subject"),
			"receivedAt": getString(e, "receivedAt"),
			"isRead":     isRead,
			"isFlagged":  isFlagged,
			"size":       getFloat(e, "size"),
		}
	}

	result := m{"emails": out, "offset": offset, "limit": limit, "count": len(out)}
	if qData, ok := respData(responses[0]); ok {
		if total, ok := qData["total"].(float64); ok {
			result["total"] = int(total)
		}
	}
	return result, nil
}

// fm_batch_get_emails: fetch multiple emails by ID with bodies in one call.
func batchGetEmails(params m) (any, error) {
	ids, err := requireIDs(params, 50)
	if err != nil {
		return nil, err
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	props := []string{"id", "threadId", "mailboxIds", "from", "to", "cc",
		"subject", "receivedAt", "preview", "keywords", "size",
		"textBody", "bodyValues"}
	fetchArgs := m{
		"accountId":           acct,
		"ids":                 ids,
		"properties":          props,
		"fetchTextBodyValues": true,
		"maxBodyValueBytes":   256 * 1024, // 256KB per body
	}

	// Optionally include HTML bodies
	if getBool(params, "includeHTML") {
		fetchArgs["properties"] = append(props, "htmlBody")
		fetchArgs["fetchHTMLBodyValues"] = true
	}

	responses, err := jmapCall([]any{
		[]any{"Email/get", fetchArgs, "g0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected Email/get response")
	}

	list := getMapSlice(data, "list")
	notFound := getStringSlice(data, "notFound")

	out := make([]m, len(list))
	for i, e := range list {
		d := emailSummaryDict(e)
		d["body"] = extractBodyText(e)
		if getBool(params, "includeHTML") {
			d["htmlBody"] = extractHTMLBody(e)
		}
		out[i] = d
	}

	result := m{"emails": out, "count": len(out)}
	if len(notFound) > 0 {
		result["notFound"] = notFound
	}
	return result, nil
}

// ── Thread Tools ────────────────────────────────────────────────────────────

func getThread(params m) (any, error) {
	emailID := getString(params, "id")
	if emailID == "" {
		return nil, errInvalidParams("id (email ID) is required")
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	// Get the email to find its threadId
	responses, err := jmapCall([]any{
		[]any{"Email/get", m{
			"accountId":  acct,
			"ids":        []string{emailID},
			"properties": []string{"threadId"},
		}, "e0"},
	}, nil)
	if err != nil {
		return nil, err
	}
	emailList := getMapSlice(must(respData(responses[0])), "list")
	if len(emailList) == 0 {
		return nil, errInvalidParams("Email not found: " + emailID)
	}
	threadID := getString(emailList[0], "threadId")
	if threadID == "" {
		return nil, errToolError("No threadId on email")
	}

	// Get the thread to find all email IDs
	threadResponses, err := jmapCall([]any{
		[]any{"Thread/get", m{
			"accountId": acct,
			"ids":       []string{threadID},
		}, "t0"},
	}, nil)
	if err != nil {
		return nil, err
	}
	threadList, ok := respList(threadResponses[0])
	if !ok || len(threadList) == 0 {
		return nil, errToolError("Thread not found: " + threadID)
	}
	threadEmailIDs := getStringSlice(threadList[0], "emailIds")
	if len(threadEmailIDs) == 0 {
		return nil, errToolError("Thread has no emails")
	}
	// Cap thread size to prevent unbounded memory usage
	if len(threadEmailIDs) > 100 {
		threadEmailIDs = threadEmailIDs[len(threadEmailIDs)-100:] // keep most recent 100
	}

	// Fetch all emails in thread
	emailResponses, err := jmapCall([]any{
		[]any{"Email/get", m{
			"accountId": acct,
			"ids":       threadEmailIDs,
			"properties": []string{"id", "threadId", "mailboxIds", "from", "to", "cc",
				"subject", "receivedAt", "preview", "keywords", "size",
				"textBody", "bodyValues"},
			"fetchTextBodyValues": true,
			"maxBodyValueBytes":   256 * 1024, // 256KB cap per body part
		}, "g0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	emails, ok := respList(emailResponses[0])
	if !ok {
		return nil, errToolError("Failed to fetch thread emails")
	}

	out := make([]m, len(emails))
	for i, e := range emails {
		d := emailSummaryDict(e)
		d["body"] = extractBodyText(e)
		out[i] = d
	}
	return m{"threadId": threadID, "emails": out}, nil
}

// ── Email Import Tools ─────────────────────────────────────────────────────

func importEmail(params m) (any, error) {
	blobID := getString(params, "blobId")
	if blobID == "" {
		return nil, errInvalidParams("blobId is required (upload the RFC 5322 message first)")
	}
	mailboxID := getString(params, "mailboxId")
	if mailboxID == "" {
		return nil, errInvalidParams("mailboxId is required")
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	importObj := m{
		"blobId":     blobID,
		"mailboxIds": m{mailboxID: true},
	}

	keywords := getMap(params, "keywords")
	if keywords != nil {
		importObj["keywords"] = keywords
	}
	if ra := getString(params, "receivedAt"); ra != "" {
		importObj["receivedAt"] = ra
	}

	responses, err := jmapCall([]any{
		[]any{"Email/import", m{
			"accountId": acct,
			"emails":    m{"imp0": importObj},
		}, "i0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected Email/import response")
	}
	if nc, ok := data["notCreated"].(map[string]any); ok {
		if errObj, ok := nc["imp0"].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to import email: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	result := m{"status": "imported"}
	if created, ok := data["created"].(map[string]any); ok {
		if e, ok := created["imp0"].(map[string]any); ok {
			result["id"] = getString(e, "id")
			result["blobId"] = getString(e, "blobId")
			result["threadId"] = getString(e, "threadId")
		}
	}
	return result, nil
}

// ── Email Parse Tools ──────────────────────────────────────────────────────

func parseEmail(params m) (any, error) {
	blobID := getString(params, "blobId")
	if blobID == "" {
		return nil, errInvalidParams("blobId is required (upload the .eml file first)")
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	properties := []string{"id", "from", "to", "cc", "bcc", "replyTo",
		"subject", "sentAt", "messageId", "inReplyTo", "references",
		"textBody", "htmlBody", "attachments", "bodyValues", "preview"}

	fetchArgs := m{
		"accountId":           acct,
		"blobIds":             []string{blobID},
		"properties":          properties,
		"fetchTextBodyValues": true,
		"fetchHTMLBodyValues": true,
		"maxBodyValueBytes":   1024 * 1024,
	}

	responses, err := jmapCall([]any{
		[]any{"Email/parse", fetchArgs, "p0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected Email/parse response")
	}

	if notParsable := getStringSlice(data, "notParsable"); contains(notParsable, blobID) {
		return nil, errToolError("Blob is not a valid RFC 5322 message: " + blobID)
	}
	if notFound := getStringSlice(data, "notFound"); contains(notFound, blobID) {
		return nil, errInvalidParams("Blob not found: " + blobID)
	}

	parsed, ok := data["parsed"].(map[string]any)
	if !ok {
		return nil, errToolError("Unexpected Email/parse response")
	}

	email, ok := parsed[blobID].(map[string]any)
	if !ok {
		return nil, errToolError("No parsed result for blob")
	}

	return emailDetailDict(email), nil
}

// ── Draft Tools ────────────────────────────────────────────────────────────

func createDraft(params m) (any, error) {
	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	// Find Drafts mailbox
	draftsID, err := findMailboxByRole(acct, "drafts")
	if err != nil {
		return nil, err
	}

	// Build email object
	emailObj := m{
		"mailboxIds": m{draftsID: true},
		"keywords":   m{"$seen": true, "$draft": true},
	}

	if subject := getString(params, "subject"); subject != "" {
		emailObj["subject"] = subject
	}
	if body := getString(params, "body"); body != "" {
		emailObj["textBody"] = []m{{"partId": "body", "type": "text/plain"}}
		emailObj["bodyValues"] = m{"body": m{"value": body, "isEncodingProblem": false, "isTruncated": false}}
	}

	// To
	if toArr := getMapSlice(params, "to"); len(toArr) > 0 {
		toAny := make([]any, len(toArr))
		for i, t := range toArr {
			toAny[i] = t
		}
		emailObj["to"] = toAny
	} else if toStrs := getStringSlice(params, "to"); len(toStrs) > 0 {
		toAny := make([]any, len(toStrs))
		for i, s := range toStrs {
			toAny[i] = m{"email": s}
		}
		emailObj["to"] = toAny
	}

	// Cc
	if ccArr := getMapSlice(params, "cc"); len(ccArr) > 0 {
		ccAny := make([]any, len(ccArr))
		for i, c := range ccArr {
			ccAny[i] = c
		}
		emailObj["cc"] = ccAny
	} else if ccStrs := getStringSlice(params, "cc"); len(ccStrs) > 0 {
		ccAny := make([]any, len(ccStrs))
		for i, s := range ccStrs {
			ccAny[i] = m{"email": s}
		}
		emailObj["cc"] = ccAny
	}

	// From identity
	idResponses, err := jmapCall([]any{
		[]any{"Identity/get", m{"accountId": acct, "properties": []string{"id", "name", "email"}}, "i0"},
	}, submissionCapsGlobal)
	if err == nil && len(idResponses) > 0 {
		if idList, ok := respList(idResponses[0]); ok && len(idList) > 0 {
			emailObj["from"] = []any{m{
				"name":  getString(idList[0], "name"),
				"email": getString(idList[0], "email"),
			}}
		}
	}

	responses, err := jmapCall([]any{
		[]any{"Email/set", m{
			"accountId": acct,
			"create":    m{"draft0": emailObj},
		}, "c0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected Email/set response")
	}
	if nc, ok := data["notCreated"].(map[string]any); ok {
		if errObj, ok := nc["draft0"].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to create draft: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	result := m{"status": "created"}
	if created, ok := data["created"].(map[string]any); ok {
		if d, ok := created["draft0"].(map[string]any); ok {
			result["id"] = getString(d, "id")
		}
	}
	return result, nil
}

func listDrafts(params m) (any, error) {
	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	draftsID, err := findMailboxByRole(acct, "drafts")
	if err != nil {
		return nil, err
	}

	limit := intParam(params, "limit", 20, 200)

	responses, err := jmapCall([]any{
		[]any{"Email/query", m{
			"accountId":       acct,
			"filter":          m{"inMailbox": draftsID},
			"sort":            []m{{"property": "receivedAt", "isAscending": false}},
			"limit":           limit,
			"collapseThreads": false,
		}, "q0"},
		[]any{"Email/get", m{
			"accountId": acct,
			"#ids":      m{"resultOf": "q0", "name": "Email/query", "path": "/ids"},
			"properties": []string{"id", "to", "cc", "subject", "receivedAt", "preview", "size"},
		}, "g0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	if len(responses) < 2 {
		return nil, errToolError("Unexpected response")
	}
	emails, ok := respList(responses[1])
	if !ok {
		return nil, errToolError("Unexpected response")
	}

	out := make([]m, len(emails))
	for i, e := range emails {
		out[i] = m{
			"id":         getString(e, "id"),
			"to":         formatAddresses(e["to"]),
			"cc":         formatAddresses(e["cc"]),
			"subject":    getString(e, "subject"),
			"receivedAt": getString(e, "receivedAt"),
			"preview":    getString(e, "preview"),
			"size":       getFloat(e, "size"),
		}
	}
	return m{"drafts": out, "count": len(out)}, nil
}

// ── Email Forwarding Tools ──────────────────────────────────────────────────

func forwardEmail(params m) (any, error) {
	emailID := getString(params, "emailId")
	if emailID == "" {
		return nil, errInvalidParams("emailId is required (the email to forward)")
	}
	toArr := getMapSlice(params, "to")
	if len(toArr) == 0 {
		if toStrs := getStringSlice(params, "to"); len(toStrs) > 0 {
			adjusted := make(m)
			for k, v := range params {
				adjusted[k] = v
			}
			toObjs := make([]any, len(toStrs))
			for i, s := range toStrs {
				toObjs[i] = m{"email": s}
			}
			adjusted["to"] = toObjs
			return forwardEmail(adjusted)
		}
		return nil, errInvalidParams("to is required (recipients to forward to)")
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	// Fetch original email
	origResponses, err := jmapCall([]any{
		[]any{"Email/get", m{
			"accountId":           acct,
			"ids":                 []string{emailID},
			"properties":          []string{"from", "to", "cc", "subject", "receivedAt", "textBody", "htmlBody", "bodyValues", "attachments"},
			"fetchTextBodyValues": true,
			"maxBodyValueBytes":   1024 * 1024,
		}, "g0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	origList := getMapSlice(must(respData(origResponses[0])), "list")
	if len(origList) == 0 {
		return nil, errInvalidParams("Email not found: " + emailID)
	}
	orig := origList[0]

	// Build forwarded message
	origSubject := getString(orig, "subject")
	subject := getString(params, "subject")
	if subject == "" {
		subject = "Fwd: " + origSubject
	}

	origFrom := formatAddresses(orig["from"])
	origTo := formatAddresses(orig["to"])
	origDate := getString(orig, "receivedAt")
	origBody := extractBodyText(orig)

	// Build forward body
	comment := getString(params, "comment")
	forwardBody := ""
	if comment != "" {
		forwardBody = comment + "\n\n"
	}
	forwardBody += "---------- Forwarded message ----------\n"
	forwardBody += fmt.Sprintf("From: %v\nTo: %v\nDate: %s\nSubject: %s\n\n",
		origFrom, origTo, origDate, origSubject)
	forwardBody += origBody

	// Use sendEmail with the constructed forward
	toAny := make([]any, len(toArr))
	for i, t := range toArr {
		toAny[i] = t
	}

	return sendEmail(m{
		"to":      toAny,
		"subject": subject,
		"body":    forwardBody,
	})
}

