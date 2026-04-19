package main

import "fmt"

// ── Identity Tools ──────────────────────────────────────────────────────────

func listIdentities(_ m) (any, error) {
	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"Identity/get", m{"accountId": acct}, "i0"},
	}, []string{
		"urn:ietf:params:jmap:core",
		"urn:ietf:params:jmap:mail",
		"urn:ietf:params:jmap:submission",
	})
	if err != nil {
		return nil, err
	}

	list, ok := respList(responses[0])
	if !ok {
		return nil, errToolError("Unexpected Identity/get response")
	}

	out := make([]m, len(list))
	for i, id := range list {
		out[i] = m{
			"id":            getString(id, "id"),
			"name":          getString(id, "name"),
			"email":         getString(id, "email"),
			"replyTo":       id["replyTo"],
			"bcc":           id["bcc"],
			"htmlSignature": getString(id, "htmlSignature"),
		}
	}
	return out, nil
}

func updateIdentity(params m) (any, error) {
	id := getString(params, "id")
	if id == "" {
		return nil, errInvalidParams("id is required")
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	update := m{}
	for _, key := range []string{"name", "textSignature", "htmlSignature"} {
		if v, ok := params[key]; ok {
			update[key] = v
		}
	}
	if v, ok := params["replyTo"]; ok {
		update["replyTo"] = v
	}
	if v, ok := params["bcc"]; ok {
		update["bcc"] = v
	}

	if len(update) == 0 {
		return nil, errInvalidParams("at least one field to update is required")
	}

	responses, err := jmapCall([]any{
		[]any{"Identity/set", m{
			"accountId": acct,
			"update":    m{id: update},
		}, "u0"},
	}, submissionCapsGlobal)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected Identity/set response")
	}
	if nu, ok := data["notUpdated"].(map[string]any); ok {
		if errObj, ok := nu[id].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to update identity: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	return m{"status": "ok", "id": id}, nil
}

// ── Masked Email Tools ──────────────────────────────────────────────────────

func listMaskedEmails(params m) (any, error) {
	acct, err := maskedEmailAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"MaskedEmail/get", m{
			"accountId": acct,
			"ids":       nil, // null = get all
		}, "m0"},
	}, maskedEmailCaps)
	if err != nil {
		return nil, err
	}

	list, ok := respList(responses[0])
	if !ok {
		return nil, errToolError("Unexpected MaskedEmail/get response")
	}

	// Optional state filter
	filterState := getString(params, "state")
	out := make([]m, 0, len(list))
	for _, me := range list {
		if filterState != "" && getString(me, "state") != filterState {
			continue
		}
		out = append(out, m{
			"id":            getString(me, "id"),
			"email":         getString(me, "email"),
			"state":         getString(me, "state"),
			"forDomain":     getString(me, "forDomain"),
			"description":   getString(me, "description"),
			"createdAt":     getString(me, "createdAt"),
			"createdBy":     getString(me, "createdBy"),
			"lastMessageAt": getString(me, "lastMessageAt"),
		})
	}
	return out, nil
}

func createMaskedEmail(params m) (any, error) {
	acct, err := maskedEmailAccountID()
	if err != nil {
		return nil, err
	}

	createObj := m{"state": "enabled"}
	if domain := getString(params, "forDomain"); domain != "" {
		createObj["forDomain"] = domain
	}
	if desc := getString(params, "description"); desc != "" {
		createObj["description"] = desc
	}

	responses, err := jmapCall([]any{
		[]any{"MaskedEmail/set", m{
			"accountId": acct,
			"create":    m{"me0": createObj},
		}, "c0"},
	}, maskedEmailCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected MaskedEmail/set response")
	}
	if nc, ok := data["notCreated"].(map[string]any); ok {
		if errObj, ok := nc["me0"].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to create masked email: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	result := m{"status": "created"}
	if created, ok := data["created"].(map[string]any); ok {
		if me, ok := created["me0"].(map[string]any); ok {
			result["id"] = getString(me, "id")
			result["email"] = getString(me, "email")
		}
	}
	return result, nil
}

func updateMaskedEmail(params m) (any, error) {
	id := getString(params, "id")
	if id == "" {
		return nil, errInvalidParams("id is required")
	}

	acct, err := maskedEmailAccountID()
	if err != nil {
		return nil, err
	}

	update := m{}
	if v, ok := params["state"]; ok {
		update["state"] = v
	}
	if v, ok := params["description"]; ok {
		update["description"] = v
	}
	if v, ok := params["forDomain"]; ok {
		update["forDomain"] = v
	}

	if len(update) == 0 {
		return nil, errInvalidParams("at least one field to update is required (state, description, forDomain)")
	}

	responses, err := jmapCall([]any{
		[]any{"MaskedEmail/set", m{
			"accountId": acct,
			"update":    m{id: update},
		}, "u0"},
	}, maskedEmailCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected MaskedEmail/set response")
	}
	if nu, ok := data["notUpdated"].(map[string]any); ok {
		if errObj, ok := nu[id].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to update masked email: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	return m{"status": "ok", "id": id}, nil
}

// ── Vacation Response Tools ─────────────────────────────────────────────────

func getVacationResponse(_ m) (any, error) {
	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"VacationResponse/get", m{
			"accountId": acct,
			"ids":       []string{"singleton"},
		}, "v0"},
	}, vacationCaps)
	if err != nil {
		return nil, err
	}

	list, ok := respList(responses[0])
	if !ok || len(list) == 0 {
		return nil, errToolError("Unexpected VacationResponse/get response")
	}
	vr := list[0]
	return m{
		"isEnabled": getBool(vr, "isEnabled"),
		"fromDate":  vr["fromDate"],
		"toDate":    vr["toDate"],
		"subject":   getString(vr, "subject"),
		"textBody":  getString(vr, "textBody"),
		"htmlBody":  getString(vr, "htmlBody"),
	}, nil
}

