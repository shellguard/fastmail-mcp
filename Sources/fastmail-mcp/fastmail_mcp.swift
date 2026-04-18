import Foundation

// MARK: - MCP Protocol Types

private struct ToolDefinition {
    let name: String
    let description: String
    let inputSchema: [String: Any]
}

private enum MCPError: Error, CustomStringConvertible {
    case invalidRequest(String)
    case methodNotFound(String)
    case invalidParams(String)
    case toolNotFound(String)
    case toolError(String)
    case authError(String)

    var description: String {
        switch self {
        case .invalidRequest(let m): return m
        case .methodNotFound(let m): return m
        case .invalidParams(let m): return m
        case .toolNotFound(let n): return "Unknown tool: \(n)"
        case .toolError(let m): return m
        case .authError(let m): return m
        }
    }

    /// Standard JSON-RPC error codes
    var jsonRPCCode: Int {
        switch self {
        case .invalidRequest: return -32600
        case .methodNotFound: return -32601
        case .invalidParams: return -32602
        case .toolNotFound:  return -32601
        case .toolError:     return -32000
        case .authError:     return -32000
        }
    }
}

// MARK: - JMAP Session & HTTP Helpers

/// Cached session: apiUrl + per-capability accountId map + the raw primaryAccounts dict.
/// All access is serialized through sessionLock.
private let sessionLock = NSLock()
private var cachedSessionApiUrl: String?
private var cachedPrimaryAccounts: [String: String] = [:]  // capability → accountId
private var cachedFallbackAccountId: String?

private func bearerToken() throws -> String {
    guard let token = ProcessInfo.processInfo.environment["FASTMAIL_TOKEN"], !token.isEmpty else {
        throw MCPError.authError("FASTMAIL_TOKEN environment variable is not set")
    }
    return token
}

/// Discover the JMAP session. Caches the full session for the process lifetime.
/// Returns the apiUrl and the primary accountId for the requested capability.
private func sessionFor(using capabilities: [String]) throws -> (apiUrl: String, accountId: String) {
    sessionLock.lock()
    defer { sessionLock.unlock() }

    if cachedSessionApiUrl == nil {
        let token = try bearerToken()
        let url = URL(string: "https://api.fastmail.com/jmap/session")!
        var request = URLRequest(url: url)
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        request.timeoutInterval = 30

        let (data, response) = try syncHTTP(request)

        guard let http = response as? HTTPURLResponse else {
            throw MCPError.toolError("Session discovery: no HTTP response")
        }
        if http.statusCode == 401 {
            throw MCPError.authError("Invalid FASTMAIL_TOKEN (401 Unauthorized)")
        }
        guard http.statusCode == 200 else {
            throw MCPError.toolError("Session discovery failed: HTTP \(http.statusCode)")
        }

        guard let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let apiUrl = json["apiUrl"] as? String,
              let accounts = json["accounts"] as? [String: Any],
              let primaryAccounts = json["primaryAccounts"] as? [String: Any] else {
            throw MCPError.toolError("Session discovery: unexpected response format")
        }

        cachedSessionApiUrl = apiUrl
        // Cache all primaryAccount mappings
        for (cap, acctId) in primaryAccounts {
            if let id = acctId as? String {
                cachedPrimaryAccounts[cap] = id
            }
        }
        cachedFallbackAccountId = accounts.keys.first
    }

    guard let apiUrl = cachedSessionApiUrl else {
        throw MCPError.toolError("Session discovery: no cached session")
    }

    // Look up account for the most specific capability requested (skip urn:ietf:params:jmap:core)
    var accountId: String?
    for cap in capabilities where cap != "urn:ietf:params:jmap:core" {
        if let id = cachedPrimaryAccounts[cap] {
            accountId = id
            break
        }
    }
    // Fall back to core, then to any account
    if accountId == nil {
        accountId = cachedPrimaryAccounts["urn:ietf:params:jmap:core"] ?? cachedFallbackAccountId
    }
    guard let acctId = accountId else {
        throw MCPError.toolError("Session discovery: no accounts found")
    }

    return (apiUrl: apiUrl, accountId: acctId)
}

/// Make a JMAP method call. Returns the methodResponses array.
private func jmapCall(_ methodCalls: [[Any]], using capabilities: [String] = [
    "urn:ietf:params:jmap:core",
    "urn:ietf:params:jmap:mail"
]) throws -> [[Any]] {
    let token = try bearerToken()
    let session = try sessionFor(using: capabilities)

    let body: [String: Any] = [
        "using": capabilities,
        "methodCalls": methodCalls
    ]

    guard let bodyData = try? JSONSerialization.data(withJSONObject: body) else {
        throw MCPError.toolError("Failed to serialize JMAP request")
    }

    var request = URLRequest(url: URL(string: session.apiUrl)!)
    request.httpMethod = "POST"
    request.httpBody = bodyData
    request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
    request.setValue("application/json", forHTTPHeaderField: "Content-Type")
    request.timeoutInterval = 60

    let (data, response) = try syncHTTPWithRetry(request)

    guard let http = response as? HTTPURLResponse else {
        throw MCPError.toolError("JMAP call: no HTTP response")
    }
    guard http.statusCode == 200 else {
        let body = String(data: data, encoding: .utf8) ?? ""
        throw MCPError.toolError("JMAP call failed: HTTP \(http.statusCode) — \(body.prefix(500))")
    }

    guard let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
          let responses = json["methodResponses"] as? [[Any]] else {
        throw MCPError.toolError("JMAP call: unexpected response format")
    }

    // Check for error responses
    for resp in responses {
        if let methodName = resp.first as? String, methodName == "error" {
            if let errObj = resp.dropFirst().first as? [String: Any] {
                let errType = errObj["type"] as? String ?? "unknown"
                let errDesc = errObj["description"] as? String ?? ""
                throw MCPError.toolError("JMAP error (\(errType)): \(errDesc)")
            }
        }
    }

    return responses
}

