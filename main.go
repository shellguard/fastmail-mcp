package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	cachedDownloadURL      string // template with {accountId}, {blobId}, {name}, {type}
	cachedUploadURL        string // template with {accountId}
	cachedPrimaryAccounts  = map[string]string{} // capability → accountId
	cachedFallbackAccount  string
	cachedCapabilities     = map[string]bool{} // all capability URNs the server advertises
	cachedCapabilityData   = map[string]m{}    // full capability objects (for reading sieveExtensions etc.)
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

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
		// Validate the API URL is a trusted HTTPS origin
		if !strings.HasPrefix(url, "https://") {
			return "", "", errToolError("Session discovery: apiUrl is not HTTPS")
		}

		cachedSessionAPIURL = url
		if dl, _ := session["downloadUrl"].(string); dl != "" {
			cachedDownloadURL = dl
		}
		if ul, _ := session["uploadUrl"].(string); ul != "" {
			cachedUploadURL = ul
		}
		// Cache all capabilities the server advertises (both keys and full objects)
		if caps, _ := session["capabilities"].(map[string]any); caps != nil {
			for cap, val := range caps {
				cachedCapabilities[cap] = true
				if obj, ok := val.(map[string]any); ok {
					cachedCapabilityData[cap] = obj
				}
			}
		}
		// Also cache account-level capabilities
		if acctMap, _ := session["accounts"].(map[string]any); acctMap != nil {
			for _, acctVal := range acctMap {
				if acct, ok := acctVal.(map[string]any); ok {
					if acctCaps, ok := acct["accountCapabilities"].(map[string]any); ok {
						for cap, val := range acctCaps {
							cachedCapabilities[cap] = true
							if obj, ok := val.(map[string]any); ok {
								cachedCapabilityData[cap] = obj
							}
						}
					}
				}
			}
		}
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
		// Only include status code in error — avoid leaking response body content
		return nil, errToolError(fmt.Sprintf("JMAP call failed: HTTP %d", statusCode))
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
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
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

var calendarCaps = []string{
	"urn:ietf:params:jmap:core",
	"urn:ietf:params:jmap:calendars",
	"https://www.fastmail.com/dev/calendars",
}

func calendarAccountID() (string, error) {
	_, id, err := sessionFor(calendarCaps)
	return id, err
}

var maskedEmailCaps = []string{
	"urn:ietf:params:jmap:core",
	"https://www.fastmail.com/dev/maskedemail",
}

func maskedEmailAccountID() (string, error) {
	_, id, err := sessionFor(maskedEmailCaps)
	return id, err
}

var vacationCaps = []string{
	"urn:ietf:params:jmap:core",
	"urn:ietf:params:jmap:mail",
	"urn:ietf:params:jmap:vacationresponse",
}

var quotaCaps = []string{
	"urn:ietf:params:jmap:core",
	"urn:ietf:params:jmap:quota",
}

var submissionCapsGlobal = []string{
	"urn:ietf:params:jmap:core",
	"urn:ietf:params:jmap:mail",
	"urn:ietf:params:jmap:submission",
}

var sieveCaps = []string{
	"urn:ietf:params:jmap:core",
	"urn:ietf:params:jmap:sieve",
}

func sieveAccountID() (string, error) {
	_, id, err := sessionFor(sieveCaps)
	return id, err
}

// uploadBlob uploads text content as a blob and returns the blobId.
func uploadBlob(accountID, content, contentType string) (string, error) {
	sessionMu.Lock()
	tpl := cachedUploadURL
	sessionMu.Unlock()
	if tpl == "" {
		// Ensure session is loaded
		if _, _, err := sessionFor(defaultCaps); err != nil {
			return "", err
		}
		sessionMu.Lock()
		tpl = cachedUploadURL
		sessionMu.Unlock()
	}
	if tpl == "" {
		return "", errToolError("Upload URL not available")
	}

	uploadURL := strings.Replace(tpl, "{accountId}", url.PathEscape(accountID), 1)

	token, err := bearerToken()
	if err != nil {
		return "", err
	}

	req, _ := http.NewRequest("POST", uploadURL, strings.NewReader(content))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)

	data, statusCode, err := doHTTPWithRetry(req, 1)
	if err != nil {
		return "", err
	}
	if statusCode != 200 && statusCode != 201 {
		return "", errToolError(fmt.Sprintf("Blob upload failed: HTTP %d", statusCode))
	}

	var result m
	if err := json.Unmarshal(data, &result); err != nil {
		return "", errToolError("Blob upload: unexpected response")
	}
	blobID := getString(result, "blobId")
	if blobID == "" {
		return "", errToolError("Blob upload: no blobId in response")
	}
	return blobID, nil
}

// downloadBlobText downloads a blob and returns its content as a string.
// Capped at 1MB to prevent abuse with large non-text blobs.
func downloadBlobText(accountID, blobID string) (string, error) {
	dlURL := blobDownloadURL(accountID, blobID, "script.sieve", "application/sieve")
	if dlURL == "" {
		return "", errToolError("Download URL not available")
	}

	token, err := bearerToken()
	if err != nil {
		return "", err
	}

	req, _ := http.NewRequest("GET", dlURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)

	data, statusCode, err := doHTTPWithRetry(req, 1)
	if err != nil {
		return "", err
	}
	if statusCode != 200 {
		return "", errToolError(fmt.Sprintf("Blob download failed: HTTP %d", statusCode))
	}
	if len(data) > 1024*1024 {
		return "", errToolError(fmt.Sprintf("Blob too large for text download (%d bytes, max 1MB)", len(data)))
	}
	return string(data), nil
}

// blobDownloadURL builds the download URL for a blob.
// Name and type are URL-encoded to prevent URL structure manipulation.
func blobDownloadURL(accountID, blobID, name, mimeType string) string {
	sessionMu.Lock()
	tpl := cachedDownloadURL
	sessionMu.Unlock()
	if tpl == "" {
		return ""
	}
	r := strings.NewReplacer(
		"{accountId}", url.PathEscape(accountID),
		"{blobId}", url.PathEscape(blobID),
		"{name}", url.PathEscape(name),
		"{type}", url.QueryEscape(mimeType),
	)
	return r.Replace(tpl)
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
		attList := []m{}
		for _, att := range attachments {
			a := m{
				"name": getString(att, "name"),
				"type": getString(att, "type"),
				"size": getFloat(att, "size"),
			}
			if bid := getString(att, "blobId"); bid != "" {
				a["blobId"] = bid
			}
			attList = append(attList, a)
		}
		d["attachments"] = attList
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
	if v < 1 {
		v = 1
	}
	if maxVal > 0 && v > maxVal {
		v = maxVal
	}
	return v
}

// allowedFilterKeys are the safe JMAP Email/query filter keys that callers may use.
var allowedFilterKeys = map[string]bool{
	"text": true, "from": true, "to": true, "cc": true, "bcc": true,
	"subject": true, "body": true, "after": true, "before": true,
	"minSize": true, "maxSize": true, "hasAttachment": true,
	"hasKeyword": true, "notKeyword": true, "header": true,
	"inMailbox": true, "inMailboxOtherThan": true,
}