func setVacationResponse(params m) (any, error) {
	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	update := m{}
	if v, ok := params["isEnabled"]; ok {
		update["isEnabled"] = v
	}
	if v, ok := params["subject"]; ok {
		update["subject"] = v
	}
	if v, ok := params["textBody"]; ok {
		update["textBody"] = v
	}
	if v, ok := params["htmlBody"]; ok {
		update["htmlBody"] = v
	}
	if v, ok := params["fromDate"]; ok {
		update["fromDate"] = v
	}
	if v, ok := params["toDate"]; ok {
		update["toDate"] = v
	}

	responses, err := jmapCall([]any{
		[]any{"VacationResponse/set", m{
			"accountId": acct,
			"update":    m{"singleton": update},
		}, "v0"},
	}, vacationCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected VacationResponse/set response")
	}
	if nu, ok := data["notUpdated"].(map[string]any); ok {
		if errObj, ok := nu["singleton"].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to set vacation response: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	return m{"status": "ok"}, nil
}

// ── Quota Tools ─────────────────────────────────────────────────────────────

func getQuota(_ m) (any, error) {
	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"Quota/get", m{
			"accountId": acct,
			"ids":       nil,
		}, "q0"},
	}, quotaCaps)
	if err != nil {
		return nil, err
	}

	list, ok := respList(responses[0])
	if !ok {
		return nil, errToolError("Unexpected Quota/get response")
	}

	out := make([]m, len(list))
	for i, q := range list {
		out[i] = m{
			"id":           getString(q, "id"),
			"resourceType": getString(q, "resourceType"),
			"used":         getFloat(q, "used"),
			"hardLimit":    getFloat(q, "hardLimit"),
			"scope":        getString(q, "scope"),
			"name":         getString(q, "name"),
			"description":  getString(q, "description"),
		}
	}
	return out, nil
}

// ── Attachment/Blob Tools ───────────────────────────────────────────────────

func downloadAttachment(params m) (any, error) {
	blobID := getString(params, "blobId")
	if blobID == "" {
		return nil, errInvalidParams("blobId is required")
	}
	name := getString(params, "name")
	if name == "" {
		name = "attachment"
	}
	mimeType := getString(params, "type")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	// Ensure session is loaded (populates cachedDownloadURL)
	_, _, err = sessionFor(defaultCaps)
	if err != nil {
		return nil, err
	}

	url := blobDownloadURL(acct, blobID, name, mimeType)
	if url == "" {
		return nil, errToolError("Download URL not available — session may not have been established")
	}

	return m{
		"downloadUrl": url,
		"blobId":      blobID,
		"name":        name,
		"type":        mimeType,
	}, nil
}

// ── Email Delivery Tracking Tools ───────────────────────────────────────────

