package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── MCP Protocol Types ──────────────────────────────────────────────────────

type toolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

type mcpError struct {
	Message string
	Code    int // JSON-RPC error code
}

func (e *mcpError) Error() string { return e.Message }

func errInvalidRequest(msg string) *mcpError { return &mcpError{msg, -32600} }
func errMethodNotFound(msg string) *mcpError { return &mcpError{msg, -32601} }
func errInvalidParams(msg string) *mcpError  { return &mcpError{msg, -32602} }
func errToolNotFound(name string) *mcpError   { return &mcpError{"Unknown tool: " + name, -32601} }
func errToolError(msg string) *mcpError       { return &mcpError{msg, -32000} }
func errAuthError(msg string) *mcpError       { return &mcpError{msg, -32000} }

// ── JMAP Session & HTTP Helpers ─────────────────────────────────────────────

var (
	sessionMu              sync.Mutex
	cachedSessionAPIURL    string
	cachedPrimaryAccounts  = map[string]string{} // capability → accountId
	cachedFallbackAccount  string
	httpClient             = &http.Client{Timeout: 90 * time.Second}
)

func bearerToken() (string, error) {
	token := os.Getenv("FASTMAIL_TOKEN")
	if token == "" {
		return "", errAuthError("FASTMAIL_TOKEN environment variable is not set")
	}
	return token, nil
}

// sessionFor discovers the JMAP session (cached) and returns apiUrl + accountId
// for the most specific requested capability.
func sessionFor(capabilities []string) (apiURL, accountID string, err error) {
	sessionMu.Lock()
	defer sessionMu.Unlock()

	if cachedSessionAPIURL == "" {
		token, err := bearerToken()
		if err != nil {
			return "", "", err
		}

		req, _ := http.NewRequest("GET", "https://api.fastmail.com/jmap/session", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := httpClient.Do(req)
		if err != nil {
			return "", "", errToolError("Session discovery failed: " + err.Error())
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)

		if resp.StatusCode == 401 {
			return "", "", errAuthError("Invalid FASTMAIL_TOKEN (401 Unauthorized)")
		}
		if resp.StatusCode != 200 {
			return "", "", errToolError(fmt.Sprintf("Session discovery failed: HTTP %d", resp.StatusCode))
		}

		var session map[string]any
		if err := json.Unmarshal(body, &session); err != nil {
			return "", "", errToolError("Session discovery: unexpected response format")
		}

		url, _ := session["apiUrl"].(string)
		accounts, _ := session["accounts"].(map[string]any)
		primary, _ := session["primaryAccounts"].(map[string]any)

		if url == "" || accounts == nil || primary == nil {
			return "", "", errToolError("Session discovery: unexpected response format")
		}

		cachedSessionAPIURL = url
		for cap, v := range primary {
			if id, ok := v.(string); ok {
				cachedPrimaryAccounts[cap] = id
			}
		}
		for id := range accounts {
			cachedFallbackAccount = id
			break
		}
	}

	// Resolve accountId for requested capabilities (prefer non-core)
	for _, cap := range capabilities {
		if cap == "urn:ietf:params:jmap:core" {
			continue
		}
		if id, ok := cachedPrimaryAccounts[cap]; ok {
			return cachedSessionAPIURL, id, nil
		}
	}
	if id, ok := cachedPrimaryAccounts["urn:ietf:params:jmap:core"]; ok {
		return cachedSessionAPIURL, id, nil
	}
	if cachedFallbackAccount != "" {
		return cachedSessionAPIURL, cachedFallbackAccount, nil
	}
	return "", "", errToolError("Session discovery: no accounts found")
}

var defaultCaps = []string{
	"urn:ietf:params:jmap:core",
	"urn:ietf:params:jmap:mail",
}

// jmapCall makes a JMAP API call and returns the methodResponses array.
func jmapCall(methodCalls []any, caps []string) ([]any, error) {
	if caps == nil {
		caps = defaultCaps
	}
	token, err := bearerToken()
	if err != nil {
		return nil, err
	}
	apiURL, _, err := sessionFor(caps)
	if err != nil {
		return nil, err
	}

	reqBody := map[string]any{
		"using":       caps,
		"methodCalls": methodCalls,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, errToolError("Failed to serialize JMAP request")
	}

	req, _ := http.NewRequest("POST", apiURL, bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	data, statusCode, err := doHTTPWithRetry(req, 2)
	if err != nil {
		return nil, err
	}
	if statusCode != 200 {
		snippet := string(data)
		if len(snippet) > 500 {
			snippet = snippet[:500]
		}
		return nil, errToolError(fmt.Sprintf("JMAP call failed: HTTP %d — %s", statusCode, snippet))
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, errToolError("JMAP call: unexpected response format")
	}

	responses, ok := result["methodResponses"].([]any)
	if !ok {
		return nil, errToolError("JMAP call: unexpected response format")
	}

	// Check for JMAP-level error responses
	for _, r := range responses {
		resp, ok := r.([]any)
		if !ok || len(resp) < 2 {
			continue
		}
		if name, _ := resp[0].(string); name == "error" {
			if errObj, ok := resp[1].(map[string]any); ok {
				errType, _ := errObj["type"].(string)
				errDesc, _ := errObj["description"].(string)
				if errType == "" {
					errType = "unknown"
				}
				return nil, errToolError(fmt.Sprintf("JMAP error (%s): %s", errType, errDesc))
			}
		}
	}

	return responses, nil
}

func doHTTPWithRetry(req *http.Request, maxRetries int) ([]byte, int, error) {
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if bodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, 0, errToolError("HTTP request failed: " + err.Error())
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == 429 && attempt < maxRetries {
			wait := 2
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if v, err := strconv.Atoi(ra); err == nil {
					wait = v
				}
			}
			if wait > 30 {
				wait = 30
			}
			fmt.Fprintf(os.Stderr, "fastmail-mcp: rate limited, retrying after %ds\n", wait)
			time.Sleep(time.Duration(wait) * time.Second)
			continue
		}
		return data, resp.StatusCode, nil
	}
	return nil, 0, errToolError("HTTP request failed after retries")
}

