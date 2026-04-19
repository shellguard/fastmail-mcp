package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

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

// checkNotUpdated checks Email/set responses for notUpdated entries.
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

// ── Bridge Subject Parsing ─────────────────────────────────────────────────

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

// must extracts data from respData, returning empty map on failure (for chaining).
func must(data m, ok bool) m {
	if !ok {
		return m{}
	}
	return data
}

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

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

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

// parseTime attempts to parse an ISO 8601 datetime, returning zero time on failure.
func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
