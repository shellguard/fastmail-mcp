package main

import "fmt"

// ── Contacts Tools ──────────────────────────────────────────────────────────

var contactsCaps = []string{
	"urn:ietf:params:jmap:core",
	"https://www.fastmail.com/dev/contacts",
}

func listContacts(params m) (any, error) {
	limit := intParam(params, "limit", 50, 200)
	search := getString(params, "search")

	acct, err := contactsAccountID()
	if err != nil {
		return nil, err
	}

	queryArgs := m{"accountId": acct, "limit": limit}
	if search != "" {
		queryArgs["filter"] = m{"text": search}
	}

	responses, err := jmapCall([]any{
		[]any{"ContactCard/query", queryArgs, "q0"},
		[]any{"ContactCard/get", m{
			"accountId":  acct,
			"#ids":       m{"resultOf": "q0", "name": "ContactCard/query", "path": "/ids"},
			"properties": []string{"id", "name", "emails", "phones", "online"},
		}, "g0"},
	}, contactsCaps)
	if err != nil {
		return nil, err
	}

	if len(responses) < 2 {
		return nil, errToolError("Unexpected contacts response")
	}
	contacts, ok := respList(responses[1])
	if !ok {
		return nil, errToolError("Unexpected contacts response")
	}

	out := make([]m, len(contacts))
	for i, c := range contacts {
		out[i] = contactSummaryDict(c)
	}
	return out, nil
}

func getContact(params m) (any, error) {
	contactID := getString(params, "id")
	if contactID == "" {
		return nil, errInvalidParams("id is required")
	}

	acct, err := contactsAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"ContactCard/get", m{
			"accountId": acct,
			"ids":       []string{contactID},
		}, "g0"},
	}, contactsCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected ContactCard/get response")
	}

	if notFound := getStringSlice(data, "notFound"); contains(notFound, contactID) {
		return nil, errInvalidParams("Contact not found: " + contactID)
	}

	list := getMapSlice(data, "list")
	if len(list) == 0 {
		return nil, errToolError("Unexpected ContactCard/get response")
	}

	return contactDetailDict(list[0]), nil
}

// ── Contact CRUD Tools ──────────────────────────────────────────────────────

