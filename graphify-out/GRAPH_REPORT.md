# Graph Report - .  (2026-08-20)

## Corpus Check
- Corpus is ~7,533 words - fits in a single context window. You may not need a graph.

## Summary
- 128 nodes · 213 edges · 9 communities
- Extraction: 87% EXTRACTED · 13% INFERRED · 0% AMBIGUOUS · INFERRED: 27 edges (avg confidence: 0.79)
- Token cost: 0 input · 0 output

## Community Hubs (Navigation)
- [[_COMMUNITY_Community 0|Community 0]]
- [[_COMMUNITY_Community 1|Community 1]]
- [[_COMMUNITY_Community 2|Community 2]]
- [[_COMMUNITY_Community 3|Community 3]]
- [[_COMMUNITY_Community 4|Community 4]]
- [[_COMMUNITY_Community 5|Community 5]]
- [[_COMMUNITY_Community 6|Community 6]]
- [[_COMMUNITY_Community 7|Community 7]]
- [[_COMMUNITY_Community 8|Community 8]]

## God Nodes (most connected - your core abstractions)
1. `newClient()` - 11 edges
2. `Server` - 10 edges
3. `Service` - 10 edges
4. `writeJSON()` - 9 edges
5. `runAsk()` - 7 edges
6. `Load()` - 7 edges
7. `main()` - 6 edges
8. `runChat()` - 5 edges
9. `Config` - 5 edges
10. `env()` - 5 edges

## Surprising Connections (you probably didn't know these)
- `main()` --calls--> `newClient()`  [INFERRED]
  main.go → cmd/copilot-cli/main.go
- `runAsk()` --calls--> `New()`  [INFERRED]
  cmd/copilot-cli/main.go → internal/api/server.go
- `main()` --calls--> `Load()`  [INFERRED]
  cmd/copilotd/main.go → internal/config/config.go
- `main()` --calls--> `NewService()`  [INFERRED]
  cmd/copilotd/main.go → internal/runtime/session.go
- `main()` --calls--> `New()`  [INFERRED]
  cmd/copilotd/main.go → internal/api/server.go

## Communities (9 total, 0 thin omitted)

### Community 0 - "Community 0"
Cohesion: 0.00
Nodes (10): New(), main(), Event, NewPool(), Request, Service, modelLabel(), NewService() (+2 more)

### Community 1 - "Community 1"
Cohesion: 0.00
Nodes (8): errorBody, contextWithTimeout(), startNDJSON(), generateRequest, Server, decode(), modelOrDefault(), writeJSON()

### Community 2 - "Community 2"
Cohesion: 0.00
Nodes (16): main(), newClient(), newSession(), promptWithStdin(), resolveCLIPath(), resolveToken(), runAsk(), runChat() (+8 more)

### Community 3 - "Community 3"
Cohesion: 0.00
Nodes (20): Agent Session Handler, Built-in Web Tools, BYOK Model Provider, Caller Backend / Python Service, CLI Streaming Helper, copilot-cli, Copilot CLI Runtime, copilotd HTTP Server (+12 more)

### Community 4 - "Community 4"
Cohesion: 0.00
Nodes (6): account, Lease, Pool, connection(), isConnectionError(), isRateLimit()

### Community 5 - "Community 5"
Cohesion: 0.00
Nodes (7): Config, env(), envInt(), envOr(), firstEnv(), Load(), splitList()

### Community 6 - "Community 6"
Cohesion: 0.00
Nodes (4): callLog, ToolCall, buildTools(), decodeArgs()

### Community 7 - "Community 7"
Cohesion: 0.00
Nodes (4): firstNonEmpty(), agentRequest, toolCallbackRequest, toolCallbackResponse

### Community 8 - "Community 8"
Cohesion: 0.00
Nodes (3): main(), run(), truncate()

## Knowledge Gaps
- **10 isolated node(s):** `options`, `agentRequest`, `toolCallbackRequest`, `toolCallbackResponse`, `errorBody` (+5 more)
  These have ≤1 connection - possible missing edges or undocumented components.