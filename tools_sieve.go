package main

import "fmt"

// ── Sieve Filter Tools ──────────────────────────────────────────────────────

func listSieveScripts(_ m) (any, error) {
	acct, caps, err := sieveAccountAndCaps()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"SieveScript/get", m{
			"accountId": acct,
			"ids":       nil, // null = get all
		}, "s0"},
	}, caps)
	if err != nil {
		return nil, err
	}

	list, ok := respList(responses[0])
	if !ok {
		return nil, errToolError("Unexpected SieveScript/get response")
	}

	out := make([]m, len(list))
	for i, s := range list {
		out[i] = m{
			"id":       getString(s, "id"),
			"name":     s["name"], // can be null
			"isActive": getBool(s, "isActive"),
			"blobId":   getString(s, "blobId"),
		}
	}
	return out, nil
}

func getSieveScript(params m) (any, error) {
	id := getString(params, "id")
	if id == "" {
		return nil, errInvalidParams("id is required")
	}

	acct, caps, err := sieveAccountAndCaps()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"SieveScript/get", m{
			"accountId": acct,
			"ids":       []string{id},
		}, "s0"},
	}, caps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected SieveScript/get response")
	}
	if notFound := getStringSlice(data, "notFound"); contains(notFound, id) {
		return nil, errInvalidParams("Sieve script not found: " + id)
	}
	list := getMapSlice(data, "list")
	if len(list) == 0 {
		return nil, errToolError("Unexpected SieveScript/get response")
	}

	script := list[0]
	blobID := getString(script, "blobId")

	// Download the script content
	content, err := downloadBlobText(acct, blobID)
	if err != nil {
		return nil, err
	}

	return m{
		"id":       getString(script, "id"),
		"name":     script["name"],
		"isActive": getBool(script, "isActive"),
		"content":  content,
	}, nil
}

