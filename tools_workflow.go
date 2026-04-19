package main

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// isSafeExternalURL validates that a URL points to a public internet host,
// blocking SSRF to localhost, private networks, link-local, and cloud metadata IPs.
func isSafeExternalURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %v", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("only HTTPS URLs are allowed")
	}
	host := u.Hostname()
	// Block obvious names
	if host == "localhost" || host == "" {
		return fmt.Errorf("localhost URLs are not allowed")
	}
	// Resolve and check IP
	ips, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("cannot resolve host %q: %v", host, err)
	}
	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("URL resolves to non-public IP %s", ipStr)
		}
		// Block cloud metadata IPs (AWS, GCP, Azure)
		if ipStr == "169.254.169.254" || ipStr == "fd00:ec2::254" {
			return fmt.Errorf("URL resolves to cloud metadata IP %s", ipStr)
		}
	}
	return nil
}

// ── Agentic Workflow Tools ───────────────────────────────────────────────────

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

	// Validate URL is safe (not internal/private/metadata — SSRF prevention)
	if err := isSafeExternalURL(unsubURL); err != nil {
		return nil, errToolError(fmt.Sprintf("Refusing to POST to unsubscribe URL: %v", err))
	}

	// Perform the RFC 8058 one-click POST
	req, _ := http.NewRequest("POST", unsubURL, strings.NewReader("List-Unsubscribe=One-Click"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	_, statusCode, err := doHTTPWithRetry(req, 1)
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
		// Only return status code — never include response body (could leak internal data)
		result["message"] = fmt.Sprintf("Server returned HTTP %d — unsubscribe may not have succeeded", statusCode)
	}
	return result, nil
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