// sanitizeEmailFilter strips any keys not in the allowlist to prevent filter injection.
func sanitizeEmailFilter(f m) m {
	out := m{}
	for k, v := range f {
		if allowedFilterKeys[k] {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return m{"text": ""}
	}
	return out
}

// validKeywordRe matches RFC 8621 keyword characters: letters, digits, $, _, -.
var validKeywordRe = regexp.MustCompile(`^[A-Za-z0-9\$_-]+$`)

// requireIDs extracts and validates an "ids" string slice from params.
func requireIDs(params m, maxCount int) ([]string, error) {
	ids := getStringSlice(params, "ids")
	if len(ids) == 0 {
		return nil, errInvalidParams("ids is required (array of email IDs)")
	}
	if maxCount > 0 && len(ids) > maxCount {
		return nil, errInvalidParams(fmt.Sprintf("Too many IDs (%d). Maximum %d per call.", len(ids), maxCount))
	}
	return ids, nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
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

// fm_get_mailbox_stats: aggregate sender frequency, date distribution, and size stats.
// Scans up to 1000 emails for a statistical overview.
func getMailboxStats(params m) (any, error) {
	mailboxID := getString(params, "mailboxId")
	if mailboxID == "" {
		return nil, errInvalidParams("mailboxId is required")
	}
	maxScan := intParam(params, "maxScan", 500, 1000)
	onlyUnread := getBool(params, "onlyUnread")

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	filter := m{"inMailbox": mailboxID}
	if onlyUnread {
		filter["notKeyword"] = "$seen"
	}

	// Fetch in pages to build stats
	senderCounts := map[string]int{}   // email → count
	senderNames := map[string]string{} // email → display name
	domainCounts := map[string]int{}   // domain → count
	var totalSize float64
	var totalEmails int
	var oldest, newest string

	scanned := 0
	for scanned < maxScan {
		batchSize := maxScan - scanned
		if batchSize > 200 {
			batchSize = 200
		}

		responses, err := jmapCall([]any{
			[]any{"Email/query", m{
				"accountId":       acct,
				"filter":          filter,
				"sort":            []m{{"property": "receivedAt", "isAscending": false}},
				"position":        scanned,
				"limit":           batchSize,
				"collapseThreads": false,
				"calculateTotal":  scanned == 0, // only on first page
			}, "q0"},
			[]any{"Email/get", m{
				"accountId": acct,
				"#ids":      m{"resultOf": "q0", "name": "Email/query", "path": "/ids"},
				"properties": []string{"id", "from", "receivedAt", "size"},
			}, "g0"},
		}, nil)
		if err != nil {
			return nil, err
		}

		if len(responses) < 2 {
			break
		}

		// Get total on first page
		if scanned == 0 {
			if qData, ok := respData(responses[0]); ok {
				if t, ok := qData["total"].(float64); ok {
					totalEmails = int(t)
				}
			}
		}

		emails, ok := respList(responses[1])
		if !ok || len(emails) == 0 {
			break
		}

		for _, e := range emails {
			// Sender aggregation
			if addrs := getMapSlice(e, "from"); len(addrs) > 0 {
				email := strings.ToLower(getString(addrs[0], "email"))
				name := getString(addrs[0], "name")
				if email != "" {
					senderCounts[email]++
					if name != "" {
						senderNames[email] = name
					}
					parts := strings.SplitN(email, "@", 2)
					if len(parts) == 2 {
						domainCounts[parts[1]]++
					}
				}
			}

			// Date tracking
			date := getString(e, "receivedAt")
			if date != "" {
				if newest == "" || date > newest {
					newest = date
				}
				if oldest == "" || date < oldest {
					oldest = date
				}
			}

			totalSize += getFloat(e, "size")
		}

		scanned += len(emails)
		if len(emails) < batchSize {
			break // no more results
		}
	}

	// Sort senders by frequency (top 50)
	type senderEntry struct {
		Email string
		Name  string
		Count int
	}
	senders := make([]senderEntry, 0, len(senderCounts))
	for email, count := range senderCounts {
		senders = append(senders, senderEntry{email, senderNames[email], count})
	}
	// Simple insertion sort for top-N (good enough for ≤1000 unique senders)
	for i := 1; i < len(senders); i++ {
		for j := i; j > 0 && senders[j].Count > senders[j-1].Count; j-- {
			senders[j], senders[j-1] = senders[j-1], senders[j]
		}
	}
	topSenders := make([]m, 0, 50)
	for i, s := range senders {
		if i >= 50 {
			break
		}
		entry := m{"email": s.Email, "count": s.Count}
		if s.Name != "" {
			entry["name"] = s.Name
		}
		topSenders = append(topSenders, entry)
	}

	// Sort domains by frequency (top 30)
	type domainEntry struct {
		Domain string
		Count  int
	}
	domains := make([]domainEntry, 0, len(domainCounts))
	for domain, count := range domainCounts {
		domains = append(domains, domainEntry{domain, count})
	}
	for i := 1; i < len(domains); i++ {
		for j := i; j > 0 && domains[j].Count > domains[j-1].Count; j-- {
			domains[j], domains[j-1] = domains[j-1], domains[j]
		}
	}
	topDomains := make([]m, 0, 30)
	for i, d := range domains {
		if i >= 30 {
			break
		}
		topDomains = append(topDomains, m{"domain": d.Domain, "count": d.Count})
	}

	return m{
		"mailboxId":     mailboxID,
		"totalEmails":   totalEmails,
		"scanned":       scanned,
		"uniqueSenders": len(senderCounts),
		"uniqueDomains": len(domainCounts),
		"topSenders":    topSenders,
		"topDomains":    topDomains,
		"totalSizeBytes": int(totalSize),
		"oldestEmail":   oldest,
		"newestEmail":   newest,
	}, nil
}

// fm_get_sieve_capabilities: return the server's supported Sieve extensions.
func getSieveCapabilities(_ m) (any, error) {
	// Ensure session is loaded
	_, _, err := sessionFor(defaultCaps)
	if err != nil {
		return nil, err
	}

	sessionMu.Lock()
	hasSieve := cachedCapabilities["urn:ietf:params:jmap:sieve"]
	sieveData := cachedCapabilityData["urn:ietf:params:jmap:sieve"]
	sessionMu.Unlock()

	if !hasSieve {
		return m{
			"supported":  false,
			"message":    "urn:ietf:params:jmap:sieve capability not advertised by server",
		}, nil
	}

	result := m{"supported": true}

	if sieveData != nil {
		if exts := getStringSlice(sieveData, "sieveExtensions"); len(exts) > 0 {
			result["extensions"] = exts
		}
		if impl := getString(sieveData, "implementation"); impl != "" {
			result["implementation"] = impl
		}
		if maxSize, ok := sieveData["maxSizeScript"].(float64); ok {
			result["maxSizeScript"] = int(maxSize)
		}
		if maxScripts, ok := sieveData["maxNumberScripts"].(float64); ok {
			result["maxNumberScripts"] = int(maxScripts)
		}
		if maxName, ok := sieveData["maxSizeScriptName"].(float64); ok {
			result["maxSizeScriptName"] = int(maxName)
		}
		if notif := getStringSlice(sieveData, "notificationMethods"); len(notif) > 0 {
			result["notificationMethods"] = notif
		}
	}

	return result, nil
}

// fm_find_duplicates: scan a mailbox for duplicate emails by Message-ID or
// composite key (subject + from + receivedAt) and return grouped duplicates.
func findDuplicates(params m) (any, error) {
	mailboxID := getString(params, "mailboxId")
	if mailboxID == "" {
		return nil, errInvalidParams("mailboxId is required")
	}
	maxScan := intParam(params, "maxScan", 1000, 5000)

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	filter := m{"inMailbox": mailboxID}

	// Track duplicates by key → list of email entries
	type emailEntry struct {
		ID         string
		Subject    string
		From       string
		ReceivedAt string
		Size       float64
	}
	groups := map[string][]emailEntry{} // dedup key → entries

	scanned := 0
	for scanned < maxScan {
		batchSize := maxScan - scanned
		if batchSize > 200 {
			batchSize = 200
		}

		responses, err := jmapCall([]any{
			[]any{"Email/query", m{
				"accountId":       acct,
				"filter":          filter,
				"sort":            []m{{"property": "receivedAt", "isAscending": true}},
				"position":        scanned,
				"limit":           batchSize,
				"collapseThreads": false,
			}, "q0"},
			[]any{"Email/get", m{
				"accountId": acct,
				"#ids":      m{"resultOf": "q0", "name": "Email/query", "path": "/ids"},
				"properties": []string{"id", "messageId", "from", "subject", "receivedAt", "size"},
			}, "g0"},
		}, nil)
		if err != nil {
			return nil, err
		}

		if len(responses) < 2 {
			break
		}

		emails, ok := respList(responses[1])
		if !ok || len(emails) == 0 {
			break
		}

		for _, e := range emails {
			from := ""
			if addrs := getMapSlice(e, "from"); len(addrs) > 0 {
				from = strings.ToLower(getString(addrs[0], "email"))
			}

			entry := emailEntry{
				ID:         getString(e, "id"),
				Subject:    getString(e, "subject"),
				From:       from,
				ReceivedAt: getString(e, "receivedAt"),
				Size:       getFloat(e, "size"),
			}

			// Primary key: Message-ID header (most reliable)
			var key string
			if msgIDs := getStringSlice(e, "messageId"); len(msgIDs) > 0 && msgIDs[0] != "" {
				key = "msgid:" + msgIDs[0]
			} else {
				// Fallback: composite of subject + from + date (truncated to minute)
				date := entry.ReceivedAt
				if len(date) > 16 {
					date = date[:16] // "2026-04-18T14:30" — truncate seconds
				}
				key = "composite:" + from + "|" + entry.Subject + "|" + date
			}

			groups[key] = append(groups[key], entry)
		}

		scanned += len(emails)
		if len(emails) < batchSize {
			break
		}
	}

	// Filter to only groups with duplicates (2+ entries)
	type dupGroup struct {
		Key     string
		Entries []emailEntry
	}
	var duplicates []dupGroup
	totalDuplicateEmails := 0
	for key, entries := range groups {
		if len(entries) > 1 {
			duplicates = append(duplicates, dupGroup{Key: key, Entries: entries})
			totalDuplicateEmails += len(entries) - 1 // all but one are dupes
		}
	}

	// Sort by group size descending (biggest duplicate clusters first)
	for i := 1; i < len(duplicates); i++ {
		for j := i; j > 0 && len(duplicates[j].Entries) > len(duplicates[j-1].Entries); j-- {
			duplicates[j], duplicates[j-1] = duplicates[j-1], duplicates[j]
		}
	}

	// Cap output to 100 groups
	if len(duplicates) > 100 {
		duplicates = duplicates[:100]
	}

	// Format output
	outGroups := make([]m, len(duplicates))
	for i, dg := range duplicates {
		entries := make([]m, len(dg.Entries))
		for j, e := range dg.Entries {
			entries[j] = m{
				"id":         e.ID,
				"from":       e.From,
				"subject":    e.Subject,
				"receivedAt": e.ReceivedAt,
				"size":       e.Size,
			}
		}
		// Suggest keeping the first (oldest) and deleting the rest
		keepID := dg.Entries[0].ID
		deleteIDs := make([]string, 0, len(dg.Entries)-1)
		for _, e := range dg.Entries[1:] {
			deleteIDs = append(deleteIDs, e.ID)
		}
		outGroups[i] = m{
			"count":           len(dg.Entries),
			"subject":         dg.Entries[0].Subject,
			"from":            dg.Entries[0].From,
			"emails":          entries,
			"suggestKeep":     keepID,
			"suggestDeleteIds": deleteIDs,
		}
	}

	return m{
		"mailboxId":          mailboxID,
		"scanned":            scanned,
		"duplicateGroups":    len(duplicates),
		"duplicateEmails":    totalDuplicateEmails,
		"groups":             outGroups,
	}, nil
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
	return m{"status": "deleted", "id": id}, nil
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

// ── Calendar Tools ──────────────────────────────────────────────────────────

func listCalendars(_ m) (any, error) {
	acct, err := calendarAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"Calendar/get", m{
			"accountId":  acct,
			"properties": []string{"id", "name", "color", "sortOrder", "isVisible", "isSubscribed", "defaultAlertsWithTime"},
		}, "c0"},
	}, calendarCaps)
	if err != nil {
		return nil, err
	}

	list, ok := respList(responses[0])
	if !ok {
		return nil, errToolError("Unexpected Calendar/get response")
	}

	out := make([]m, len(list))
	for i, cal := range list {
		out[i] = m{
			"id":           getString(cal, "id"),
			"name":         getString(cal, "name"),
			"color":        getString(cal, "color"),
			"sortOrder":    getFloat(cal, "sortOrder"),
			"isVisible":    getBool(cal, "isVisible"),
			"isSubscribed": getBool(cal, "isSubscribed"),
		}
	}
	return out, nil
}