/// Synchronous HTTP request using DispatchSemaphore.
private func syncHTTP(_ request: URLRequest) throws -> (Data, URLResponse) {
    let semaphore = DispatchSemaphore(value: 0)
    var resultData: Data?
    var resultResponse: URLResponse?
    var resultError: Error?

    let task = URLSession.shared.dataTask(with: request) { data, response, error in
        resultData = data
        resultResponse = response
        resultError = error
        semaphore.signal()
    }
    task.resume()

    if semaphore.wait(timeout: .now() + 90) == .timedOut {
        task.cancel()
        throw MCPError.toolError("HTTP request timed out")
    }

    if let error = resultError {
        throw MCPError.toolError("HTTP request failed: \(error.localizedDescription)")
    }
    guard let data = resultData, let response = resultResponse else {
        throw MCPError.toolError("HTTP request: no data/response")
    }
    return (data, response)
}

/// Synchronous HTTP with simple 429 retry.
private func syncHTTPWithRetry(_ request: URLRequest, maxRetries: Int = 2) throws -> (Data, URLResponse) {
    for attempt in 0...maxRetries {
        let (data, response) = try syncHTTP(request)
        if let http = response as? HTTPURLResponse, http.statusCode == 429 {
            if attempt < maxRetries {
                let retryAfter = Int(http.value(forHTTPHeaderField: "Retry-After") ?? "2") ?? 2
                let wait = min(retryAfter, 30) // cap at 30s
                fputs("fastmail-mcp: rate limited, retrying after \(wait)s\n", stderr)
                Thread.sleep(forTimeInterval: Double(wait))
                continue
            }
        }
        return (data, response)
    }
    throw MCPError.toolError("HTTP request failed after retries")
}

// MARK: - JMAP Helpers

private func accountId() throws -> String {
    return try sessionFor(using: [
        "urn:ietf:params:jmap:core",
        "urn:ietf:params:jmap:mail"
    ]).accountId
}

// contactsAccountId resolves via "https://www.fastmail.com/dev/contacts" capability.
// sessionFor now correctly prefers non-core capabilities for lookup.
private func contactsAccountId() throws -> String {
    return try sessionFor(using: [
        "urn:ietf:params:jmap:core",
        "https://www.fastmail.com/dev/contacts"
    ]).accountId
}

// MARK: - Tool Implementations

// ── Email Tools ──────────────────────────────────────────────────────────────

private func listMailboxes() throws -> Any {
    let acct = try accountId()
    let responses = try jmapCall([
        ["Mailbox/get", ["accountId": acct, "properties": ["id", "name", "role", "totalEmails", "unreadEmails", "parentId", "sortOrder"]], "m0"]
    ])

    guard let result = responses.first, result.count >= 2,
          let data = result[1] as? [String: Any],
          let list = data["list"] as? [[String: Any]] else {
        throw MCPError.toolError("Unexpected Mailbox/get response")
    }

    return list.map { mb -> [String: Any] in
        var d: [String: Any] = [
            "id": mb["id"] ?? "",
            "name": mb["name"] ?? "",
            "totalEmails": mb["totalEmails"] ?? 0,
            "unreadEmails": mb["unreadEmails"] ?? 0,
            "sortOrder": mb["sortOrder"] ?? 0
        ]
        if let role = mb["role"] as? String { d["role"] = role }
        if let parentId = mb["parentId"] as? String { d["parentId"] = parentId }
        return d
    }
}

private func listEmails(params: [String: Any]) throws -> Any {
    guard let mailboxId = params["mailboxId"] as? String, !mailboxId.isEmpty else {
        throw MCPError.invalidParams("mailboxId is required")
    }
    let limit = min(params["limit"] as? Int ?? 20, 200)
    let offset = params["offset"] as? Int ?? 0
    let onlyUnread = params["onlyUnread"] as? Bool ?? false

    let acct = try accountId()

    var filter: [String: Any] = ["inMailbox": mailboxId]
    if onlyUnread {
        filter["notKeyword"] = "$seen"
    }

    let queryArgs: [String: Any] = [
        "accountId": acct,
        "filter": filter,
        "sort": [["property": "receivedAt", "isAscending": false]],
        "position": offset,
        "limit": limit,
        "collapseThreads": false
    ]

    let responses = try jmapCall([
        ["Email/query", queryArgs, "q0"],
        ["Email/get", [
            "accountId": acct,
            "#ids": ["resultOf": "q0", "name": "Email/query", "path": "/ids"],
            "properties": ["id", "threadId", "mailboxIds", "from", "to", "subject",
                           "receivedAt", "preview", "keywords", "size"]
        ], "g0"]
    ])

    // Find Email/get response
    guard let getResp = responses.last, getResp.count >= 2,
          let getData = getResp[1] as? [String: Any],
          let emails = getData["list"] as? [[String: Any]] else {
        throw MCPError.toolError("Unexpected Email/query+get response")
    }

    // Also get total from query response
    var total: Int?
    if let queryResp = responses.first, queryResp.count >= 2,
       let queryData = queryResp[1] as? [String: Any] {
        total = queryData["total"] as? Int
    }

    let mapped = emails.map { emailSummaryDict($0) }
    var result: [String: Any] = ["emails": mapped, "offset": offset, "limit": limit]
    if let t = total { result["total"] = t }
    return result
}

private func getEmail(params: [String: Any]) throws -> Any {
    guard let emailId = params["id"] as? String, !emailId.isEmpty else {
        throw MCPError.invalidParams("id is required")
    }

    let acct = try accountId()
    let responses = try jmapCall([
        ["Email/get", [
            "accountId": acct,
            "ids": [emailId],
            "properties": ["id", "threadId", "mailboxIds", "from", "to", "cc", "bcc",
                           "replyTo", "subject", "receivedAt", "sentAt", "preview",
                           "keywords", "size", "textBody", "htmlBody", "attachments",
                           "bodyValues", "messageId", "inReplyTo", "references"],
            "fetchTextBodyValues": true,
            "fetchHTMLBodyValues": true
        ], "g0"]
    ])

    guard let resp = responses.first, resp.count >= 2,
          let data = resp[1] as? [String: Any] else {
        throw MCPError.toolError("Unexpected Email/get response")
    }

    // Check notFound first
    if let notFound = data["notFound"] as? [String], notFound.contains(emailId) {
        throw MCPError.invalidParams("Email not found: \(emailId)")
    }

    guard let list = data["list"] as? [[String: Any]], let email = list.first else {
        throw MCPError.toolError("Unexpected Email/get response")
    }

    return emailDetailDict(email)
}