func createContact(params m) (any, error) {
	acct, err := contactsAccountID()
	if err != nil {
		return nil, err
	}

	contactObj := m{}

	// Build name
	if nameObj, ok := params["name"].(map[string]any); ok {
		contactObj["name"] = nameObj
	} else {
		// Accept simple firstName/lastName
		nameMap := m{}
		if v := getString(params, "firstName"); v != "" {
			nameMap["given"] = v
		}
		if v := getString(params, "lastName"); v != "" {
			nameMap["surname"] = v
		}
		if v := getString(params, "fullName"); v != "" {
			nameMap["full"] = v
		}
		if len(nameMap) > 0 {
			contactObj["name"] = nameMap
		}
	}

	// Emails
	if emails := getMapSlice(params, "emails"); len(emails) > 0 {
		emailsMap := m{}
		for i, e := range emails {
			emailsMap[fmt.Sprintf("e%d", i)] = e
		}
		contactObj["emails"] = emailsMap
	} else if emailStrs := getStringSlice(params, "emails"); len(emailStrs) > 0 {
		emailsMap := m{}
		for i, addr := range emailStrs {
			emailsMap[fmt.Sprintf("e%d", i)] = m{"address": addr}
		}
		contactObj["emails"] = emailsMap
	}

	// Phones
	if phones := getMapSlice(params, "phones"); len(phones) > 0 {
		phonesMap := m{}
		for i, p := range phones {
			phonesMap[fmt.Sprintf("p%d", i)] = p
		}
		contactObj["phones"] = phonesMap
	} else if phoneStrs := getStringSlice(params, "phones"); len(phoneStrs) > 0 {
		phonesMap := m{}
		for i, num := range phoneStrs {
			phonesMap[fmt.Sprintf("p%d", i)] = m{"number": num}
		}
		contactObj["phones"] = phonesMap
	}

	// Notes
	if notes := getString(params, "notes"); notes != "" {
		contactObj["notes"] = m{"n0": m{"note": notes}}
	}

	// Organizations
	if company := getString(params, "company"); company != "" {
		contactObj["organizations"] = m{"o0": m{"name": company}}
	}

	responses, err := jmapCall([]any{
		[]any{"ContactCard/set", m{
			"accountId": acct,
			"create":    m{"c0": contactObj},
		}, "c0"},
	}, contactsCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected ContactCard/set response")
	}
	if nc, ok := data["notCreated"].(map[string]any); ok {
		if errObj, ok := nc["c0"].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to create contact: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	result := m{"status": "created"}
	if created, ok := data["created"].(map[string]any); ok {
		if c, ok := created["c0"].(map[string]any); ok {
			result["id"] = getString(c, "id")
		}
	}
	return result, nil
}

func updateContact(params m) (any, error) {
	id := getString(params, "id")
	if id == "" {
		return nil, errInvalidParams("id is required")
	}

	acct, err := contactsAccountID()
	if err != nil {
		return nil, err
	}

	update := m{}
	if v, ok := params["name"]; ok {
		update["name"] = v
	}
	if v := getString(params, "firstName"); v != "" {
		update["name/given"] = v
	}
	if v := getString(params, "lastName"); v != "" {
		update["name/surname"] = v
	}
	if v := getString(params, "fullName"); v != "" {
		update["name/full"] = v
	}
	if v, ok := params["emails"]; ok {
		if strs := getStringSlice(params, "emails"); len(strs) > 0 {
			emailsMap := m{}
			for i, addr := range strs {
				emailsMap[fmt.Sprintf("e%d", i)] = m{"address": addr}
			}
			update["emails"] = emailsMap
		} else {
			update["emails"] = v
		}
	}
	if v, ok := params["phones"]; ok {
		if strs := getStringSlice(params, "phones"); len(strs) > 0 {
			phonesMap := m{}
			for i, num := range strs {
				phonesMap[fmt.Sprintf("p%d", i)] = m{"number": num}
			}
			update["phones"] = phonesMap
		} else {
			update["phones"] = v
		}
	}
	if notes := getString(params, "notes"); notes != "" {
		update["notes"] = m{"n0": m{"note": notes}}
	}
	if company := getString(params, "company"); company != "" {
		update["organizations"] = m{"o0": m{"name": company}}
	}

	if len(update) == 0 {
		return nil, errInvalidParams("at least one field to update is required")
	}

	responses, err := jmapCall([]any{
		[]any{"ContactCard/set", m{
			"accountId": acct,
			"update":    m{id: update},
		}, "u0"},
	}, contactsCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected ContactCard/set response")
	}
	if nu, ok := data["notUpdated"].(map[string]any); ok {
		if errObj, ok := nu[id].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to update contact: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	return m{"status": "ok", "id": id}, nil
}

func deleteContact(params m) (any, error) {
	id := getString(params, "id")
	if id == "" {
		return nil, errInvalidParams("id is required")
	}

	acct, err := contactsAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"ContactCard/set", m{
			"accountId": acct,
			"destroy":   []string{id},
		}, "d0"},
	}, contactsCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected ContactCard/set response")
	}
	if nd, ok := data["notDestroyed"].(map[string]any); ok {
		if errObj, ok := nd[id].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to delete contact: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	return m{"status": "deleted", "id": id}, nil
}

// ── Address Book Tools ──────────────────────────────────────────────────────

func listAddressBooks(_ m) (any, error) {
	acct, err := contactsAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"AddressBook/get", m{
			"accountId":  acct,
			"properties": []string{"id", "name", "isDefault", "isSubscribed"},
		}, "a0"},
	}, contactsCaps)
	if err != nil {
		return nil, err
	}

	list, ok := respList(responses[0])
	if !ok {
		return nil, errToolError("Unexpected AddressBook/get response")
	}

	out := make([]m, len(list))
	for i, ab := range list {
		out[i] = m{
			"id":           getString(ab, "id"),
			"name":         getString(ab, "name"),
			"isDefault":    getBool(ab, "isDefault"),
			"isSubscribed": getBool(ab, "isSubscribed"),
		}
	}
	return out, nil
}

// ── Address Book Management Tools ───────────────────────────────────────────

func createAddressBook(params m) (any, error) {
	name := getString(params, "name")
	if name == "" {
		return nil, errInvalidParams("name is required")
	}

	acct, err := contactsAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"AddressBook/set", m{
			"accountId": acct,
			"create":    m{"ab0": m{"name": name}},
		}, "c0"},
	}, contactsCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected AddressBook/set response")
	}
	if nc, ok := data["notCreated"].(map[string]any); ok {
		if errObj, ok := nc["ab0"].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to create address book: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	result := m{"status": "created", "name": name}
	if created, ok := data["created"].(map[string]any); ok {
		if ab, ok := created["ab0"].(map[string]any); ok {
			result["id"] = getString(ab, "id")
		}
	}
	return result, nil
}

func deleteAddressBook(params m) (any, error) {
	id := getString(params, "id")
	if id == "" {
		return nil, errInvalidParams("id is required")
	}

	acct, err := contactsAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"AddressBook/set", m{
			"accountId": acct,
			"destroy":   []string{id},
		}, "d0"},
	}, contactsCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected AddressBook/set response")
	}
	if nd, ok := data["notDestroyed"].(map[string]any); ok {
		if errObj, ok := nd[id].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to delete address book: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	return m{"status": "deleted", "id": id}, nil
}