func listEvents(params m) (any, error) {
	after := getString(params, "after")
	before := getString(params, "before")
	if after == "" || before == "" {
		return nil, errInvalidParams("after and before are required (UTC datetime strings, e.g. 2026-04-18T00:00:00Z)")
	}
	limit := intParam(params, "limit", 50, 200)

	acct, err := calendarAccountID()
	if err != nil {
		return nil, err
	}

	filter := m{"after": after, "before": before}
	if calID := getString(params, "calendarId"); calID != "" {
		filter["inCalendars"] = []string{calID}
	}

	responses, err := jmapCall([]any{
		[]any{"CalendarEvent/query", m{
			"accountId": acct,
			"filter":    filter,
			"sort":      []m{{"property": "start", "isAscending": true}},
			"limit":     limit,
		}, "q0"},
		[]any{"CalendarEvent/get", m{
			"accountId": acct,
			"#ids":      m{"resultOf": "q0", "name": "CalendarEvent/query", "path": "/ids"},
			"properties": []string{"id", "calendarIds", "title", "start", "timeZone",
				"duration", "showWithoutTime", "status", "freeBusyStatus",
				"participants", "locations", "recurrenceRules", "alerts", "description"},
		}, "g0"},
	}, calendarCaps)
	if err != nil {
		return nil, err
	}

	if len(responses) < 2 {
		return nil, errToolError("Unexpected CalendarEvent response")
	}
	events, ok := respList(responses[1])
	if !ok {
		return nil, errToolError("Unexpected CalendarEvent response")
	}

	out := make([]m, len(events))
	for i, ev := range events {
		out[i] = eventSummaryDict(ev)
	}
	return m{"events": out}, nil
}

func getEvent(params m) (any, error) {
	eventID := getString(params, "id")
	if eventID == "" {
		return nil, errInvalidParams("id is required")
	}

	acct, err := calendarAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"CalendarEvent/get", m{
			"accountId": acct,
			"ids":       []string{eventID},
		}, "g0"},
	}, calendarCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected CalendarEvent/get response")
	}
	if notFound := getStringSlice(data, "notFound"); contains(notFound, eventID) {
		return nil, errInvalidParams("Event not found: " + eventID)
	}
	list := getMapSlice(data, "list")
	if len(list) == 0 {
		return nil, errToolError("Unexpected CalendarEvent/get response")
	}
	return list[0], nil
}