func getEmailSubmission(params m) (any, error) {
	id := getString(params, "id")

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	var calls []any
	if id != "" {
		calls = []any{
			[]any{"EmailSubmission/get", m{
				"accountId": acct,
				"ids":       []string{id},
			}, "s0"},
		}
	} else {
		// List recent submissions
		limit := intParam(params, "limit", 20, 100)
		calls = []any{
			[]any{"EmailSubmission/query", m{
				"accountId": acct,
				"sort":      []m{{"property": "sentAt", "isAscending": false}},
				"limit":     limit,
			}, "q0"},
			[]any{"EmailSubmission/get", m{
				"accountId": acct,
				"#ids":      m{"resultOf": "q0", "name": "EmailSubmission/query", "path": "/ids"},
			}, "g0"},
		}
	}

	responses, err := jmapCall(calls, submissionCapsGlobal)
	if err != nil {
		return nil, err
	}

	// Get the last response (the /get response)
	getResp := responses[len(responses)-1]
	list, ok := respList(getResp)
	if !ok {
		return nil, errToolError("Unexpected EmailSubmission response")
	}

	out := make([]m, len(list))
	for i, sub := range list {
		entry := m{
			"id":         getString(sub, "id"),
			"emailId":    getString(sub, "emailId"),
			"threadId":   getString(sub, "threadId"),
			"identityId": getString(sub, "identityId"),
			"sendAt":     sub["sendAt"],
			"undoStatus": getString(sub, "undoStatus"),
		}
		if ds := getMap(sub, "deliveryStatus"); ds != nil {
			entry["deliveryStatus"] = ds
		}
		if dsn := getSlice(sub, "dsnBlobIds"); len(dsn) > 0 {
			entry["dsnBlobIds"] = dsn
		}
		out[i] = entry
	}

	if id != "" && len(out) == 1 {
		return out[0], nil
	}
	return m{"submissions": out}, nil
}

// ── MDN (Read Receipt) Tools ────────────────────────────────────────────────

var mdnCaps = []string{
	"urn:ietf:params:jmap:core",
	"urn:ietf:params:jmap:mail",
	"urn:ietf:params:jmap:mdn",
}

func sendMDN(params m) (any, error) {
	emailID := getString(params, "forEmailId")
	if emailID == "" {
		return nil, errInvalidParams("forEmailId is required (the email to acknowledge)")
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	// Verify the email actually requested a read receipt
	checkResponses, err := jmapCall([]any{
		[]any{"Email/get", m{
			"accountId":  acct,
			"ids":        []string{emailID},
			"properties": []string{"header:Disposition-Notification-To:asAddresses"},
		}, "chk0"},
	}, nil)
	if err != nil {
		return nil, err
	}
	chkList := getMapSlice(must(respData(checkResponses[0])), "list")
	if len(chkList) == 0 {
		return nil, errInvalidParams("Email not found: " + emailID)
	}
	// Check if Disposition-Notification-To header exists
	dntHeader := chkList[0]["header:Disposition-Notification-To:asAddresses"]
	if dntHeader == nil {
		return nil, errInvalidParams("Email does not request a read receipt (no Disposition-Notification-To header)")
	}
	if arr, ok := dntHeader.([]any); ok && len(arr) == 0 {
		return nil, errInvalidParams("Email does not request a read receipt (empty Disposition-Notification-To header)")
	}

	mdnObj := m{
		"forEmailId":             emailID,
		"disposition":            m{"actionMode": "manual-action", "sendingMode": "mdn-sent-manually", "type": "displayed"},
		"includeOriginalMessage": false,
	}
	if subject := getString(params, "subject"); subject != "" {
		mdnObj["subject"] = subject
	}
	if textBody := getString(params, "textBody"); textBody != "" {
		mdnObj["textBody"] = textBody
	}

	// Get the identity for the reportingUA
	idResponses, err := jmapCall([]any{
		[]any{"Identity/get", m{"accountId": acct, "properties": []string{"id", "email"}}, "i0"},
	}, submissionCapsGlobal)
	if err == nil && len(idResponses) > 0 {
		if idList, ok := respList(idResponses[0]); ok && len(idList) > 0 {
			mdnObj["identityId"] = getString(idList[0], "id")
		}
	}

	responses, err := jmapCall([]any{
		[]any{"MDN/send", m{
			"accountId": acct,
			"send":      m{"mdn0": mdnObj},
		}, "m0"},
	}, mdnCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected MDN/send response")
	}

	if ns, ok := data["notSent"].(map[string]any); ok {
		if errObj, ok := ns["mdn0"].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to send read receipt: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	return m{"status": "sent", "forEmailId": emailID}, nil
}

func parseMDN(params m) (any, error) {
	blobID := getString(params, "blobId")
	if blobID == "" {
		return nil, errInvalidParams("blobId is required")
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"MDN/parse", m{
			"accountId": acct,
			"blobIds":   []string{blobID},
		}, "p0"},
	}, mdnCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected MDN/parse response")
	}

	if notFound := getStringSlice(data, "notFound"); contains(notFound, blobID) {
		return nil, errInvalidParams("Blob not found: " + blobID)
	}
	if notParsable := getStringSlice(data, "notParsable"); contains(notParsable, blobID) {
		return nil, errToolError("Blob is not a valid MDN: " + blobID)
	}

	parsed, ok := data["parsed"].(map[string]any)
	if !ok {
		return nil, errToolError("Unexpected MDN/parse response")
	}

	mdn, ok := parsed[blobID].(map[string]any)
	if !ok {
		return nil, errToolError("No parsed result for blob")
	}

	return mdn, nil
}