private func searchEmails(params: [String: Any]) throws -> Any {
    guard let query = params["query"] as? String, !query.isEmpty else {
        throw MCPError.invalidParams("query is required")
    }
    let limit = min(params["limit"] as? Int ?? 20, 200)
    let mailboxId = params["mailboxId"] as? String

    let acct = try accountId()

    // Try to parse query as JSON filter, fall back to text filter
    var filter: [String: Any]
    if let queryData = query.data(using: .utf8),
       let parsed = try? JSONSerialization.jsonObject(with: queryData) as? [String: Any] {
        filter = parsed
    } else {
        filter = ["text": query]
    }

    if let mbId = mailboxId, !mbId.isEmpty {
        filter["inMailbox"] = mbId
    }

    let queryArgs: [String: Any] = [
        "accountId": acct,
        "filter": filter,
        "sort": [["property": "receivedAt", "isAscending": false]],
        "position": 0,
        "limit": limit,
        "collapseThreads": false
    ]

    let responses = try jmapCall([
        ["Email/query", queryArgs, "q0"],
        ["Email/get", [
            "accountId": acct,
            "#ids": ["resultOf": "q0", "name": "Email/query", "path": "/ids"],
            "properties": ["id", "threadId", "mailboxIds", "from", "to", "subject",
                           "receivedAt", "preview", "keywords", "size"]
        ], "g0"]
    ])

    guard let getResp = responses.last, getResp.count >= 2,
          let getData = getResp[1] as? [String: Any],
          let emails = getData["list"] as? [[String: Any]] else {
        throw MCPError.toolError("Unexpected search response")
    }

    var total: Int?
    if let queryResp = responses.first, queryResp.count >= 2,
       let queryData = queryResp[1] as? [String: Any] {
        total = queryData["total"] as? Int
    }

    let mapped = emails.map { emailSummaryDict($0) }
    var result: [String: Any] = ["emails": mapped, "limit": limit]
    if let t = total { result["total"] = t }
    return result
}

private func sendEmail(params: [String: Any]) throws -> Any {
    guard let toArr = params["to"] as? [[String: Any]], !toArr.isEmpty else {
        // Also accept simple string array
        if let toStrs = params["to"] as? [String], !toStrs.isEmpty {
            var adjusted = params
            adjusted["to"] = toStrs.map { ["email": $0] as [String: Any] }
            return try sendEmail(params: adjusted)
        }
        throw MCPError.invalidParams("to is required (array of {name?, email})")
    }
    guard let subject = params["subject"] as? String else {
        throw MCPError.invalidParams("subject is required")
    }
    guard let body = params["body"] as? String else {
        throw MCPError.invalidParams("body is required")
    }

    let acct = try accountId()

    // Get identities to find the sending from address
    let identityResponses = try jmapCall([
        ["Identity/get", ["accountId": acct, "properties": ["id", "name", "email"]], "i0"]
    ], using: [
        "urn:ietf:params:jmap:core",
        "urn:ietf:params:jmap:mail",
        "urn:ietf:params:jmap:submission"
    ])

    guard let idResp = identityResponses.first, idResp.count >= 2,
          let idData = idResp[1] as? [String: Any],
          let identities = idData["list"] as? [[String: Any]],
          let identity = identities.first else {
        throw MCPError.toolError("No sending identity found")
    }

    let identityId = identity["id"] as? String ?? ""

    // Build from address
    let fromAddr: [String: Any] = [
        "name": identity["name"] ?? "",
        "email": identity["email"] ?? ""
    ]

    // Build the email object
    var emailObj: [String: Any] = [
        "from": [fromAddr],
        "to": toArr,
        "subject": subject,
        "textBody": [["partId": "body", "type": "text/plain"]],
        "bodyValues": ["body": ["value": body, "isEncodingProblem": false, "isTruncated": false]],
        "keywords": ["$seen": true, "$draft": true],
        "mailboxIds": [:] as [String: Any]  // Will be set by submission
    ]

    // Optional cc
    if let ccArr = params["cc"] as? [[String: Any]] {
        emailObj["cc"] = ccArr
    } else if let ccStrs = params["cc"] as? [String] {
        emailObj["cc"] = ccStrs.map { ["email": $0] as [String: Any] }
    }

    // Optional replyTo headers
    if let replyToId = params["replyToId"] as? String, !replyToId.isEmpty {
        // Fetch the original email's messageId to set In-Reply-To and References
        let origResponses = try jmapCall([
            ["Email/get", [
                "accountId": acct,
                "ids": [replyToId],
                "properties": ["messageId", "references", "subject"]
            ], "orig0"]
        ])
        if let origResp = origResponses.first, origResp.count >= 2,
           let origData = origResp[1] as? [String: Any],
           let origList = origData["list"] as? [[String: Any]],
           let orig = origList.first {
            if let msgIds = orig["messageId"] as? [String], let msgId = msgIds.first {
                emailObj["inReplyTo"] = [msgId]
                var refs = orig["references"] as? [String] ?? []
                refs.append(msgId)
                emailObj["references"] = refs
            }
        }
    }

    // Find Drafts and Sent mailboxes
    let mbResponses = try jmapCall([
        ["Mailbox/get", [
            "accountId": acct,
            "properties": ["id", "name", "role"]
        ], "mg0"]
    ])
    var draftsId: String?
    var sentId: String?
    if let mbResp = mbResponses.first, mbResp.count >= 2,
       let mbData = mbResp[1] as? [String: Any],
       let mbList = mbData["list"] as? [[String: Any]] {
        for mb in mbList {
            let role = mb["role"] as? String
            let id = mb["id"] as? String
            if role == "drafts" { draftsId = id }
            if role == "sent" { sentId = id }
        }
    }
    guard let draftsMbId = draftsId else {
        throw MCPError.toolError("Could not find Drafts mailbox — required for sending")
    }
    emailObj["mailboxIds"] = [draftsMbId: true]

    // Create email and submit in one call.
    // Use JMAP creation references: "#emailId" with ResultReference pointing
    // to the created object's server-assigned id from Email/set.
    // On success, move from Drafts to Sent (or just remove Drafts keyword).
    let createId = "draft"
    var submissionArgs: [String: Any] = [
        "accountId": acct,
        "create": ["sub0": [
            "identityId": identityId,
            "#emailId": [
                "resultOf": "c0",
                "name": "Email/set",
                "path": "/created/\(createId)/id"
            ]
        ] as [String: Any]]
    ]
    // Move to Sent folder on success; fall back to destroying draft if no Sent folder
    if let sentMbId = sentId {
        submissionArgs["onSuccessUpdateEmail"] = [
            "#\(createId)": [
                "mailboxIds": [sentMbId: true],
                "keywords/$draft": nil
            ] as [String: Any?]
        ]
    } else {
        submissionArgs["onSuccessDestroyEmail"] = ["#\(createId)"]
    }

    let responses = try jmapCall([
        ["Email/set", [
            "accountId": acct,
            "create": [createId: emailObj]
        ], "c0"],
        ["EmailSubmission/set", submissionArgs, "s0"]
    ], using: [
        "urn:ietf:params:jmap:core",
        "urn:ietf:params:jmap:mail",
        "urn:ietf:params:jmap:submission"
    ])

    // Check for errors in creation
    if let setResp = responses.first, setResp.count >= 2,
       let setData = setResp[1] as? [String: Any],
       let notCreated = setData["notCreated"] as? [String: Any],
       let err = notCreated[createId] as? [String: Any] {
        let errType = err["type"] as? String ?? "unknown"
        let errDesc = err["description"] as? String ?? ""
        throw MCPError.toolError("Failed to create email: \(errType) — \(errDesc)")
    }

    // Check submission errors
    if responses.count > 1 {
        let subResp = responses[1]
        if subResp.count >= 2,
           let subData = subResp[1] as? [String: Any],
           let notCreated = subData["notCreated"] as? [String: Any],
           let err = notCreated["sub0"] as? [String: Any] {
            let errType = err["type"] as? String ?? "unknown"
            let errDesc = err["description"] as? String ?? ""
            throw MCPError.toolError("Failed to submit email: \(errType) — \(errDesc)")
        }
    }

    return ["status": "sent", "to": toArr, "subject": subject]
}