func setSieveScript(params m) (any, error) {
	content := getString(params, "content")
	if content == "" {
		return nil, errInvalidParams("content is required (Sieve script source)")
	}

	acct, caps, err := sieveAccountAndCaps()
	if err != nil {
		return nil, err
	}

	// Upload script content as a blob
	blobID, err := uploadBlob(acct, content, "application/sieve")
	if err != nil {
		return nil, err
	}

	id := getString(params, "id")
	activate := getBool(params, "activate")

	var setArgs m

	if id != "" {
		// Update existing script
		update := m{"blobId": blobID}
		if v, ok := params["name"]; ok {
			update["name"] = v
		}
		setArgs = m{
			"accountId": acct,
			"update":    m{id: update},
		}
		if activate {
			setArgs["onSuccessActivateScript"] = id
		}
	} else {
		// Create new script
		createObj := m{"blobId": blobID}
		if name := getString(params, "name"); name != "" {
			createObj["name"] = name
		}
		setArgs = m{
			"accountId": acct,
			"create":    m{"sv0": createObj},
		}
		if activate {
			setArgs["onSuccessActivateScript"] = "#sv0"
		}
	}

	responses, err := jmapCall([]any{
		[]any{"SieveScript/set", setArgs, "c0"},
	}, caps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected SieveScript/set response")
	}

	// Check for errors
	if id != "" {
		if nu, ok := data["notUpdated"].(map[string]any); ok {
			if errObj, ok := nu[id].(map[string]any); ok {
				return nil, errToolError(fmt.Sprintf("Failed to update script: %s — %s",
					getString(errObj, "type"), getString(errObj, "description")))
			}
		}
		return m{"status": "updated", "id": id, "activated": activate}, nil
	}

	if nc, ok := data["notCreated"].(map[string]any); ok {
		if errObj, ok := nc["sv0"].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to create script: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	result := m{"status": "created", "activated": activate}
	if created, ok := data["created"].(map[string]any); ok {
		if s, ok := created["sv0"].(map[string]any); ok {
			result["id"] = getString(s, "id")
		}
	}
	return result, nil
}

func deleteSieveScript(params m) (any, error) {
	id := getString(params, "id")
	if id == "" {
		return nil, errInvalidParams("id is required")
	}

	acct, caps, err := sieveAccountAndCaps()
	if err != nil {
		return nil, err
	}

	// Check if this script is active before destroying
	getResponses, err := jmapCall([]any{
		[]any{"SieveScript/get", m{
			"accountId": acct,
			"ids":       []string{id},
		}, "g0"},
	}, caps)
	if err != nil {
		return nil, err
	}
	scriptList := getMapSlice(must(respData(getResponses[0])), "list")
	if len(scriptList) == 0 {
		return nil, errInvalidParams("Sieve script not found: " + id)
	}

	// Only deactivate if THIS script is the active one
	destroyArgs := m{
		"accountId": acct,
		"destroy":   []string{id},
	}
	if getBool(scriptList[0], "isActive") {
		destroyArgs["onSuccessDeactivateScript"] = true
	}

	responses, err := jmapCall([]any{
		[]any{"SieveScript/set", destroyArgs, "d0"},
	}, caps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected SieveScript/set response")
	}
	if nd, ok := data["notDestroyed"].(map[string]any); ok {
		if errObj, ok := nd[id].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to delete script: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	return m{"status": "deleted", "id": id}, nil
}

func activateSieveScript(params m) (any, error) {
	acct, caps, err := sieveAccountAndCaps()
	if err != nil {
		return nil, err
	}

	id := getString(params, "id")
	setArgs := m{"accountId": acct}

	if id == "" {
		// Deactivate all
		setArgs["onSuccessDeactivateScript"] = true
	} else {
		// Activate specific script
		setArgs["onSuccessActivateScript"] = id
	}

	responses, err := jmapCall([]any{
		[]any{"SieveScript/set", setArgs, "a0"},
	}, caps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected SieveScript/set response")
	}
	// Check for accountId-level error
	if errType := getString(data, "type"); errType != "" {
		return nil, errToolError(fmt.Sprintf("Failed to activate: %s — %s",
			errType, getString(data, "description")))
	}

	if id == "" {
		return m{"status": "deactivated"}, nil
	}
	return m{"status": "activated", "id": id}, nil
}

func validateSieveScript(params m) (any, error) {
	content := getString(params, "content")
	if content == "" {
		return nil, errInvalidParams("content is required (Sieve script source)")
	}

	acct, caps, err := sieveAccountAndCaps()
	if err != nil {
		return nil, err
	}

	// Upload as blob first
	blobID, err := uploadBlob(acct, content, "application/sieve")
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"SieveScript/validate", m{
			"accountId": acct,
			"blobId":    blobID,
		}, "v0"},
	}, caps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected SieveScript/validate response")
	}

	if errObj := data["error"]; errObj != nil {
		if e, ok := errObj.(map[string]any); ok {
			return m{
				"valid":       false,
				"error":       getString(e, "type"),
				"description": getString(e, "description"),
			}, nil
		}
		return m{"valid": false, "error": fmt.Sprintf("%v", errObj)}, nil
	}

	return m{"valid": true}, nil
}

// fm_get_sieve_capabilities: return the server's supported Sieve extensions.
func getSieveCapabilities(_ m) (any, error) {
	// Ensure session is loaded
	_, _, err := sessionFor(defaultCaps)
	if err != nil {
		return nil, err
	}

	sessionMu.Lock()
	defer sessionMu.Unlock()

	// Check all candidate URNs
	for _, cap := range sieveCapCandidates {
		if cachedCapabilities[cap] {
			result := m{"supported": true, "capability": cap}
			if data := cachedCapabilityData[cap]; data != nil {
				if exts := getStringSlice(data, "sieveExtensions"); len(exts) > 0 {
					result["extensions"] = exts
				}
				if impl := getString(data, "implementation"); impl != "" {
					result["implementation"] = impl
				}
				if maxSize, ok := data["maxSizeScript"].(float64); ok {
					result["maxSizeScript"] = int(maxSize)
				}
				if maxScripts, ok := data["maxNumberScripts"].(float64); ok {
					result["maxNumberScripts"] = int(maxScripts)
				}
				if maxName, ok := data["maxSizeScriptName"].(float64); ok {
					result["maxSizeScriptName"] = int(maxName)
				}
				if notif := getStringSlice(data, "notificationMethods"); len(notif) > 0 {
					result["notificationMethods"] = notif
				}
			}
			return result, nil
		}
	}

	// List all available capabilities so the user can see what's there
	available := []string{}
	for cap := range cachedCapabilities {
		available = append(available, cap)
	}

	return m{
		"supported":             false,
		"message":               "No Sieve capability found. Checked: urn:ietf:params:jmap:sieve, https://www.fastmail.com/dev/sieve",
		"availableCapabilities": available,
	}, nil
}