func createEvent(params m) (any, error) {
	calendarID := getString(params, "calendarId")
	if calendarID == "" {
		return nil, errInvalidParams("calendarId is required")
	}
	title := getString(params, "title")
	if title == "" {
		return nil, errInvalidParams("title is required")
	}
	start := getString(params, "start")
	if start == "" {
		return nil, errInvalidParams("start is required (e.g. 2026-04-18T14:00:00)")
	}

	acct, err := calendarAccountID()
	if err != nil {
		return nil, err
	}

	eventObj := m{
		"calendarIds": m{calendarID: true},
		"title":       title,
		"start":       start,
	}
	if tz := getString(params, "timeZone"); tz != "" {
		eventObj["timeZone"] = tz
	}
	if dur := getString(params, "duration"); dur != "" {
		eventObj["duration"] = dur
	}
	if v, ok := params["showWithoutTime"]; ok {
		eventObj["showWithoutTime"] = v
	}
	if desc := getString(params, "description"); desc != "" {
		eventObj["description"] = desc
	}
	if loc := getString(params, "location"); loc != "" {
		eventObj["locations"] = m{"loc1": m{"name": loc}}
	}
	if v, ok := params["participants"]; ok {
		eventObj["participants"] = v
	}
	if v, ok := params["alerts"]; ok {
		eventObj["alerts"] = v
	}
	if v, ok := params["recurrenceRules"]; ok {
		eventObj["recurrenceRules"] = v
	}
	if status := getString(params, "status"); status != "" {
		eventObj["status"] = status
	}

	responses, err := jmapCall([]any{
		[]any{"CalendarEvent/set", m{
			"accountId": acct,
			"create":    m{"ev0": eventObj},
		}, "c0"},
	}, calendarCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected CalendarEvent/set response")
	}
	if nc, ok := data["notCreated"].(map[string]any); ok {
		if errObj, ok := nc["ev0"].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to create event: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	result := m{"status": "created", "title": title}
	if created, ok := data["created"].(map[string]any); ok {
		if ev, ok := created["ev0"].(map[string]any); ok {
			result["id"] = getString(ev, "id")
		}
	}
	return result, nil
}

func updateEvent(params m) (any, error) {
	eventID := getString(params, "id")
	if eventID == "" {
		return nil, errInvalidParams("id is required")
	}

	acct, err := calendarAccountID()
	if err != nil {
		return nil, err
	}

	update := m{}
	for _, key := range []string{"title", "start", "timeZone", "duration", "description", "status", "freeBusyStatus"} {
		if v, ok := params[key]; ok {
			update[key] = v
		}
	}
	if v, ok := params["showWithoutTime"]; ok {
		update["showWithoutTime"] = v
	}
	if loc := getString(params, "location"); loc != "" {
		update["locations"] = m{"loc1": m{"name": loc}}
	}
	if v, ok := params["participants"]; ok {
		update["participants"] = v
	}
	if v, ok := params["alerts"]; ok {
		update["alerts"] = v
	}
	if v, ok := params["recurrenceRules"]; ok {
		update["recurrenceRules"] = v
	}
	if calID := getString(params, "calendarId"); calID != "" {
		update["calendarIds"] = m{calID: true}
	}

	if len(update) == 0 {
		return nil, errInvalidParams("at least one field to update is required")
	}

	responses, err := jmapCall([]any{
		[]any{"CalendarEvent/set", m{
			"accountId": acct,
			"update":    m{eventID: update},
		}, "u0"},
	}, calendarCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected CalendarEvent/set response")
	}
	if nu, ok := data["notUpdated"].(map[string]any); ok {
		if errObj, ok := nu[eventID].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to update event: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	return m{"status": "ok", "id": eventID}, nil
}

func deleteEvent(params m) (any, error) {
	eventID := getString(params, "id")
	if eventID == "" {
		return nil, errInvalidParams("id is required")
	}

	acct, err := calendarAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"CalendarEvent/set", m{
			"accountId": acct,
			"destroy":   []string{eventID},
		}, "d0"},
	}, calendarCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected CalendarEvent/set response")
	}
	if nd, ok := data["notDestroyed"].(map[string]any); ok {
		if errObj, ok := nd[eventID].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to delete event: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	return m{"status": "deleted", "id": eventID}, nil
}