private func markRead(params: [String: Any]) throws -> Any {
    guard let ids = params["ids"] as? [String], !ids.isEmpty else {
        throw MCPError.invalidParams("ids is required (array of email IDs)")
    }
    let read = params["read"] as? Bool ?? true

    let acct = try accountId()

    var update: [String: Any] = [:]
    for id in ids {
        if read {
            update[id] = ["keywords/$seen": true]
        } else {
            update[id] = ["keywords/$seen": nil] as [String: Any?]
        }
    }

    let responses = try jmapCall([
        ["Email/set", ["accountId": acct, "update": update], "u0"]
    ])

    let failures = checkNotUpdated(responses)
    if !failures.isEmpty && failures.count == ids.count {
        throw MCPError.toolError("Failed to update all emails: \(failures)")
    }
    var result: [String: Any] = ["status": "ok", "ids": ids, "read": read]
    if !failures.isEmpty { result["failures"] = failures }
    return result
}

private func moveEmail(params: [String: Any]) throws -> Any {
    guard let ids = params["ids"] as? [String], !ids.isEmpty else {
        throw MCPError.invalidParams("ids is required (array of email IDs)")
    }
    guard let mailboxId = params["mailboxId"] as? String, !mailboxId.isEmpty else {
        throw MCPError.invalidParams("mailboxId is required")
    }

    let acct = try accountId()

    var update: [String: Any] = [:]
    for id in ids {
        update[id] = ["mailboxIds": [mailboxId: true]]
    }

    let responses = try jmapCall([
        ["Email/set", ["accountId": acct, "update": update], "u0"]
    ])

    let failures = checkNotUpdated(responses)
    if !failures.isEmpty && failures.count == ids.count {
        throw MCPError.toolError("Failed to move all emails: \(failures)")
    }
    var result: [String: Any] = ["status": "ok", "ids": ids, "mailboxId": mailboxId]
    if !failures.isEmpty { result["failures"] = failures }
    return result
}

private func deleteEmail(params: [String: Any]) throws -> Any {
    guard let ids = params["ids"] as? [String], !ids.isEmpty else {
        throw MCPError.invalidParams("ids is required (array of email IDs)")
    }

    let acct = try accountId()

    // Find Trash mailbox
    let mbResponses = try jmapCall([
        ["Mailbox/query", ["accountId": acct, "filter": ["role": "trash"]], "mq0"],
        ["Mailbox/get", [
            "accountId": acct,
            "#ids": ["resultOf": "mq0", "name": "Mailbox/query", "path": "/ids"],
            "properties": ["id"]
        ], "mg0"]
    ])

    guard let mbResp = mbResponses.last, mbResp.count >= 2,
          let mbData = mbResp[1] as? [String: Any],
          let mbList = mbData["list"] as? [[String: Any]],
          let trashMb = mbList.first,
          let trashId = trashMb["id"] as? String else {
        throw MCPError.toolError("Could not find Trash mailbox")
    }

    // Move to trash
    var update: [String: Any] = [:]
    for id in ids {
        update[id] = ["mailboxIds": [trashId: true]]
    }

    let responses = try jmapCall([
        ["Email/set", ["accountId": acct, "update": update], "u0"]
    ])

    let failures = checkNotUpdated(responses)
    if !failures.isEmpty && failures.count == ids.count {
        throw MCPError.toolError("Failed to delete all emails: \(failures)")
    }
    var result: [String: Any] = ["status": "ok", "ids": ids, "movedTo": "Trash"]
    if !failures.isEmpty { result["failures"] = failures }
    return result
}

// ── Bridge Inbox Tools ───────────────────────────────────────────────────────