// ── JMAP Helpers ────────────────────────────────────────────────────────────

func mailAccountID() (string, error) {
	_, id, err := sessionFor(defaultCaps)
	return id, err
}

func contactsAccountID() (string, error) {
	_, id, err := sessionFor([]string{
		"urn:ietf:params:jmap:core",
		"https://www.fastmail.com/dev/contacts",
	})
	return id, err
}

// ── JSON helpers ────────────────────────────────────────────────────────────

// m is shorthand for map[string]any — keeps JMAP call construction readable.
type m = map[string]any

func getString(obj m, key string) string {
	v, _ := obj[key].(string)
	return v
}

func getFloat(obj m, key string) float64 {
	v, _ := obj[key].(float64)
	return v
}

func getBool(obj m, key string) bool {
	v, _ := obj[key].(bool)
	return v
}

func getMap(obj m, key string) m {
	v, _ := obj[key].(map[string]any)
	return v
}

func getSlice(obj m, key string) []any {
	v, _ := obj[key].([]any)
	return v
}

func getMapSlice(obj m, key string) []m {
	raw := getSlice(obj, key)
	out := make([]m, 0, len(raw))
	for _, item := range raw {
		if v, ok := item.(map[string]any); ok {
			out = append(out, v)
		}
	}
	return out
}

func getStringSlice(obj m, key string) []string {
	raw := getSlice(obj, key)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if v, ok := item.(string); ok {
			out = append(out, v)
		}
	}
	return out
}

// respData extracts the data map from a JMAP response entry: ["MethodName", {data}, "tag"]
func respData(resp any) (m, bool) {
	arr, ok := resp.([]any)
	if !ok || len(arr) < 2 {
		return nil, false
	}
	data, ok := arr[1].(map[string]any)
	return data, ok
}

