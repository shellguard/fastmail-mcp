package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

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
		if dl, _ := session["downloadUrl"].(string); dl != "" && strings.HasPrefix(dl, "https://") {
			cachedDownloadURL = dl
		}
		if ul, _ := session["uploadUrl"].(string); ul != "" && strings.HasPrefix(ul, "https://") {
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