private func listBridgeMessages(params: [String: Any]) throws -> Any {
    let bridgeName = params["mailboxName"] as? String ?? "Bridge"
    let acct = try accountId()

    // Find the bridge mailbox by name
    let mbResponses = try jmapCall([
        ["Mailbox/get", ["accountId": acct, "properties": ["id", "name", "role"]], "mb0"]
    ])

    guard let mbResp = mbResponses.first, mbResp.count >= 2,
          let mbData = mbResp[1] as? [String: Any],
          let mbList = mbData["list"] as? [[String: Any]] else {
        throw MCPError.toolError("Could not list mailboxes")
    }

    guard let bridgeMb = mbList.first(where: { ($0["name"] as? String) == bridgeName }) else {
        throw MCPError.toolError("Mailbox '\(bridgeName)' not found. Create it in Fastmail first.")
    }

    let bridgeId = bridgeMb["id"] as? String ?? ""

    // Query unread emails in bridge mailbox
    let responses = try jmapCall([
        ["Email/query", [
            "accountId": acct,
            "filter": ["inMailbox": bridgeId, "notKeyword": "$seen"] as [String: Any],
            "sort": [["property": "receivedAt", "isAscending": false]],
            "limit": 50
        ], "q0"],
        ["Email/get", [
            "accountId": acct,
            "#ids": ["resultOf": "q0", "name": "Email/query", "path": "/ids"],
            "properties": ["id", "threadId", "from", "subject", "receivedAt",
                           "textBody", "bodyValues", "preview"],
            "fetchTextBodyValues": true
        ], "g0"]
    ])

    guard let getResp = responses.last, getResp.count >= 2,
          let getData = getResp[1] as? [String: Any],
          let emails = getData["list"] as? [[String: Any]] else {
        throw MCPError.toolError("Unexpected bridge query response")
    }

    return emails.map { email -> [String: Any] in
        var d: [String: Any] = [
            "id": email["id"] ?? "",
            "from": formatAddresses(email["from"]),
            "receivedAt": email["receivedAt"] ?? "",
            "subject": email["subject"] ?? ""
        ]

        // Extract body text
        let bodyText = extractBodyText(email)
        d["body"] = bodyText

        // Parse structured type from subject: [TYPE] description
        let subject = email["subject"] as? String ?? ""
        if let match = parseBridgeSubject(subject) {
            d["bridgeType"] = match.type
            d["bridgeDescription"] = match.description
        }

        return d
    }
}

private func ackBridgeMessage(params: [String: Any]) throws -> Any {
    guard let ids = params["ids"] as? [String], !ids.isEmpty else {
        // Accept single id too
        if let id = params["id"] as? String, !id.isEmpty {
            return try ackBridgeMessage(params: ["ids": [id]])
        }
        throw MCPError.invalidParams("ids (or id) is required")
    }

    let bridgeName = params["mailboxName"] as? String ?? "Bridge"
    let processedName = params["processedMailboxName"] as? String ?? "Bridge/Processed"
    let acct = try accountId()

    // Find mailboxes
    let mbResponses = try jmapCall([
        ["Mailbox/get", ["accountId": acct, "properties": ["id", "name", "parentId"]], "mb0"]
    ])

    guard let mbResp = mbResponses.first, mbResp.count >= 2,
          let mbData = mbResp[1] as? [String: Any],
          let mbList = mbData["list"] as? [[String: Any]] else {
        throw MCPError.toolError("Could not list mailboxes")
    }

    // Find Processed mailbox (try "Processed" under Bridge parent, or exact name match)
    var processedId: String?

    // First try: find Bridge mailbox and look for Processed child
    if let bridgeMb = mbList.first(where: { ($0["name"] as? String) == bridgeName }) {
        let bridgeId = bridgeMb["id"] as? String ?? ""
        if let processed = mbList.first(where: {
            ($0["name"] as? String) == "Processed" && ($0["parentId"] as? String) == bridgeId
        }) {
            processedId = processed["id"] as? String
        }
    }

    // Second try: look for exact name match with slash
    if processedId == nil {
        if let processed = mbList.first(where: { ($0["name"] as? String) == processedName }) {
            processedId = processed["id"] as? String
        }
    }

    guard let destId = processedId else {
        throw MCPError.toolError("Mailbox '\(processedName)' not found. Create it as a subfolder of '\(bridgeName)' in Fastmail.")
    }

    // Mark read and move to Processed.
    // Use JMAP patch paths for both keywords and mailboxIds to avoid
    // mixing full-object replacement with patch-path keys.
    var update: [String: Any] = [:]
    for id in ids {
        update[id] = [
            "keywords/$seen": true,
            "mailboxIds": [destId: true]
        ] as [String: Any]
    }

    let responses = try jmapCall([
        ["Email/set", ["accountId": acct, "update": update], "u0"]
    ])

    let failures = checkNotUpdated(responses)
    if !failures.isEmpty && failures.count == ids.count {
        throw MCPError.toolError("Failed to acknowledge all messages: \(failures)")
    }
    var result: [String: Any] = ["status": "ok", "ids": ids, "movedTo": processedName]
    if !failures.isEmpty { result["failures"] = failures }
    return result
}

// ── Contacts Tools ───────────────────────────────────────────────────────────

private func listContacts(params: [String: Any]) throws -> Any {
    let limit = min(params["limit"] as? Int ?? 50, 200)
    let search = params["search"] as? String

    let caps = [
        "urn:ietf:params:jmap:core",
        "https://www.fastmail.com/dev/contacts"
    ]
    let acct = try sessionFor(using: caps).accountId

    var filter: [String: Any]? = nil
    if let s = search, !s.isEmpty {
        filter = ["text": s]
    }

    var queryArgs: [String: Any] = [
        "accountId": acct,
        "limit": limit
    ]
    if let f = filter {
        queryArgs["filter"] = f
    }

    let responses = try jmapCall([
        ["ContactCard/query", queryArgs, "q0"],
        ["ContactCard/get", [
            "accountId": acct,
            "#ids": ["resultOf": "q0", "name": "ContactCard/query", "path": "/ids"],
            "properties": ["id", "name", "emails", "phones", "online"]
        ], "g0"]
    ], using: caps)

    guard let getResp = responses.last, getResp.count >= 2,
          let getData = getResp[1] as? [String: Any],
          let contacts = getData["list"] as? [[String: Any]] else {
        throw MCPError.toolError("Unexpected contacts response")
    }

    return contacts.map { contactSummaryDict($0) }
}

private func getContact(params: [String: Any]) throws -> Any {
    guard let contactId = params["id"] as? String, !contactId.isEmpty else {
        throw MCPError.invalidParams("id is required")
    }

    let caps = [
        "urn:ietf:params:jmap:core",
        "https://www.fastmail.com/dev/contacts"
    ]
    let acct = try sessionFor(using: caps).accountId

    let responses = try jmapCall([
        ["ContactCard/get", [
            "accountId": acct,
            "ids": [contactId]
        ], "g0"]
    ], using: caps)

    guard let resp = responses.first, resp.count >= 2,
          let data = resp[1] as? [String: Any] else {
        throw MCPError.toolError("Unexpected ContactCard/get response")
    }

    if let notFound = data["notFound"] as? [String], notFound.contains(contactId) {
        throw MCPError.invalidParams("Contact not found: \(contactId)")
    }

    guard let list = data["list"] as? [[String: Any]], let contact = list.first else {
        throw MCPError.toolError("Unexpected ContactCard/get response")
    }

    return contactDetailDict(contact)
}

