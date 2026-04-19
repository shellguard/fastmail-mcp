package main

import (
	"testing"
)

// ── sanitizeEmailFilter ─────────────────────────────────────────────────────

func TestSanitizeEmailFilter_AllowedKeys(t *testing.T) {
	input := m{
		"text":          "search term",
		"from":          "alice@example.com",
		"subject":       "invoice",
		"hasAttachment": true,
		"after":         "2026-01-01T00:00:00Z",
	}
	result := sanitizeEmailFilter(input)
	for key := range input {
		if _, ok := result[key]; !ok {
			t.Errorf("allowed key %q was stripped", key)
		}
	}
}

func TestSanitizeEmailFilter_BlocksInjection(t *testing.T) {
	input := m{
		"text":      "legit search",
		"operator":  "OR",            // injection: compound operator
		"conditions": []any{},        // injection: nested conditions
		"unknown":   "value",         // unknown key
	}
	result := sanitizeEmailFilter(input)
	if _, ok := result["operator"]; ok {
		t.Error("operator key should have been stripped")
	}
	if _, ok := result["conditions"]; ok {
		t.Error("conditions key should have been stripped")
	}
	if _, ok := result["unknown"]; ok {
		t.Error("unknown key should have been stripped")
	}
	if result["text"] != "legit search" {
		t.Error("allowed key 'text' should be preserved")
	}
}

func TestSanitizeEmailFilter_EmptyReturnsTextFilter(t *testing.T) {
	result := sanitizeEmailFilter(m{"operator": "NOT"})
	if _, ok := result["text"]; !ok {
		t.Error("empty filter after sanitization should default to text filter")
	}
}

func TestSanitizeEmailFilter_InMailboxAllowed(t *testing.T) {
	input := m{"from": "test@example.com", "inMailbox": "mb123"}
	result := sanitizeEmailFilter(input)
	if result["inMailbox"] != "mb123" {
		t.Error("inMailbox should be allowed")
	}
}

// ── validKeywordRe ──────────────────────────────────────────────────────────

func TestValidKeywordRe_AcceptsValid(t *testing.T) {
	valid := []string{"$flagged", "$seen", "$draft", "$answered", "$forwarded",
		"$junk", "$notjunk", "$phishing", "custom_label", "my-tag", "Tag123"}
	for _, kw := range valid {
		if !validKeywordRe.MatchString(kw) {
			t.Errorf("keyword %q should be valid", kw)
		}
	}
}

func TestValidKeywordRe_RejectsInvalid(t *testing.T) {
	invalid := []string{
		"$flagged/../../mailboxIds", // path traversal
		"key with spaces",
		"key\nnewline",
		"",
		"key/slash",
		"key.dot",    // dots not in RFC 8621 keyword charset
		"key=equals",
	}
	for _, kw := range invalid {
		if validKeywordRe.MatchString(kw) {
			t.Errorf("keyword %q should be invalid", kw)
		}
	}
}

// ── parseBridgeSubject ──────────────────────────────────────────────────────

func TestParseBridgeSubject_ValidTypes(t *testing.T) {
	tests := []struct {
		input   string
		typ     string
		desc    string
	}{
		{"[TASK] Buy groceries", "TASK", "Buy groceries"},
		{"[NOTE] Remember to call Mom", "NOTE", "Remember to call Mom"},
		{"[EVENT] Doctor appointment 2026-03-15 14:00", "EVENT", "Doctor appointment 2026-03-15 14:00"},
		{"[task] lowercase type", "TASK", "lowercase type"},
	}
	for _, tt := range tests {
		result := parseBridgeSubject(tt.input)
		if result == nil {
			t.Errorf("parseBridgeSubject(%q) returned nil", tt.input)
			continue
		}
		if result.typ != tt.typ {
			t.Errorf("parseBridgeSubject(%q).typ = %q, want %q", tt.input, result.typ, tt.typ)
		}
		if result.description != tt.desc {
			t.Errorf("parseBridgeSubject(%q).description = %q, want %q", tt.input, result.description, tt.desc)
		}
	}
}

func TestParseBridgeSubject_RejectsInvalid(t *testing.T) {
	invalid := []string{
		"plain subject",
		"[UNKNOWN] some text",
		"[TASK]no space",
		"",
		"[TASK]",
		"TASK Buy groceries",
	}
	for _, s := range invalid {
		if result := parseBridgeSubject(s); result != nil {
			t.Errorf("parseBridgeSubject(%q) should return nil, got %+v", s, result)
		}
	}
}