// respList extracts the "list" array from a JMAP response data map.
func respList(resp any) ([]m, bool) {
	data, ok := respData(resp)
	if !ok {
		return nil, false
	}
	raw, ok := data["list"].([]any)
	if !ok {
		return nil, false
	}
	out := make([]m, 0, len(raw))
	for _, item := range raw {
		if v, ok := item.(map[string]any); ok {
			out = append(out, v)
		}
	}
	return out, true
}

// ── Tool Implementations ────────────────────────────────────────────────────

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

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	// Try to parse query as JSON filter, fall back to text filter
	var filter m
	if err := json.Unmarshal([]byte(query), &filter); err != nil {
		filter = m{"text": query}
	}

	if mailboxID != "" {
		filter["inMailbox"] = mailboxID
	}

	responses, err := jmapCall([]any{
		[]any{"Email/query", m{
			"accountId":      acct,
			"filter":         filter,
			"sort":           []m{{"property": "receivedAt", "isAscending": false}},
			"position":       0,
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
		return nil, errToolError("Unexpected search response")
	}

	emails, ok := respList(responses[1])
	if !ok {
		return nil, errToolError("Unexpected search response")
	}

	mapped := make([]m, len(emails))
	for i, e := range emails {
		mapped[i] = emailSummaryDict(e)
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
	ids := getStringSlice(params, "ids")
	if len(ids) == 0 {
		return nil, errInvalidParams("ids is required (array of email IDs)")
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
	ids := getStringSlice(params, "ids")
	if len(ids) == 0 {
		return nil, errInvalidParams("ids is required (array of email IDs)")
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
	ids := getStringSlice(params, "ids")
	if len(ids) == 0 {
		return nil, errInvalidParams("ids is required (array of email IDs)")
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

// ── JMAP Response Helpers ───────────────────────────────────────────────────

func checkNotUpdated(responses []any) map[string]string {
	failures := map[string]string{}
	for _, r := range responses {
		arr, ok := r.([]any)
		if !ok || len(arr) < 2 {
			continue
		}
		name, _ := arr[0].(string)
		if name != "Email/set" {
			continue
		}
		data, ok := arr[1].(map[string]any)
		if !ok {
			continue
		}
		notUpdated, ok := data["notUpdated"].(map[string]any)
		if !ok {
			continue
		}
		for id, errObj := range notUpdated {
			if e, ok := errObj.(map[string]any); ok {
				et, _ := e["type"].(string)
				ed, _ := e["description"].(string)
				if et == "" {
					et = "unknown"
				}
				failures[id] = et + ": " + ed
			} else {
				failures[id] = "unknown error"
			}
		}
	}
	return failures
}

// ── Serialization Helpers ───────────────────────────────────────────────────

func emailSummaryDict(email m) m {
	keywords := getMap(email, "keywords")
	_, isRead := keywords["$seen"]
	_, isFlagged := keywords["$flagged"]

	return m{
		"id":         getString(email, "id"),
		"threadId":   getString(email, "threadId"),
		"subject":    getString(email, "subject"),
		"from":       formatAddresses(email["from"]),
		"to":         formatAddresses(email["to"]),
		"receivedAt": getString(email, "receivedAt"),
		"preview":    getString(email, "preview"),
		"isRead":     isRead,
		"isFlagged":  isFlagged,
		"size":       getFloat(email, "size"),
	}
}

func emailDetailDict(email m) m {
	keywords := getMap(email, "keywords")
	_, isRead := keywords["$seen"]
	_, isFlagged := keywords["$flagged"]

	d := m{
		"id":         getString(email, "id"),
		"threadId":   getString(email, "threadId"),
		"subject":    getString(email, "subject"),
		"from":       formatAddresses(email["from"]),
		"to":         formatAddresses(email["to"]),
		"cc":         formatAddresses(email["cc"]),
		"receivedAt": getString(email, "receivedAt"),
		"isRead":     isRead,
		"isFlagged":  isFlagged,
	}

	if sentAt := getString(email, "sentAt"); sentAt != "" {
		d["sentAt"] = sentAt
	}
	if email["replyTo"] != nil {
		d["replyTo"] = formatAddresses(email["replyTo"])
	}
	if msgID := getStringSlice(email, "messageId"); len(msgID) > 0 {
		d["messageId"] = msgID
	}
	if irt := getStringSlice(email, "inReplyTo"); len(irt) > 0 {
		d["inReplyTo"] = irt
	}

	d["body"] = extractBodyText(email)
	d["htmlBody"] = extractHTMLBody(email)

	if attachments := getMapSlice(email, "attachments"); len(attachments) > 0 {
		names := []string{}
		for _, att := range attachments {
			if n := getString(att, "name"); n != "" {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			d["attachmentNames"] = names
		}
	}

	return d
}

func contactSummaryDict(contact m) m {
	d := m{"id": getString(contact, "id")}

	if nameObj := getMap(contact, "name"); nameObj != nil {
		if full := getString(nameObj, "full"); full != "" {
			d["name"] = full
		} else {
			given := getString(nameObj, "given")
			surname := getString(nameObj, "surname")
			d["name"] = strings.TrimSpace(given + " " + surname)
		}
	}

	if emailsMap := getMap(contact, "emails"); emailsMap != nil {
		addrs := []string{}
		for _, entry := range emailsMap {
			if e, ok := entry.(map[string]any); ok {
				if addr, _ := e["address"].(string); addr != "" {
					addrs = append(addrs, addr)
				}
			}
		}
		if len(addrs) > 0 {
			d["emails"] = addrs
		}
	}

	return d
}

func contactDetailDict(contact m) m {
	d := contactSummaryDict(contact)

	if phonesMap := getMap(contact, "phones"); phonesMap != nil {
		phones := []m{}
		for _, entry := range phonesMap {
			if e, ok := entry.(map[string]any); ok {
				phones = append(phones, m{
					"number": getString(e, "number"),
					"label":  getString(e, "label"),
				})
			}
		}
		if len(phones) > 0 {
			d["phones"] = phones
		}
	}

	if onlineMap := getMap(contact, "online"); onlineMap != nil {
		online := []m{}
		for _, entry := range onlineMap {
			if e, ok := entry.(map[string]any); ok {
				online = append(online, m{
					"resource": getString(e, "resource"),
					"type":     getString(e, "type"),
					"label":    getString(e, "label"),
				})
			}
		}
		if len(online) > 0 {
			d["online"] = online
		}
	}

	return d
}

func formatAddresses(obj any) any {
	arr, ok := obj.([]any)
	if !ok {
		return []any{}
	}
	out := make([]m, 0, len(arr))
	for _, item := range arr {
		if addr, ok := item.(map[string]any); ok {
			out = append(out, m{
				"name":  getString(addr, "name"),
				"email": getString(addr, "email"),
			})
		}
	}
	return out
}

func extractBodyText(email m) string {
	bodyValues := getMap(email, "bodyValues")
	if textParts := getMapSlice(email, "textBody"); len(textParts) > 0 {
		for _, part := range textParts {
			if partID := getString(part, "partId"); partID != "" {
				if value := getMap(bodyValues, partID); value != nil {
					if text := getString(value, "value"); text != "" {
						return text
					}
				}
			}
		}
	}
	return getString(email, "preview")
}

func extractHTMLBody(email m) string {
	bodyValues := getMap(email, "bodyValues")
	if htmlParts := getMapSlice(email, "htmlBody"); len(htmlParts) > 0 {
		for _, part := range htmlParts {
			if partID := getString(part, "partId"); partID != "" {
				if value := getMap(bodyValues, partID); value != nil {
					if text := getString(value, "value"); text != "" {
						return text
					}
				}
			}
		}
	}
	return ""
}

type bridgeMatch struct {
	typ         string
	description string
}

var bridgeSubjectRe = regexp.MustCompile(`^\[(\w+)\]\s+(.+)$`)

func parseBridgeSubject(subject string) *bridgeMatch {
	m := bridgeSubjectRe.FindStringSubmatch(subject)
	if m == nil {
		return nil
	}
	typ := strings.ToUpper(m[1])
	switch typ {
	case "TASK", "NOTE", "EVENT":
		return &bridgeMatch{typ: typ, description: m[2]}
	}
	return nil
}

// ── Utility ─────────────────────────────────────────────────────────────────

func intParam(params m, key string, defaultVal, maxVal int) int {
	v := defaultVal
	if f, ok := params[key].(float64); ok {
		v = int(f)
	}
	if maxVal > 0 && v > maxVal {
		v = maxVal
	}
	return v
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

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
				"query":     m{"type": "string", "description": "Search query (text or JSON JMAP filter)"},
				"mailboxId": m{"type": "string", "description": "Optional: limit search to this mailbox"},
				"limit":     m{"type": "integer", "description": "Max results (default 20, max 200)"},
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
}

// ── Tool Dispatch ───────────────────────────────────────────────────────────

type toolFunc func(m) (any, error)

var toolHandlers = map[string]toolFunc{
	"fm_list_mailboxes":       listMailboxes,
	"fm_list_emails":          listEmails,
	"fm_get_email":            getEmail,
	"fm_search_emails":        searchEmails,
	"fm_send_email":           sendEmail,
	"fm_mark_read":            markRead,
	"fm_move_email":           moveEmail,
	"fm_delete_email":         deleteEmail,
	"fm_list_bridge_messages": listBridgeMessages,
	"fm_ack_bridge_message":   ackBridgeMessage,
	"fm_list_contacts":        listContacts,
	"fm_get_contact":          getContact,
	"fm_list_identities":      listIdentities,
}

func callTool(name string, arguments m) (any, error) {
	handler, ok := toolHandlers[name]
	if !ok {
		return nil, errToolNotFound(name)
	}
	return handler(arguments)
}

// ── MCP Server ──────────────────────────────────────────────────────────────

const maxInputBytes = 10 * 1024 * 1024

func run() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, maxInputBytes), maxInputBytes)

	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var msg m
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}

		handleMessage(msg, enc)
	}
}

func handleMessage(msg m, enc *json.Encoder) {
	id := msg["id"]
	method := getString(msg, "method")

	send := func(result m) {
		if id == nil {
			return
		}
		enc.Encode(m{"jsonrpc": "2.0", "id": id, "result": result})
	}

	sendErr := func(code int, message string) {
		if id == nil {
			return // JSON-RPC: no response for notifications
		}
		enc.Encode(m{"jsonrpc": "2.0", "id": id, "error": m{"code": code, "message": message}})
	}

	switch method {
	case "initialize":
		send(m{
			"protocolVersion": "2024-11-05",
			"capabilities":    m{"tools": m{"listChanged": false}},
			"serverInfo":      m{"name": "fastmail-mcp", "version": "1.0.0"},
		})

	case "notifications/initialized":
		// no response

	case "tools/list":
		toolList := make([]m, len(tools))
		for i, t := range tools {
			toolList[i] = m{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema}
		}
		send(m{"tools": toolList})

	case "tools/call":
		params := getMap(msg, "params")
		toolName := getString(params, "name")
		if toolName == "" {
			sendErr(-32602, "Tool name missing")
			return
		}
		arguments := getMap(params, "arguments")
		if arguments == nil {
			arguments = m{}
		}

		result, err := callTool(toolName, arguments)
		if err != nil {
			if me, ok := err.(*mcpError); ok {
				sendErr(me.Code, me.Message)
			} else {
				sendErr(-32000, err.Error())
			}
			return
		}

		jsonBytes, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			send(m{
				"content": []m{{"type": "text", "text": fmt.Sprintf("%v", result)}},
				"isError": false,
			})
			return
		}
		send(m{
			"content": []m{{"type": "text", "text": string(jsonBytes)}},
			"isError": false,
		})

	case "ping":
		send(m{})

	default:
		sendErr(-32601, "Method not found: "+method)
	}
}

// ── Entry Point ─────────────────────────────────────────────────────────────

func main() {
	run()
}