// ── Identity Tools ───────────────────────────────────────────────────────────

private func listIdentities() throws -> Any {
    let acct = try accountId()
    let responses = try jmapCall([
        ["Identity/get", ["accountId": acct], "i0"]
    ], using: [
        "urn:ietf:params:jmap:core",
        "urn:ietf:params:jmap:mail",
        "urn:ietf:params:jmap:submission"
    ])

    guard let resp = responses.first, resp.count >= 2,
          let data = resp[1] as? [String: Any],
          let list = data["list"] as? [[String: Any]] else {
        throw MCPError.toolError("Unexpected Identity/get response")
    }

    return list.map { id -> [String: Any] in
        [
            "id": id["id"] ?? "",
            "name": id["name"] ?? "",
            "email": id["email"] ?? "",
            "replyTo": id["replyTo"] ?? NSNull(),
            "bcc": id["bcc"] ?? NSNull(),
            "htmlSignature": id["htmlSignature"] ?? ""
        ]
    }
}

// MARK: - JMAP Response Helpers

/// Check Email/set response for notUpdated entries. Returns a dict of id → error description.
private func checkNotUpdated(_ responses: [[Any]]) -> [String: String] {
    var failures: [String: String] = [:]
    for resp in responses {
        guard resp.count >= 2,
              let methodName = resp.first as? String, methodName == "Email/set",
              let data = resp[1] as? [String: Any],
              let notUpdated = data["notUpdated"] as? [String: Any] else { continue }
        for (id, errObj) in notUpdated {
            if let err = errObj as? [String: Any] {
                let errType = err["type"] as? String ?? "unknown"
                let errDesc = err["description"] as? String ?? ""
                failures[id] = "\(errType): \(errDesc)"
            } else {
                failures[id] = "unknown error"
            }
        }
    }
    return failures
}

// MARK: - Serialization Helpers

private func emailSummaryDict(_ email: [String: Any]) -> [String: Any] {
    let keywords = email["keywords"] as? [String: Any] ?? [:]
    let isRead = keywords["$seen"] != nil
    let isFlagged = keywords["$flagged"] != nil

    return [
        "id": email["id"] ?? "",
        "threadId": email["threadId"] ?? "",
        "subject": email["subject"] ?? "",
        "from": formatAddresses(email["from"]),
        "to": formatAddresses(email["to"]),
        "receivedAt": email["receivedAt"] ?? "",
        "preview": email["preview"] ?? "",
        "isRead": isRead,
        "isFlagged": isFlagged,
        "size": email["size"] ?? 0
    ]
}

private func emailDetailDict(_ email: [String: Any]) -> [String: Any] {
    let keywords = email["keywords"] as? [String: Any] ?? [:]

    var d: [String: Any] = [
        "id": email["id"] ?? "",
        "threadId": email["threadId"] ?? "",
        "subject": email["subject"] ?? "",
        "from": formatAddresses(email["from"]),
        "to": formatAddresses(email["to"]),
        "cc": formatAddresses(email["cc"]),
        "receivedAt": email["receivedAt"] ?? "",
        "isRead": keywords["$seen"] != nil,
        "isFlagged": keywords["$flagged"] != nil
    ]

    if let sentAt = email["sentAt"] as? String { d["sentAt"] = sentAt }
    if let replyTo = email["replyTo"] { d["replyTo"] = formatAddresses(replyTo) }
    if let messageId = email["messageId"] as? [String] { d["messageId"] = messageId }
    if let inReplyTo = email["inReplyTo"] as? [String] { d["inReplyTo"] = inReplyTo }

    // Body text
    d["body"] = extractBodyText(email)

    // HTML body
    d["htmlBody"] = extractHTMLBody(email)

    // Attachments
    if let attachments = email["attachments"] as? [[String: Any]] {
        d["attachmentNames"] = attachments.compactMap { att -> String? in
            att["name"] as? String
        }
    }

    return d
}

private func contactSummaryDict(_ contact: [String: Any]) -> [String: Any] {
    var d: [String: Any] = ["id": contact["id"] ?? ""]

    // JSContact name object
    if let nameObj = contact["name"] as? [String: Any] {
        let full = nameObj["full"] as? String
        let given = nameObj["given"] as? String ?? ""
        let surname = nameObj["surname"] as? String ?? ""
        d["name"] = full ?? "\(given) \(surname)".trimmingCharacters(in: .whitespaces)
    }

    // Emails map
    if let emailsMap = contact["emails"] as? [String: Any] {
        d["emails"] = emailsMap.values.compactMap { entry -> String? in
            (entry as? [String: Any])?["address"] as? String
        }
    }

    return d
}

private func contactDetailDict(_ contact: [String: Any]) -> [String: Any] {
    var d = contactSummaryDict(contact)

    // Phones
    if let phonesMap = contact["phones"] as? [String: Any] {
        d["phones"] = phonesMap.values.compactMap { entry -> [String: Any]? in
            guard let e = entry as? [String: Any] else { return nil }
            return ["number": e["number"] ?? "", "label": e["label"] ?? ""]
        }
    }

    // Online (URLs, etc)
    if let onlineMap = contact["online"] as? [String: Any] {
        d["online"] = onlineMap.values.compactMap { entry -> [String: Any]? in
            guard let e = entry as? [String: Any] else { return nil }
            return ["resource": e["resource"] ?? "", "type": e["type"] ?? "", "label": e["label"] ?? ""]
        }
    }

    return d
}

private func formatAddresses(_ obj: Any?) -> Any {
    guard let arr = obj as? [[String: Any]] else { return [] }
    return arr.map { addr -> [String: Any] in
        [
            "name": addr["name"] ?? "",
            "email": addr["email"] ?? ""
        ]
    }
}

private func extractBodyText(_ email: [String: Any]) -> String {
    let bodyValues = email["bodyValues"] as? [String: Any] ?? [:]

    // Try textBody parts first
    if let textParts = email["textBody"] as? [[String: Any]] {
        for part in textParts {
            if let partId = part["partId"] as? String,
               let value = bodyValues[partId] as? [String: Any],
               let text = value["value"] as? String {
                return text
            }
        }
    }

    return email["preview"] as? String ?? ""
}

