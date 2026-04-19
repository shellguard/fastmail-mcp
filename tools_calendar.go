package main

import (
	"fmt"
	"strings"
)

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
	// JSON Pointer escape (RFC 6901): ~ → ~0, / → ~1
	escapedID := strings.ReplaceAll(participantID, "~", "~0")
	escapedID = strings.ReplaceAll(escapedID, "/", "~1")

	update := m{
		"participants/" + escapedID + "/participationStatus": status,
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