func rsvpEvent(params m) (any, error) {
	eventID := getString(params, "id")
	if eventID == "" {
		return nil, errInvalidParams("id is required")
	}
	status := getString(params, "status")
	switch status {
	case "accepted", "declined", "tentative", "needs-action":
		// valid
	default:
		return nil, errInvalidParams("status must be one of: accepted, declined, tentative, needs-action")
	}

	acct, err := calendarAccountID()
	if err != nil {
		return nil, err
	}

	// Fetch the event to find our participant entry
	getResponses, err := jmapCall([]any{
		[]any{"CalendarEvent/get", m{
			"accountId":  acct,
			"ids":        []string{eventID},
			"properties": []string{"participants"},
		}, "g0"},
	}, calendarCaps)
	if err != nil {
		return nil, err
	}
	evList := getMapSlice(must(respData(getResponses[0])), "list")
	if len(evList) == 0 {
		return nil, errInvalidParams("Event not found: " + eventID)
	}

	participants := getMap(evList[0], "participants")
	if participants == nil {
		return nil, errToolError("Event has no participants")
	}

	// Find participant matching email param, or first attendee
	targetEmail := getString(params, "email")
	var participantID string
	for pid, pv := range participants {
		if p, ok := pv.(map[string]any); ok {
			if targetEmail != "" && getString(p, "email") == targetEmail {
				participantID = pid
				break
			}
			if roles := getMap(p, "roles"); roles != nil {
				if _, isAttendee := roles["attendee"]; isAttendee {
					participantID = pid
				}
			}
		}
	}
	if participantID == "" {
		return nil, errToolError("Could not find matching participant")
	}
	// Reject participant IDs containing '/' to prevent JMAP patch path traversal
	if strings.Contains(participantID, "/") {
		return nil, errToolError("Participant ID contains invalid characters")
	}

	update := m{
		"participants/" + participantID + "/participationStatus": status,
	}

	responses, err := jmapCall([]any{
		[]any{"CalendarEvent/set", m{
			"accountId": acct,
			"update":    m{eventID: update},
		}, "u0"},
	}, calendarCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected CalendarEvent/set response")
	}
	if nu, ok := data["notUpdated"].(map[string]any); ok {
		if errObj, ok := nu[eventID].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to RSVP: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	return m{"status": "ok", "id": eventID, "participationStatus": status}, nil
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

// ── Snooze & Flag Tools ─────────────────────────────────────────────────────

func snoozeEmail(params m) (any, error) {
	id := getString(params, "id")
	if id == "" {
		return nil, errInvalidParams("id is required")
	}
	until := getString(params, "until")
	if until == "" {
		return nil, errInvalidParams("until is required (UTC datetime, e.g. 2026-04-19T09:00:00Z)")
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	// Get inbox ID for moveToMailboxId default
	moveToID := getString(params, "mailboxId")
	if moveToID == "" {
		// Default to inbox
		mbResponses, err := jmapCall([]any{
			[]any{"Mailbox/query", m{"accountId": acct, "filter": m{"role": "inbox"}}, "mq0"},
		}, nil)
		if err == nil && len(mbResponses) > 0 {
			if qData, ok := respData(mbResponses[0]); ok {
				if ids := getStringSlice(qData, "ids"); len(ids) > 0 {
					moveToID = ids[0]
				}
			}
		}
	}

	snoozed := m{"until": until}
	if moveToID != "" {
		snoozed["moveToMailboxId"] = moveToID
	}

	update := m{id: m{"snoozed": snoozed}}

	responses, err := jmapCall([]any{
		[]any{"Email/set", m{"accountId": acct, "update": update}, "u0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	failures := checkNotUpdated(responses)
	if len(failures) > 0 {
		return nil, errToolError(fmt.Sprintf("Failed to snooze: %v", failures))
	}
	return m{"status": "ok", "id": id, "until": until}, nil
}

func flagEmail(params m) (any, error) {
	ids, err := requireIDs(params, maxBatchIDs)
	if err != nil {
		return nil, err
	}
	keyword := getString(params, "keyword")
	if keyword == "" {
		keyword = "$flagged"
	}
	if !validKeywordRe.MatchString(keyword) {
		return nil, errInvalidParams("keyword contains invalid characters (must be letters, digits, $, _, -)")
	}
	set := true
	if v, ok := params["set"].(bool); ok {
		set = v
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	update := m{}
	for _, id := range ids {
		if set {
			update[id] = m{"keywords/" + keyword: true}
		} else {
			update[id] = m{"keywords/" + keyword: nil}
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
	result := m{"status": "ok", "ids": ids, "keyword": keyword, "set": set}
	if len(failures) > 0 {
		result["failures"] = failures
	}
	return result, nil
}

// ── Spam Reporting Tools ────────────────────────────────────────────────────

// reportSpam moves emails to Junk and sets $junk keyword to train Fastmail's spam filter.
func reportSpam(params m) (any, error) {
	ids, err := requireIDs(params, maxBatchIDs)
	if err != nil {
		return nil, err
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	// Find Junk mailbox
	junkID, err := findMailboxByRole(acct, "junk")
	if err != nil {
		return nil, err
	}

	update := m{}
	for _, id := range ids {
		update[id] = m{
			"mailboxIds":      m{junkID: true},
			"keywords/$junk":  true,
			"keywords/$seen":  true, // mark read — no need to see spam again
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
		return nil, errToolError(fmt.Sprintf("Failed to report all as spam: %v", failures))
	}
	result := m{"status": "ok", "ids": ids, "movedTo": "Junk", "action": "reported as spam"}
	if len(failures) > 0 {
		result["failures"] = failures
	}
	return result, nil
}

// reportPhishing moves emails to Junk and sets $phishing keyword.
func reportPhishing(params m) (any, error) {
	ids, err := requireIDs(params, maxBatchIDs)
	if err != nil {
		return nil, err
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	junkID, err := findMailboxByRole(acct, "junk")
	if err != nil {
		return nil, err
	}

	update := m{}
	for _, id := range ids {
		update[id] = m{
			"mailboxIds":         m{junkID: true},
			"keywords/$phishing": true,
			"keywords/$junk":     true,
			"keywords/$seen":     true,
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
		return nil, errToolError(fmt.Sprintf("Failed to report all as phishing: %v", failures))
	}
	result := m{"status": "ok", "ids": ids, "movedTo": "Junk", "action": "reported as phishing"}
	if len(failures) > 0 {
		result["failures"] = failures
	}
	return result, nil
}

// reportNotSpam moves emails out of Junk, removes $junk, sets $notjunk.
func reportNotSpam(params m) (any, error) {
	ids, err := requireIDs(params, maxBatchIDs)
	if err != nil {
		return nil, err
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	// Move to Inbox by default, or specified mailbox
	destID := getString(params, "mailboxId")
	if destID == "" {
		var err error
		destID, err = findMailboxByRole(acct, "inbox")
		if err != nil {
			return nil, err
		}
	}

	update := m{}
	for _, id := range ids {
		update[id] = m{
			"mailboxIds":        m{destID: true},
			"keywords/$notjunk": true,
			"keywords/$junk":    nil,
			"keywords/$phishing": nil,
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
		return nil, errToolError(fmt.Sprintf("Failed to report all as not spam: %v", failures))
	}
	result := m{"status": "ok", "ids": ids, "action": "reported as not spam"}
	if len(failures) > 0 {
		result["failures"] = failures
	}
	return result, nil
}

// ── Archive & Destroy Tools ─────────────────────────────────────────────────

// archiveEmail moves emails to the Archive mailbox.
func archiveEmail(params m) (any, error) {
	ids, err := requireIDs(params, maxBatchIDs)
	if err != nil {
		return nil, err
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	archiveID, err := findMailboxByRole(acct, "archive")
	if err != nil {
		return nil, err
	}

	update := m{}
	for _, id := range ids {
		update[id] = m{"mailboxIds": m{archiveID: true}}
	}

	responses, err := jmapCall([]any{
		[]any{"Email/set", m{"accountId": acct, "update": update}, "u0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	failures := checkNotUpdated(responses)
	if len(failures) > 0 && len(failures) == len(ids) {
		return nil, errToolError(fmt.Sprintf("Failed to archive all emails: %v", failures))
	}
	result := m{"status": "ok", "ids": ids, "movedTo": "Archive"}
	if len(failures) > 0 {
		result["failures"] = failures
	}
	return result, nil
}

// destroyEmail permanently deletes emails (bypasses Trash).
func destroyEmail(params m) (any, error) {
	ids, err := requireIDs(params, 100) // lower cap for irreversible operation
	if err != nil {
		return nil, err
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"Email/set", m{
			"accountId": acct,
			"destroy":   ids,
		}, "d0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected Email/set response")
	}

	var failures map[string]string
	if nd, ok := data["notDestroyed"].(map[string]any); ok {
		failures = map[string]string{}
		for id, errObj := range nd {
			if e, ok := errObj.(map[string]any); ok {
				failures[id] = getString(e, "type") + ": " + getString(e, "description")
			} else {
				failures[id] = "unknown error"
			}
		}
	}

	if len(failures) > 0 && len(failures) == len(ids) {
		return nil, errToolError(fmt.Sprintf("Failed to destroy all emails: %v", failures))
	}
	result := m{"status": "ok", "ids": ids, "action": "permanently deleted"}
	if len(failures) > 0 {
		result["failures"] = failures
	}
	return result, nil
}

// unsnoozeEmail clears the snooze on an email.
func unsnoozeEmail(params m) (any, error) {
	id := getString(params, "id")
	if id == "" {
		return nil, errInvalidParams("id is required")
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	update := m{id: m{"snoozed": nil}}

	responses, err := jmapCall([]any{
		[]any{"Email/set", m{"accountId": acct, "update": update}, "u0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	failures := checkNotUpdated(responses)
	if len(failures) > 0 {
		return nil, errToolError(fmt.Sprintf("Failed to unsnooze: %v", failures))
	}
	return m{"status": "ok", "id": id, "action": "unsnoozed"}, nil
}

// findMailboxByRole finds a mailbox ID by its role (inbox, junk, archive, trash, etc.)
func findMailboxByRole(acct, role string) (string, error) {
	responses, err := jmapCall([]any{
		[]any{"Mailbox/query", m{"accountId": acct, "filter": m{"role": role}}, "mq0"},
		[]any{"Mailbox/get", m{
			"accountId":  acct,
			"#ids":       m{"resultOf": "mq0", "name": "Mailbox/query", "path": "/ids"},
			"properties": []string{"id"},
		}, "mg0"},
	}, nil)
	if err != nil {
		return "", err
	}

	if len(responses) < 2 {
		return "", errToolError(fmt.Sprintf("Could not find %s mailbox", role))
	}
	list, ok := respList(responses[1])
	if !ok || len(list) == 0 {
		return "", errToolError(fmt.Sprintf("Could not find %s mailbox", role))
	}
	id := getString(list[0], "id")
	if id == "" {
		return "", errToolError(fmt.Sprintf("Could not find %s mailbox", role))
	}
	return id, nil
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

// ── Identity Management Tools ───────────────────────────────────────────────

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

// ── Email Import Tools ──────────────────────────────────────────────────────

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

// ── Newsletter / Mailing List Tools ─────────────────────────────────────────

// fm_detect_newsletters scans a mailbox for emails with List-Id/List-Unsubscribe headers
// and aggregates by mailing list.
func detectNewsletters(params m) (any, error) {
	mailboxID := getString(params, "mailboxId")
	if mailboxID == "" {
		return nil, errInvalidParams("mailboxId is required")
	}
	maxScan := intParam(params, "maxScan", 500, 2000)

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	filter := m{"inMailbox": mailboxID}

	type listInfo struct {
		ListID       string
		Name         string
		From         string
		FromName     string
		Unsubscribe  string
		UnsubPost    bool // has List-Unsubscribe-Post (RFC 8058 one-click)
		Count        int
		FirstSeen    string
		LastSeen     string
		SampleIDs    []string
	}
	lists := map[string]*listInfo{} // key by listId or from-address

	scanned := 0
	for scanned < maxScan {
		batchSize := maxScan - scanned
		if batchSize > 200 {
			batchSize = 200
		}

		responses, err := jmapCall([]any{
			[]any{"Email/query", m{
				"accountId":       acct,
				"filter":          filter,
				"sort":            []m{{"property": "receivedAt", "isAscending": false}},
				"position":        scanned,
				"limit":           batchSize,
				"collapseThreads": false,
			}, "q0"},
			[]any{"Email/get", m{
				"accountId": acct,
				"#ids":      m{"resultOf": "q0", "name": "Email/query", "path": "/ids"},
				"properties": []string{"id", "from", "subject", "receivedAt",
					"header:List-Id:asText",
					"header:List-Unsubscribe:asText",
					"header:List-Unsubscribe-Post:asText"},
			}, "g0"},
		}, nil)
		if err != nil {
			return nil, err
		}

		if len(responses) < 2 {
			break
		}

		emails, ok := respList(responses[1])
		if !ok || len(emails) == 0 {
			break
		}

		for _, e := range emails {
			listID, _ := e["header:List-Id:asText"].(string)
			unsubscribe, _ := e["header:List-Unsubscribe:asText"].(string)
			unsubPost, _ := e["header:List-Unsubscribe-Post:asText"].(string)

			// Only process emails that have mailing list headers
			if listID == "" && unsubscribe == "" {
				continue
			}

			from := ""
			fromName := ""
			if addrs := getMapSlice(e, "from"); len(addrs) > 0 {
				from = strings.ToLower(getString(addrs[0], "email"))
				fromName = getString(addrs[0], "name")
			}

			// Key by List-Id if available, else by from address
			key := listID
			if key == "" {
				key = "from:" + from
			}

			date := getString(e, "receivedAt")
			emailID := getString(e, "id")

			if li, ok := lists[key]; ok {
				li.Count++
				if date != "" && date < li.FirstSeen {
					li.FirstSeen = date
				}
				if date != "" && date > li.LastSeen {
					li.LastSeen = date
				}
				if len(li.SampleIDs) < 3 {
					li.SampleIDs = append(li.SampleIDs, emailID)
				}
			} else {
				lists[key] = &listInfo{
					ListID:      listID,
					Name:        fromName,
					From:        from,
					FromName:    fromName,
					Unsubscribe: unsubscribe,
					UnsubPost:   strings.Contains(strings.ToLower(unsubPost), "list-unsubscribe=one-click"),
					Count:       1,
					FirstSeen:   date,
					LastSeen:    date,
					SampleIDs:   []string{emailID},
				}
			}
		}

		scanned += len(emails)
		if len(emails) < batchSize {
			break
		}
	}

	// Sort by count descending
	type listEntry struct {
		Key  string
		Info *listInfo
	}
	sorted := make([]listEntry, 0, len(lists))
	for k, v := range lists {
		sorted = append(sorted, listEntry{k, v})
	}
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].Info.Count > sorted[j-1].Info.Count; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}

	out := make([]m, 0, len(sorted))
	for _, entry := range sorted {
		li := entry.Info
		d := m{
			"from":      li.From,
			"name":      li.Name,
			"count":     li.Count,
			"firstSeen": li.FirstSeen,
			"lastSeen":  li.LastSeen,
			"sampleIds": li.SampleIDs,
		}
		if li.ListID != "" {
			d["listId"] = li.ListID
		}
		if li.Unsubscribe != "" {
			d["unsubscribeHeader"] = li.Unsubscribe
			d["canOneClickUnsubscribe"] = li.UnsubPost
		}
		out = append(out, d)
	}

	return m{
		"mailboxId":   mailboxID,
		"scanned":     scanned,
		"listsFound":  len(out),
		"newsletters": out,
	}, nil
}

// fm_unsubscribe_list performs RFC 8058 one-click List-Unsubscribe-Post for TRUSTED senders.
func unsubscribeList(params m) (any, error) {
	emailID := getString(params, "emailId")
	if emailID == "" {
		return nil, errInvalidParams("emailId is required (an email from the list to unsubscribe from)")
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	// Fetch the unsubscribe headers
	responses, err := jmapCall([]any{
		[]any{"Email/get", m{
			"accountId": acct,
			"ids":       []string{emailID},
			"properties": []string{"from",
				"header:List-Unsubscribe:asText",
				"header:List-Unsubscribe-Post:asText"},
		}, "g0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	list := getMapSlice(must(respData(responses[0])), "list")
	if len(list) == 0 {
		return nil, errInvalidParams("Email not found: " + emailID)
	}
	email := list[0]

	unsubHeader, _ := email["header:List-Unsubscribe:asText"].(string)
	unsubPostHeader, _ := email["header:List-Unsubscribe-Post:asText"].(string)

	if unsubHeader == "" {
		return nil, errInvalidParams("Email has no List-Unsubscribe header — cannot unsubscribe via standard mechanism")
	}

	// Check for RFC 8058 one-click support
	if !strings.Contains(strings.ToLower(unsubPostHeader), "list-unsubscribe=one-click") {
		return m{
			"status":  "unsupported",
			"message": "Email does not support RFC 8058 one-click unsubscribe. The List-Unsubscribe header is present but requires manual action (likely a URL to visit). Consider using fm_report_spam instead if this is unwanted mail.",
			"unsubscribeHeader": unsubHeader,
		}, nil
	}

	// Extract HTTPS URL from List-Unsubscribe header
	// Format: <https://example.com/unsubscribe>, <mailto:unsub@example.com>
	var unsubURL string
	for _, part := range strings.Split(unsubHeader, ",") {
		part = strings.TrimSpace(part)
		part = strings.Trim(part, "<>")
		if strings.HasPrefix(part, "https://") {
			unsubURL = part
			break
		}
	}

	if unsubURL == "" {
		return m{
			"status":  "unsupported",
			"message": "List-Unsubscribe header has no HTTPS URL — only mailto. Use fm_report_spam for untrusted senders.",
			"unsubscribeHeader": unsubHeader,
		}, nil
	}

	// Perform the RFC 8058 one-click POST
	req, _ := http.NewRequest("POST", unsubURL, strings.NewReader("List-Unsubscribe=One-Click"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	data, statusCode, err := doHTTPWithRetry(req, 1)
	if err != nil {
		return nil, errToolError("Unsubscribe request failed: " + err.Error())
	}

	result := m{
		"status":     "sent",
		"url":        unsubURL,
		"httpStatus": statusCode,
	}
	if statusCode >= 200 && statusCode < 300 {
		result["message"] = "Unsubscribe request accepted"
	} else {
		result["message"] = fmt.Sprintf("Server returned HTTP %d — unsubscribe may not have succeeded", statusCode)
		if len(data) > 0 && len(data) < 500 {
			result["response"] = string(data)
		}
	}
	return result, nil
}

// ── Draft Management Tools ──────────────────────────────────────────────────

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

// ── Follow-up Finder Tools ──────────────────────────────────────────────────

// fm_find_unreplied scans Sent mailbox for emails that haven't received replies.
func findUnreplied(params m) (any, error) {
	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	// Find Sent mailbox
	sentID, err := findMailboxByRole(acct, "sent")
	if err != nil {
		return nil, err
	}

	maxScan := intParam(params, "maxScan", 200, 500)
	daysOld := intParam(params, "daysOld", 3, 90)

	// Calculate the cutoff date
	cutoff := time.Now().UTC().AddDate(0, 0, -daysOld).Format("2006-01-02T15:04:05Z")

	responses, err := jmapCall([]any{
		[]any{"Email/query", m{
			"accountId": acct,
			"filter":    m{"inMailbox": sentID, "after": cutoff},
			"sort":      []m{{"property": "receivedAt", "isAscending": false}},
			"limit":     maxScan,
		}, "q0"},
		[]any{"Email/get", m{
			"accountId": acct,
			"#ids":      m{"resultOf": "q0", "name": "Email/query", "path": "/ids"},
			"properties": []string{"id", "threadId", "to", "subject", "receivedAt", "messageId"},
		}, "g0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	if len(responses) < 2 {
		return nil, errToolError("Unexpected response")
	}
	sentEmails, ok := respList(responses[1])
	if !ok {
		return nil, errToolError("Unexpected response")
	}

	// For each sent email, check if the thread has a reply (another email from someone else)
	// Batch thread IDs
	threadIDs := map[string]bool{}
	emailsByThread := map[string][]m{}
	for _, e := range sentEmails {
		tid := getString(e, "threadId")
		if tid != "" {
			threadIDs[tid] = true
			emailsByThread[tid] = append(emailsByThread[tid], e)
		}
	}

	// Fetch threads to get all email IDs
	threadIDSlice := make([]string, 0, len(threadIDs))
	for tid := range threadIDs {
		threadIDSlice = append(threadIDSlice, tid)
	}

	// Cap thread fetch to prevent excessive calls
	if len(threadIDSlice) > 100 {
		threadIDSlice = threadIDSlice[:100]
	}

	threadResponses, err := jmapCall([]any{
		[]any{"Thread/get", m{
			"accountId": acct,
			"ids":       threadIDSlice,
		}, "t0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	threadList, _ := respList(threadResponses[0])
	threadSizes := map[string]int{} // threadId → total emails in thread
	for _, t := range threadList {
		tid := getString(t, "id")
		emailIDs := getStringSlice(t, "emailIds")
		threadSizes[tid] = len(emailIDs)
	}

	// Unreplied = sent emails in threads with only 1 email (the sent one itself)
	unreplied := []m{}
	for _, e := range sentEmails {
		tid := getString(e, "threadId")
		if threadSizes[tid] <= 1 {
			to := ""
			if addrs := getMapSlice(e, "to"); len(addrs) > 0 {
				to = getString(addrs[0], "email")
				if name := getString(addrs[0], "name"); name != "" {
					to = name + " <" + to + ">"
				}
			}
			unreplied = append(unreplied, m{
				"id":         getString(e, "id"),
				"to":         to,
				"subject":    getString(e, "subject"),
				"sentAt":     getString(e, "receivedAt"),
				"daysSince":  int(time.Since(parseTime(getString(e, "receivedAt"))).Hours() / 24),
			})
		}
	}

	return m{
		"scanned":   len(sentEmails),
		"unreplied": unreplied,
		"count":     len(unreplied),
		"daysOld":   daysOld,
	}, nil
}

// ── Sender Analysis Tools ───────────────────────────────────────────────────

func analyzeSender(params m) (any, error) {
	sender := getString(params, "email")
	if sender == "" {
		return nil, errInvalidParams("email is required (sender email address to analyze)")
	}

	acct, err := mailAccountID()
	if err != nil {
		return nil, err
	}

	maxScan := intParam(params, "maxScan", 200, 500)

	// Search for all emails from this sender
	responses, err := jmapCall([]any{
		[]any{"Email/query", m{
			"accountId":      acct,
			"filter":         m{"from": sender},
			"sort":           []m{{"property": "receivedAt", "isAscending": true}},
			"limit":          maxScan,
			"calculateTotal": true,
		}, "q0"},
		[]any{"Email/get", m{
			"accountId": acct,
			"#ids":      m{"resultOf": "q0", "name": "Email/query", "path": "/ids"},
			"properties": []string{"id", "mailboxIds", "subject", "receivedAt", "keywords", "size",
				"header:List-Id:asText",
				"header:List-Unsubscribe:asText",
				"header:List-Unsubscribe-Post:asText",
				"header:Authentication-Results:asText"},
		}, "g0"},
	}, nil)
	if err != nil {
		return nil, err
	}

	if len(responses) < 2 {
		return nil, errToolError("Unexpected response")
	}

	var totalEmails int
	if qData, ok := respData(responses[0]); ok {
		if t, ok := qData["total"].(float64); ok {
			totalEmails = int(t)
		}
	}

	emails, ok := respList(responses[1])
	if !ok {
		return nil, errToolError("Unexpected response")
	}

	// Aggregate stats
	mailboxHits := map[string]int{}
	var totalSize float64
	var readCount, unreadCount, flaggedCount int
	var hasListHeaders bool
	var listID, unsubscribe string
	var authResults []string
	var oldest, newest string
	subjects := map[string]int{}

	for _, e := range emails {
		// Mailbox distribution
		if mbIds := getMap(e, "mailboxIds"); mbIds != nil {
			for mbId := range mbIds {
				mailboxHits[mbId]++
			}
		}

		// Read/flag status
		kw := getMap(e, "keywords")
		if _, seen := kw["$seen"]; seen {
			readCount++
		} else {
			unreadCount++
		}
		if _, flagged := kw["$flagged"]; flagged {
			flaggedCount++
		}

		// Size
		totalSize += getFloat(e, "size")

		// Dates
		date := getString(e, "receivedAt")
		if date != "" {
			if oldest == "" || date < oldest {
				oldest = date
			}
			if newest == "" || date > newest {
				newest = date
			}
		}

		// List headers
		if lid, _ := e["header:List-Id:asText"].(string); lid != "" {
			hasListHeaders = true
			listID = lid
		}
		if unsub, _ := e["header:List-Unsubscribe:asText"].(string); unsub != "" {
			unsubscribe = unsub
		}

		// Auth results (sample first one)
		if ar, _ := e["header:Authentication-Results:asText"].(string); ar != "" && len(authResults) < 3 {
			authResults = append(authResults, ar)
		}

		// Subject patterns (top 10)
		subj := getString(e, "subject")
		if subj != "" && len(subjects) < 100 {
			subjects[subj]++
		}
	}

	// Resolve mailbox names
	mbResponses, err := jmapCall([]any{
		[]any{"Mailbox/get", m{
			"accountId":  acct,
			"properties": []string{"id", "name", "role"},
		}, "mb0"},
	}, nil)
	mbNameMap := map[string]string{}
	if err == nil && len(mbResponses) > 0 {
		if mbList, ok := respList(mbResponses[0]); ok {
			for _, mb := range mbList {
				mbNameMap[getString(mb, "id")] = getString(mb, "name")
			}
		}
	}

	mailboxDist := []m{}
	for mbId, count := range mailboxHits {
		name := mbNameMap[mbId]
		if name == "" {
			name = mbId
		}
		mailboxDist = append(mailboxDist, m{"mailbox": name, "count": count})
	}

	// Top subjects
	type subjEntry struct {
		Subject string
		Count   int
	}
	topSubjects := make([]subjEntry, 0, len(subjects))
	for s, c := range subjects {
		topSubjects = append(topSubjects, subjEntry{s, c})
	}
	for i := 1; i < len(topSubjects); i++ {
		for j := i; j > 0 && topSubjects[j].Count > topSubjects[j-1].Count; j-- {
			topSubjects[j], topSubjects[j-1] = topSubjects[j-1], topSubjects[j]
		}
	}
	topSubjOut := make([]m, 0, 10)
	for i, s := range topSubjects {
		if i >= 10 {
			break
		}
		topSubjOut = append(topSubjOut, m{"subject": s.Subject, "count": s.Count})
	}

	result := m{
		"email":          sender,
		"totalEmails":    totalEmails,
		"scanned":        len(emails),
		"readCount":      readCount,
		"unreadCount":    unreadCount,
		"flaggedCount":   flaggedCount,
		"totalSizeBytes": int(totalSize),
		"oldestEmail":    oldest,
		"newestEmail":    newest,
		"mailboxes":      mailboxDist,
		"topSubjects":    topSubjOut,
		"isMailingList":  hasListHeaders,
	}

	if hasListHeaders {
		result["listId"] = listID
		result["unsubscribeHeader"] = unsubscribe
	}
	if len(authResults) > 0 {
		result["authenticationResults"] = authResults
	}

	return result, nil
}

// parseTime attempts to parse an ISO 8601 datetime, returning zero time on failure.
func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ── Sieve Filter Tools ──────────────────────────────────────────────────────

func listSieveScripts(_ m) (any, error) {
	acct, err := sieveAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"SieveScript/get", m{
			"accountId": acct,
			"ids":       nil, // null = get all
		}, "s0"},
	}, sieveCaps)
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

	acct, err := sieveAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"SieveScript/get", m{
			"accountId": acct,
			"ids":       []string{id},
		}, "s0"},
	}, sieveCaps)
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

	acct, err := sieveAccountID()
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
	}, sieveCaps)
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

	acct, err := sieveAccountID()
	if err != nil {
		return nil, err
	}

	// Check if this script is active before destroying
	getResponses, err := jmapCall([]any{
		[]any{"SieveScript/get", m{
			"accountId": acct,
			"ids":       []string{id},
		}, "g0"},
	}, sieveCaps)
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
	}, sieveCaps)
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
	acct, err := sieveAccountID()
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
	}, sieveCaps)
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

	acct, err := sieveAccountID()
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
	}, sieveCaps)
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

// ── Calendar Management Tools ───────────────────────────────────────────────

func createCalendar(params m) (any, error) {
	name := getString(params, "name")
	if name == "" {
		return nil, errInvalidParams("name is required")
	}

	acct, err := calendarAccountID()
	if err != nil {
		return nil, err
	}

	createObj := m{"name": name}
	if color := getString(params, "color"); color != "" {
		createObj["color"] = color
	}
	if v, ok := params["isVisible"]; ok {
		createObj["isVisible"] = v
	}
	if v, ok := params["sortOrder"]; ok {
		createObj["sortOrder"] = v
	}

	responses, err := jmapCall([]any{
		[]any{"Calendar/set", m{
			"accountId": acct,
			"create":    m{"cal0": createObj},
		}, "c0"},
	}, calendarCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected Calendar/set response")
	}
	if nc, ok := data["notCreated"].(map[string]any); ok {
		if errObj, ok := nc["cal0"].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to create calendar: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	result := m{"status": "created", "name": name}
	if created, ok := data["created"].(map[string]any); ok {
		if cal, ok := created["cal0"].(map[string]any); ok {
			result["id"] = getString(cal, "id")
		}
	}
	return result, nil
}

func updateCalendar(params m) (any, error) {
	id := getString(params, "id")
	if id == "" {
		return nil, errInvalidParams("id is required")
	}

	acct, err := calendarAccountID()
	if err != nil {
		return nil, err
	}

	update := m{}
	for _, key := range []string{"name", "color"} {
		if v, ok := params[key]; ok {
			update[key] = v
		}
	}
	if v, ok := params["isVisible"]; ok {
		update["isVisible"] = v
	}
	if v, ok := params["sortOrder"]; ok {
		update["sortOrder"] = v
	}

	if len(update) == 0 {
		return nil, errInvalidParams("at least one field to update is required")
	}

	responses, err := jmapCall([]any{
		[]any{"Calendar/set", m{
			"accountId": acct,
			"update":    m{id: update},
		}, "u0"},
	}, calendarCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected Calendar/set response")
	}
	if nu, ok := data["notUpdated"].(map[string]any); ok {
		if errObj, ok := nu[id].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to update calendar: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	return m{"status": "ok", "id": id}, nil
}

func deleteCalendar(params m) (any, error) {
	id := getString(params, "id")
	if id == "" {
		return nil, errInvalidParams("id is required")
	}

	acct, err := calendarAccountID()
	if err != nil {
		return nil, err
	}

	responses, err := jmapCall([]any{
		[]any{"Calendar/set", m{
			"accountId": acct,
			"destroy":   []string{id},
		}, "d0"},
	}, calendarCaps)
	if err != nil {
		return nil, err
	}

	data, ok := respData(responses[0])
	if !ok {
		return nil, errToolError("Unexpected Calendar/set response")
	}
	if nd, ok := data["notDestroyed"].(map[string]any); ok {
		if errObj, ok := nd[id].(map[string]any); ok {
			return nil, errToolError(fmt.Sprintf("Failed to delete calendar: %s — %s",
				getString(errObj, "type"), getString(errObj, "description")))
		}
	}
	return m{"status": "deleted", "id": id}, nil
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

// ── Email Parse Tools ───────────────────────────────────────────────────────

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

// ── Calendar Serialization Helpers ──────────────────────────────────────────

func eventSummaryDict(ev m) m {
	d := m{
		"id":              getString(ev, "id"),
		"title":           getString(ev, "title"),
		"start":           getString(ev, "start"),
		"timeZone":        getString(ev, "timeZone"),
		"duration":        getString(ev, "duration"),
		"showWithoutTime": getBool(ev, "showWithoutTime"),
		"status":          getString(ev, "status"),
		"freeBusyStatus":  getString(ev, "freeBusyStatus"),
	}
	if desc := getString(ev, "description"); desc != "" {
		d["description"] = desc
	}
	if locs := getMap(ev, "locations"); locs != nil {
		names := []string{}
		for _, v := range locs {
			if loc, ok := v.(map[string]any); ok {
				if n := getString(loc, "name"); n != "" {
					names = append(names, n)
				}
			}
		}
		if len(names) > 0 {
			d["locations"] = names
		}
	}
	if participants := getMap(ev, "participants"); participants != nil {
		pList := []m{}
		for _, v := range participants {
			if p, ok := v.(map[string]any); ok {
				pList = append(pList, m{
					"name":   getString(p, "name"),
					"email":  getString(p, "email"),
					"status": getString(p, "participationStatus"),
				})
			}
		}
		if len(pList) > 0 {
			d["participants"] = pList
		}
	}
	return d
}

// must extracts data from respData, returning empty map on failure (for chaining).
func must(data m, ok bool) m {
	if !ok {
		return m{}
	}
	return data
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
		Description: "Aggregate statistics for a mailbox: top senders, top domains, date range, size. Scans up to 1000 emails for a statistical overview — ideal for planning cleanup and Sieve rules.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"mailboxId":  m{"type": "string", "description": "Mailbox ID to analyze"},
				"maxScan":    m{"type": "integer", "description": "Max emails to scan (default 500, max 1000)"},
				"onlyUnread": m{"type": "boolean", "description": "Only analyze unread emails (default false)"},
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
		Description: "Scan a mailbox for newsletters and mailing lists by detecting List-Id/List-Unsubscribe headers. Returns aggregated list with sender, count, and whether RFC 8058 one-click unsubscribe is supported.",
		InputSchema: m{
			"type": "object",
			"properties": m{
				"mailboxId": m{"type": "string", "description": "Mailbox ID to scan"},
				"maxScan":   m{"type": "integer", "description": "Max emails to scan (default 500, max 2000)"},
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

// ── Tool Dispatch ───────────────────────────────────────────────────────────

type toolFunc func(m) (any, error)

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

func callTool(name string, arguments m) (any, error) {
	handler, ok := toolHandlers[name]
	if !ok {
		return nil, errToolNotFound(name)
	}
	return handler(arguments)
}

// ── MCP Server ──────────────────────────────────────────────────────────────

const maxInputBytes = 10 * 1024 * 1024
const maxResponseBytes = 50 * 1024 * 1024 // 50MB cap on JMAP response reads
const maxBatchIDs = 500                    // cap on IDs per destructive batch (well under JMAP maxObjectsInSet)

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
			"serverInfo":      m{"name": "fastmail-mcp", "version": "3.1.0"},
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