private func extractHTMLBody(_ email: [String: Any]) -> String {
    let bodyValues = email["bodyValues"] as? [String: Any] ?? [:]

    if let htmlParts = email["htmlBody"] as? [[String: Any]] {
        for part in htmlParts {
            if let partId = part["partId"] as? String,
               let value = bodyValues[partId] as? [String: Any],
               let text = value["value"] as? String {
                return text
            }
        }
    }

    return ""
}

private struct BridgeMatch {
    let type: String       // TASK, NOTE, EVENT
    let description: String
}

private let bridgeSubjectRegex: NSRegularExpression? = try? NSRegularExpression(pattern: #"^\[(\w+)\]\s+(.+)$"#)

private func parseBridgeSubject(_ subject: String) -> BridgeMatch? {
    guard let regex = bridgeSubjectRegex,
          let match = regex.firstMatch(in: subject, range: NSRange(subject.startIndex..., in: subject)) else {
        return nil
    }

    guard let typeRange = Range(match.range(at: 1), in: subject),
          let descRange = Range(match.range(at: 2), in: subject) else {
        return nil
    }

    let type = String(subject[typeRange]).uppercased()
    let desc = String(subject[descRange])

    // Only match known types
    guard ["TASK", "NOTE", "EVENT"].contains(type) else { return nil }

    return BridgeMatch(type: type, description: desc)
}

// MARK: - Tool Definitions

private let tools: [ToolDefinition] = [
    // ── Email ────────────────────────────────────────────────────────────────
    ToolDefinition(
        name: "fm_list_mailboxes",
        description: "List all Fastmail mailboxes with name, role, unread count, and total count.",
        inputSchema: ["type": "object", "properties": [:] as [String: Any], "required": []]
    ),
    ToolDefinition(
        name: "fm_list_emails",
        description: "List emails in a mailbox. Returns subject, from, date, preview, read/flagged status.",
        inputSchema: [
            "type": "object",
            "properties": [
                "mailboxId": ["type": "string", "description": "Mailbox ID (get from fm_list_mailboxes)"],
                "limit": ["type": "integer", "description": "Max emails to return (default 20)"],
                "offset": ["type": "integer", "description": "Offset for pagination (default 0)"],
                "onlyUnread": ["type": "boolean", "description": "Only return unread emails (default false)"]
            ] as [String: Any],
            "required": ["mailboxId"]
        ]
    ),
    ToolDefinition(
        name: "fm_get_email",
        description: "Get full email by ID. Returns subject, from, to, cc, date, text body, HTML body, and attachment names.",
        inputSchema: [
            "type": "object",
            "properties": ["id": ["type": "string", "description": "Email ID"]],
            "required": ["id"]
        ]
    ),
    ToolDefinition(
        name: "fm_search_emails",
        description: "Search emails across mailboxes. Query can be a plain text string or a JMAP filter object as JSON string (e.g. {\"from\":\"alice@example.com\", \"subject\":\"invoice\"}).",
        inputSchema: [
            "type": "object",
            "properties": [
                "query": ["type": "string", "description": "Search query (text or JSON JMAP filter)"],
                "mailboxId": ["type": "string", "description": "Optional: limit search to this mailbox"],
                "limit": ["type": "integer", "description": "Max results (default 20)"]
            ] as [String: Any],
            "required": ["query"]
        ]
    ),
    ToolDefinition(
        name: "fm_send_email",
        description: "Send an email. The 'to' field accepts [{name, email}] objects or plain email strings.",
        inputSchema: [
            "type": "object",
            "properties": [
                "to": ["type": "array", "description": "Recipients: [{name?, email}] or [\"email\"]", "items": [:] as [String: Any]],
                "subject": ["type": "string", "description": "Email subject"],
                "body": ["type": "string", "description": "Plain text email body"],
                "cc": ["type": "array", "description": "CC recipients (same format as to)", "items": [:] as [String: Any]],
                "replyToId": ["type": "string", "description": "Email ID to reply to (sets In-Reply-To/References headers)"]
            ] as [String: Any],
            "required": ["to", "subject", "body"]
        ]
    ),
    ToolDefinition(
        name: "fm_mark_read",
        description: "Mark email(s) as read or unread.",
        inputSchema: [
            "type": "object",
            "properties": [
                "ids": ["type": "array", "description": "Email IDs to update", "items": ["type": "string"]],
                "read": ["type": "boolean", "description": "true = mark read, false = mark unread (default true)"]
            ] as [String: Any],
            "required": ["ids"]
        ]
    ),
    ToolDefinition(
        name: "fm_move_email",
        description: "Move email(s) to a different mailbox.",
        inputSchema: [
            "type": "object",
            "properties": [
                "ids": ["type": "array", "description": "Email IDs to move", "items": ["type": "string"]],
                "mailboxId": ["type": "string", "description": "Destination mailbox ID"]
            ] as [String: Any],
            "required": ["ids", "mailboxId"]
        ]
    ),
    ToolDefinition(
        name: "fm_delete_email",
        description: "Move email(s) to Trash.",
        inputSchema: [
            "type": "object",
            "properties": [
                "ids": ["type": "array", "description": "Email IDs to trash", "items": ["type": "string"]]
            ] as [String: Any],
            "required": ["ids"]
        ]
    ),

    // ── Bridge Inbox ─────────────────────────────────────────────────────────
    ToolDefinition(
        name: "fm_list_bridge_messages",
        description: "List unread emails in the Bridge mailbox. Parses structured subjects like [TASK], [NOTE], [EVENT]. Returns bridgeType and bridgeDescription fields when subject matches convention.",
        inputSchema: [
            "type": "object",
            "properties": [
                "mailboxName": ["type": "string", "description": "Bridge mailbox name (default: 'Bridge')"]
            ] as [String: Any],
            "required": []
        ]
    ),
    ToolDefinition(
        name: "fm_ack_bridge_message",
        description: "Acknowledge a bridge message: mark as read and move to Bridge/Processed. Provide either 'ids' (array) or 'id' (single string).",
        inputSchema: [
            "type": "object",
            "properties": [
                "ids": ["type": "array", "description": "Email IDs to acknowledge", "items": ["type": "string"]],
                "id": ["type": "string", "description": "Single email ID (alternative to ids)"],
                "mailboxName": ["type": "string", "description": "Bridge mailbox name (default: 'Bridge')"],
                "processedMailboxName": ["type": "string", "description": "Processed subfolder name (default: 'Bridge/Processed')"]
            ] as [String: Any],
            "required": [],
            "anyOf": [
                ["required": ["ids"]],
                ["required": ["id"]]
            ] as [[String: Any]]
        ]
    ),

    // ── Contacts ─────────────────────────────────────────────────────────────
    ToolDefinition(
        name: "fm_list_contacts",
        description: "List contacts. Optionally search by name or email substring.",
        inputSchema: [
            "type": "object",
            "properties": [
                "limit": ["type": "integer", "description": "Max contacts to return (default 50)"],
                "search": ["type": "string", "description": "Search by name or email"]
            ] as [String: Any],
            "required": []
        ]
    ),
    ToolDefinition(
        name: "fm_get_contact",
        description: "Get a contact by ID with full details (name, emails, phones, online).",
        inputSchema: [
            "type": "object",
            "properties": ["id": ["type": "string", "description": "Contact ID"]],
            "required": ["id"]
        ]
    ),

    // ── Identity ─────────────────────────────────────────────────────────────
    ToolDefinition(
        name: "fm_list_identities",
        description: "List sending identities (email addresses available for sending).",
        inputSchema: ["type": "object", "properties": [:] as [String: Any], "required": []]
    ),
]

// MARK: - Tool Dispatch

private func callTool(name: String, arguments: [String: Any]) throws -> Any {
    switch name {
    case "fm_list_mailboxes":       return try listMailboxes()
    case "fm_list_emails":          return try listEmails(params: arguments)
    case "fm_get_email":            return try getEmail(params: arguments)
    case "fm_search_emails":        return try searchEmails(params: arguments)
    case "fm_send_email":           return try sendEmail(params: arguments)
    case "fm_mark_read":            return try markRead(params: arguments)
    case "fm_move_email":           return try moveEmail(params: arguments)
    case "fm_delete_email":         return try deleteEmail(params: arguments)
    case "fm_list_bridge_messages": return try listBridgeMessages(params: arguments)
    case "fm_ack_bridge_message":   return try ackBridgeMessage(params: arguments)
    case "fm_list_contacts":        return try listContacts(params: arguments)
    case "fm_get_contact":          return try getContact(params: arguments)
    case "fm_list_identities":      return try listIdentities()
    default: throw MCPError.toolNotFound(name)
    }
}

// MARK: - MCP Server

private class MCPServer {
    private static let maxInputBytes = 10 * 1024 * 1024

    func run() {
        while let line = readLine(strippingNewline: true) {
            guard !line.trimmingCharacters(in: .whitespaces).isEmpty else { continue }
            guard line.utf8.count <= MCPServer.maxInputBytes else {
                fputs("fastmail-mcp: oversized input (\(line.utf8.count) bytes), skipping\n", stderr)
                continue
            }
            guard let data = line.data(using: .utf8),
                  let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
                continue
            }
            handleMessage(json)
        }
    }

    private func handleMessage(_ msg: [String: Any]) {
        let id = msg["id"]
        let method = msg["method"] as? String ?? ""

        do {
            switch method {
            case "initialize":
                send(id: id, result: [
                    "protocolVersion": "2024-11-05",
                    "capabilities": ["tools": ["listChanged": false]],
                    "serverInfo": ["name": "fastmail-mcp", "version": "1.0.0"]
                ])

            case "notifications/initialized":
                break

            case "tools/list":
                let toolList = tools.map { t -> [String: Any] in
                    ["name": t.name, "description": t.description, "inputSchema": t.inputSchema]
                }
                send(id: id, result: ["tools": toolList])

            case "tools/call":
                let params = msg["params"] as? [String: Any] ?? [:]
                guard let toolName = params["name"] as? String else {
                    throw MCPError.invalidParams("Tool name missing")
                }
                let arguments = params["arguments"] as? [String: Any] ?? [:]
                let result = try callTool(name: toolName, arguments: arguments)
                let content: [[String: Any]]
                if let jsonData = try? JSONSerialization.data(withJSONObject: result, options: [.prettyPrinted]),
                   let jsonStr = String(data: jsonData, encoding: .utf8) {
                    content = [["type": "text", "text": jsonStr]]
                } else {
                    content = [["type": "text", "text": "\(result)"]]
                }
                send(id: id, result: ["content": content, "isError": false])

            case "ping":
                send(id: id, result: [:])

            default:
                throw MCPError.methodNotFound("Method not found: \(method)")
            }
        } catch let e as MCPError {
            sendError(id: id, code: e.jsonRPCCode, message: e.description)
        } catch {
            sendError(id: id, code: -32000, message: error.localizedDescription)
        }
    }

    private func send(id: Any?, result: [String: Any]) {
        guard let id = id else { return }
        let response: [String: Any] = ["jsonrpc": "2.0", "id": id, "result": result]
        emit(response)
    }

    private func sendError(id: Any?, code: Int, message: String) {
        // JSON-RPC: notifications (no id) must not receive responses
        guard id != nil else { return }
        let response: [String: Any] = [
            "jsonrpc": "2.0",
            "id": id!,
            "error": ["code": code, "message": message]
        ]
        emit(response)
    }

    private func emit(_ obj: [String: Any]) {
        if let data = try? JSONSerialization.data(withJSONObject: obj),
           let str = String(data: data, encoding: .utf8) {
            print(str); fflush(stdout); return
        }
        let id = obj["id"]
        let idLit: String
        if id == nil || id is NSNull { idLit = "null" }
        else if let n = id as? Int { idLit = "\(n)" }
        else if let s = id as? String {
            let escaped = s
                .replacingOccurrences(of: "\\", with: "\\\\")
                .replacingOccurrences(of: "\"", with: "\\\"")
                .replacingOccurrences(of: "\n", with: "\\n")
                .replacingOccurrences(of: "\r", with: "\\r")
                .replacingOccurrences(of: "\t", with: "\\t")
            idLit = "\"\(escaped)\""
        }
        else { idLit = "null" }
        print("{\"jsonrpc\":\"2.0\",\"id\":\(idLit),\"error\":{\"code\":-32700,\"message\":\"Response serialization failed\"}}")
        fflush(stdout)
    }
}

// MARK: - Entry Point

private let server = MCPServer()
DispatchQueue.global(qos: .userInitiated).async {
    server.run()
    fflush(stdout)
    exit(0)
}
RunLoop.main.run()