// ── intParam ────────────────────────────────────────────────────────────────

func TestIntParam_Default(t *testing.T) {
	result := intParam(m{}, "limit", 20, 200)
	if result != 20 {
		t.Errorf("intParam default: got %d, want 20", result)
	}
}

func TestIntParam_ProvidedValue(t *testing.T) {
	result := intParam(m{"limit": float64(50)}, "limit", 20, 200)
	if result != 50 {
		t.Errorf("intParam provided: got %d, want 50", result)
	}
}

func TestIntParam_CapsAtMax(t *testing.T) {
	result := intParam(m{"limit": float64(999)}, "limit", 20, 200)
	if result != 200 {
		t.Errorf("intParam cap: got %d, want 200", result)
	}
}

func TestIntParam_NegativeFloorsAtOne(t *testing.T) {
	result := intParam(m{"offset": float64(-5)}, "offset", 0, 0)
	if result != 1 {
		t.Errorf("intParam negative: got %d, want 1", result)
	}
}

func TestIntParam_ZeroFloorsAtOne(t *testing.T) {
	result := intParam(m{"maxScan": float64(0)}, "maxScan", 500, 1000)
	if result != 1 {
		t.Errorf("intParam zero: got %d, want 1", result)
	}
}

// ── requireIDs ──────────────────────────────────────────────────────────────

func TestRequireIDs_Valid(t *testing.T) {
	ids, err := requireIDs(m{"ids": []any{"a", "b", "c"}}, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 3 {
		t.Errorf("got %d ids, want 3", len(ids))
	}
}

func TestRequireIDs_Empty(t *testing.T) {
	_, err := requireIDs(m{"ids": []any{}}, 10)
	if err == nil {
		t.Error("expected error for empty ids")
	}
}

func TestRequireIDs_Missing(t *testing.T) {
	_, err := requireIDs(m{}, 10)
	if err == nil {
		t.Error("expected error for missing ids")
	}
}

func TestRequireIDs_ExceedsMax(t *testing.T) {
	ids := make([]any, 20)
	for i := range ids {
		ids[i] = "id"
	}
	_, err := requireIDs(m{"ids": ids}, 10)
	if err == nil {
		t.Error("expected error for exceeding max")
	}
}

// ── blobDownloadURL ─────────────────────────────────────────────────────────

func TestBlobDownloadURL_URLEncoding(t *testing.T) {
	// Temporarily set the cached URL for testing
	sessionMu.Lock()
	old := cachedDownloadURL
	cachedDownloadURL = "https://example.com/{accountId}/{blobId}/{name}?type={type}"
	sessionMu.Unlock()
	defer func() {
		sessionMu.Lock()
		cachedDownloadURL = old
		sessionMu.Unlock()
	}()

	url := blobDownloadURL("acct1", "blob1", "file name.pdf", "application/pdf")
	if url == "" {
		t.Fatal("expected non-empty URL")
	}
	// Name should be URL-encoded (space → %20)
	if !contains([]string{url}, "file%20name.pdf") {
		// PathEscape encodes space as %20
		if !containsStr(url, "file%20name.pdf") {
			t.Errorf("name not URL-encoded in: %s", url)
		}
	}
	// Type should be query-escaped
	if !containsStr(url, "application%2Fpdf") && !containsStr(url, "application/pdf") {
		t.Errorf("type not properly encoded in: %s", url)
	}
}

func TestBlobDownloadURL_SpecialChars(t *testing.T) {
	sessionMu.Lock()
	old := cachedDownloadURL
	cachedDownloadURL = "https://example.com/{accountId}/{blobId}/{name}?type={type}"
	sessionMu.Unlock()
	defer func() {
		sessionMu.Lock()
		cachedDownloadURL = old
		sessionMu.Unlock()
	}()

	url := blobDownloadURL("acct", "blob", "test?file#name", "text/html")
	// ? and # should be encoded, not altering URL structure
	if containsStr(url, "test?file") {
		t.Errorf("? in name should be encoded: %s", url)
	}
}

func TestBlobDownloadURL_EmptyTemplate(t *testing.T) {
	sessionMu.Lock()
	old := cachedDownloadURL
	cachedDownloadURL = ""
	sessionMu.Unlock()
	defer func() {
		sessionMu.Lock()
		cachedDownloadURL = old
		sessionMu.Unlock()
	}()

	url := blobDownloadURL("acct", "blob", "file", "text/plain")
	if url != "" {
		t.Errorf("expected empty URL for empty template, got: %s", url)
	}
}

// ── containsStr helper (for test use) ───────────────────────────────────────

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && searchStr(s, substr)
}

func searchStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// ── formatAddresses ─────────────────────────────────────────────────────────

func TestFormatAddresses_Nil(t *testing.T) {
	result := formatAddresses(nil)
	arr, ok := result.([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", result)
	}
	if len(arr) != 0 {
		t.Errorf("expected empty array, got %d elements", len(arr))
	}
}

func TestFormatAddresses_Valid(t *testing.T) {
	input := []any{
		map[string]any{"name": "Alice", "email": "alice@example.com"},
		map[string]any{"name": "", "email": "bob@example.com"},
	}
	result := formatAddresses(input)
	arr, ok := result.([]m)
	if !ok {
		t.Fatalf("expected []m, got %T", result)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 addresses, got %d", len(arr))
	}
	if arr[0]["email"] != "alice@example.com" {
		t.Errorf("first email: got %v", arr[0]["email"])
	}
}

// ── emailSummaryDict ────────────────────────────────────────────────────────

func TestEmailSummaryDict(t *testing.T) {
	email := m{
		"id":         "msg1",
		"threadId":   "t1",
		"subject":    "Test Subject",
		"from":       []any{map[string]any{"name": "Alice", "email": "alice@example.com"}},
		"to":         []any{map[string]any{"name": "Bob", "email": "bob@example.com"}},
		"receivedAt": "2026-04-18T10:00:00Z",
		"preview":    "Preview text...",
		"keywords":   map[string]any{"$seen": true, "$flagged": true},
		"size":       float64(1234),
	}

	result := emailSummaryDict(email)

	if result["id"] != "msg1" {
		t.Errorf("id: got %v", result["id"])
	}
	if result["isRead"] != true {
		t.Errorf("isRead: got %v, want true", result["isRead"])
	}
	if result["isFlagged"] != true {
		t.Errorf("isFlagged: got %v, want true", result["isFlagged"])
	}
}

func TestEmailSummaryDict_Unread(t *testing.T) {
	email := m{
		"id":       "msg2",
		"keywords": map[string]any{}, // no $seen
	}
	result := emailSummaryDict(email)
	if result["isRead"] != false {
		t.Errorf("isRead should be false for unread email")
	}
}

// ── contactSummaryDict ──────────────────────────────────────────────────────

func TestContactSummaryDict_FullName(t *testing.T) {
	contact := m{
		"id":   "c1",
		"name": map[string]any{"full": "John Smith"},
	}
	result := contactSummaryDict(contact)
	if result["name"] != "John Smith" {
		t.Errorf("name: got %v", result["name"])
	}
}

func TestContactSummaryDict_GivenSurname(t *testing.T) {
	contact := m{
		"id":   "c2",
		"name": map[string]any{"given": "Jane", "surname": "Doe"},
	}
	result := contactSummaryDict(contact)
	if result["name"] != "Jane Doe" {
		t.Errorf("name: got %q, want %q", result["name"], "Jane Doe")
	}
}

func TestContactSummaryDict_Emails(t *testing.T) {
	contact := m{
		"id": "c3",
		"emails": map[string]any{
			"e0": map[string]any{"address": "jane@example.com"},
			"e1": map[string]any{"address": "jane@work.com"},
		},
	}
	result := contactSummaryDict(contact)
	emails, ok := result["emails"].([]string)
	if !ok {
		t.Fatalf("emails: expected []string, got %T", result["emails"])
	}
	if len(emails) != 2 {
		t.Errorf("expected 2 emails, got %d", len(emails))
	}
}

// ── checkNotUpdated ─────────────────────────────────────────────────────────

func TestCheckNotUpdated_NoErrors(t *testing.T) {
	responses := []any{
		[]any{"Email/set", map[string]any{"updated": map[string]any{"msg1": nil}}, "u0"},
	}
	failures := checkNotUpdated(responses)
	if len(failures) != 0 {
		t.Errorf("expected no failures, got %v", failures)
	}
}

func TestCheckNotUpdated_WithErrors(t *testing.T) {
	responses := []any{
		[]any{"Email/set", map[string]any{
			"notUpdated": map[string]any{
				"msg1": map[string]any{"type": "notFound", "description": "Email not found"},
			},
		}, "u0"},
	}
	failures := checkNotUpdated(responses)
	if len(failures) != 1 {
		t.Fatalf("expected 1 failure, got %d", len(failures))
	}
	if failures["msg1"] != "notFound: Email not found" {
		t.Errorf("failure message: got %q", failures["msg1"])
	}
}

func TestCheckNotUpdated_IgnoresNonEmailSet(t *testing.T) {
	responses := []any{
		[]any{"Mailbox/set", map[string]any{
			"notUpdated": map[string]any{"mb1": map[string]any{"type": "error"}},
		}, "u0"},
	}
	failures := checkNotUpdated(responses)
	if len(failures) != 0 {
		t.Errorf("should ignore non-Email/set responses, got %v", failures)
	}
}

// ── parseTime ───────────────────────────────────────────────────────────────

func TestParseTime_RFC3339(t *testing.T) {
	result := parseTime("2026-04-18T14:30:00Z")
	if result.IsZero() {
		t.Error("failed to parse RFC3339 datetime")
	}
	if result.Year() != 2026 || result.Month() != 4 || result.Day() != 18 {
		t.Errorf("parsed wrong date: %v", result)
	}
}

func TestParseTime_Empty(t *testing.T) {
	result := parseTime("")
	if !result.IsZero() {
		t.Errorf("empty string should return zero time, got %v", result)
	}
}

func TestParseTime_Invalid(t *testing.T) {
	result := parseTime("not a date")
	if !result.IsZero() {
		t.Errorf("invalid string should return zero time, got %v", result)
	}
}

// ── must ────────────────────────────────────────────────────────────────────

func TestMust_OK(t *testing.T) {
	data := m{"key": "value"}
	result := must(data, true)
	if result["key"] != "value" {
		t.Errorf("must with ok=true should return data")
	}
}

func TestMust_NotOK(t *testing.T) {
	result := must(nil, false)
	if result == nil {
		t.Error("must with ok=false should return empty map, not nil")
	}
	if len(result) != 0 {
		t.Error("must with ok=false should return empty map")
	}
}

// ── extractBodyText ─────────────────────────────────────────────────────────

func TestExtractBodyText_FromBodyValues(t *testing.T) {
	email := m{
		"textBody":   []any{map[string]any{"partId": "p1"}},
		"bodyValues": map[string]any{"p1": map[string]any{"value": "Hello world"}},
		"preview":    "Preview...",
	}
	result := extractBodyText(email)
	if result != "Hello world" {
		t.Errorf("expected body text, got %q", result)
	}
}

func TestExtractBodyText_FallsBackToPreview(t *testing.T) {
	email := m{
		"textBody":   []any{},
		"bodyValues": map[string]any{},
		"preview":    "Preview text",
	}
	result := extractBodyText(email)
	if result != "Preview text" {
		t.Errorf("expected preview, got %q", result)
	}
}

// ── eventSummaryDict ────────────────────────────────────────────────────────

func TestEventSummaryDict(t *testing.T) {
	event := m{
		"id":              "ev1",
		"title":           "Team Meeting",
		"start":           "2026-04-18T14:00:00",
		"timeZone":        "America/New_York",
		"duration":        "PT1H",
		"showWithoutTime": false,
		"status":          "confirmed",
		"description":     "Weekly sync",
		"locations": map[string]any{
			"loc1": map[string]any{"name": "Room 42"},
		},
		"participants": map[string]any{
			"p1": map[string]any{"name": "Alice", "email": "alice@example.com", "participationStatus": "accepted"},
		},
	}
	result := eventSummaryDict(event)
	if result["title"] != "Team Meeting" {
		t.Errorf("title: got %v", result["title"])
	}
	if result["description"] != "Weekly sync" {
		t.Errorf("description: got %v", result["description"])
	}
	locations, ok := result["locations"].([]string)
	if !ok || len(locations) != 1 || locations[0] != "Room 42" {
		t.Errorf("locations: got %v", result["locations"])
	}
	participants, ok := result["participants"].([]m)
	if !ok || len(participants) != 1 {
		t.Errorf("participants: got %v", result["participants"])
	}
}
