package main

import "fmt"

// ── Mailbox Management Tools ────────────────────────────────────────────────

func createMailbox(params m) (any, error) {
	name := getString(params, "name")
	if name == "" {
		return nil, errInvalidParams("name is required")
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	createObj := m{"name": name}
	if parentID := getString(params, "parentId"); parentID != "" {
		createObj["parentId"] = parentID
	}

	responses, err := jmapCall([]any{
		[]any{"Mailbox/set", m{
			"accountId": acct,
			"create":    m{"mb0": createObj},
		}, "c0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected Mailbox/set response")
	}
	if nc, ok := data["notCreated"].(map[string]any); ok {
		if errObj, ok := nc["mb0"].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to create mailbox: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	clearMailboxRoleCache()
	if created, ok := data["created"].(map[string]any); ok {
		if mb, ok := created["mb0"].(map[string]any); ok {
			return m{"status": "created", "id": getString(mb, "id"), "name": name}, nil
		}
	}
	return m{"status": "created", "name": name}, nil
}

func renameMailbox(params m) (any, error) {
	id := getString(params, "id")
	if id == "" {
		return nil, errInvalidParams("id is required")
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	update := m{}
	if name := getString(params, "name"); name != "" {
		update["name"] = name
	}
	if params["parentId"] != nil {
		update["parentId"] = params["parentId"] // can be null to move to top level
	}

	if len(update) == 0 {
		return nil, errInvalidParams("at least one of name or parentId is required")
	}

	responses, err := jmapCall([]any{
		[]any{"Mailbox/set", m{
			"accountId": acct,
			"update":    m{id: update},
		}, "u0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected Mailbox/set response")
	}
	if nu, ok := data["notUpdated"].(map[string]any); ok {
		if errObj, ok := nu[id].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to update mailbox: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	clearMailboxRoleCache()
	return m{"status": "ok", "id": id}, nil
}

func deleteMailbox(params m) (any, error) {
	id := getString(params, "id")
	if id == "" {
		return nil, errInvalidParams("id is required")
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	setArgs := m{
		"accountId": acct,
		"destroy":   []string{id},
	}
	if getBool(params, "deleteContents") {
		setArgs["onDestroyRemoveEmails"] = true
	}

	responses, err := jmapCall([]any{
		[]any{"Mailbox/set", setArgs, "d0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected Mailbox/set response")
	}
	if nd, ok := data["notDestroyed"].(map[string]any); ok {
		if errObj, ok := nd[id].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to delete mailbox: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	clearMailboxRoleCache()
	return m{"status": "deleted", "id": id}, nil
}

// ── Bridge Inbox Tools ──────────────────────────────────────────────────────

func listBridgeMessages(params m) (any, error) {
	bridgeName := getString(params, "mailboxName")
	if bridgeName == "" {
		bridgeName = "Bridge"
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	mbResponses, err := jmapCall([]any{
		[]any{"Mailbox/get", m{"accountId": acct, "properties": []string{"id", "name", "role"}}, "mb0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	mbList, ok := respList(mbResponses[0])
	if !ok {
		return nil, errToolError("Could not list mailboxes")
	}

	var bridgeID string
	for _, mb := range mbList {
		if getString(mb, "name") == bridgeName {
			bridgeID = getString(mb, "id")
			break
		}
	}
	if bridgeID == "" {
		return nil, errToolError(fmt.Sprintf("Mailbox '%s' not found. Create it in Fastmail first.", bridgeName))
	}

	responses, err := jmapCall([]any{
		[]any{"Email/query", m{
			"accountId": acct,
			"filter":    m{"inMailbox": bridgeID, "notKeyword": "$seen"},
			"sort":      []m{{"property": "receivedAt", "isAscending": false}},
			"limit":     50,
		}, "q0"},
		[]any{"Email/get", m{
			"accountId": acct,
			"#ids":      m{"resultOf": "q0", "name": "Email/query", "path": "/ids"},
			"properties": []string{"id", "threadId", "from", "subject", "receivedAt",
				"textBody", "bodyValues", "preview"},
			"fetchTextBodyValues": true,
			"maxBodyValueBytes":   256 * 1024, // 256KB cap for bridge messages
		}, "g0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	if len(responses) < 2 {
		return nil, errToolError("Unexpected bridge query response")
	}
	emails, ok := respList(responses[1])
	if !ok {
		return nil, errToolError("Unexpected bridge query response")
	}

	out := make([]m, len(emails))
	for i, email := range emails {
		d := m{
			"id":         getString(email, "id"),
			"from":       formatAddresses(email["from"]),
			"receivedAt": getString(email, "receivedAt"),
			"subject":    getString(email, "subject"),
			"body":       extractBodyText(email),
		}

		subj := getString(email, "subject")
		if match := parseBridgeSubject(subj); match != nil {
			d["bridgeType"] = match.typ
			d["bridgeDescription"] = match.description
		}
		out[i] = d
	}
	return out, nil
}

func ackBridgeMessage(params m) (any, error) {
	ids := getStringSlice(params, "ids")
	if len(ids) == 0 {
		// Accept single id
		if id := getString(params, "id"); id != "" {
			return ackBridgeMessage(m{"ids": []any{id}})
		}
		return nil, errInvalidParams("ids (or id) is required")
	}

	bridgeName := getString(params, "mailboxName")
	if bridgeName == "" {
		bridgeName = "Bridge"
	}
	processedName := getString(params, "processedMailboxName")
	if processedName == "" {
		processedName = "Bridge/Processed"
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	mbResponses, err := jmapCall([]any{
		[]any{"Mailbox/get", m{"accountId": acct, "properties": []string{"id", "name", "parentId"}}, "mb0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	mbList, ok := respList(mbResponses[0])
	if !ok {
		return nil, errToolError("Could not list mailboxes")
	}

	// Find Processed mailbox
	var processedID string

	// First: find Bridge mailbox, then look for Processed child
	for _, mb := range mbList {
		if getString(mb, "name") == bridgeName {
			bridgeID := getString(mb, "id")
			for _, mb2 := range mbList {
				if getString(mb2, "name") == "Processed" && getString(mb2, "parentId") == bridgeID {
					processedID = getString(mb2, "id")
					break
				}
			}
			break
		}
	}

	// Second: try exact name match
	if processedID == "" {
		for _, mb := range mbList {
			if getString(mb, "name") == processedName {
				processedID = getString(mb, "id")
				break
			}
		}
	}

	if processedID == "" {
		return nil, errToolError(fmt.Sprintf("Mailbox '%s' not found. Create it as a subfolder of '%s' in Fastmail.", processedName, bridgeName))
	}

	update := m{}
	for _, id := range ids {
		update[id] = m{
			"keywords/$seen": true,
			"mailboxIds":     m{processedID: true},
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
		return nil, errToolError(fmt.Sprintf("Failed to acknowledge all messages: %v", failures))
	}
	result := m{"status": "ok", "ids": ids, "movedTo": processedName}
	if len(failures) > 0 {
		result["failures"] = failures
	}
	return result, nil
}
