package ai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"time"

	"errors"
	"github.com/coder/websocket"
	"sync"
)

const (
	codexWebSocketBeta      = "responses_websockets=2026-02-06"
	codexWebSocketReadLimit = 16 << 20
)

func streamCodexWebSocket(
	ctx context.Context,
	client *http.Client,
	model Model,
	opts *StreamOptions,
) iter.Seq2[*StreamEvent, error] {
	return func(yield func(*StreamEvent, error) bool) {
		streamCtx, cancel := codexStreamContext(ctx, opts)
		defer cancel()

		payload, err := buildCodexWebSocketPayload(model, opts)
		if err != nil {
			yield(nil, err)
			return
		}

		wsURL, err := resolveCodexWebSocketURL(model.BaseURL)
		if err != nil {
			yield(nil, err)
			return
		}

		dial := func() (*websocket.Conn, error) {
			conn, dialErr := dialCodexWebSocket(streamCtx, client, wsURL, model, opts, yield)
			return conn, dialErr
		}
		conn, entry, release, err := acquireCodexWebSocket(dial, opts.SessionID)
		if err != nil {
			if err == errCodexDialHandled {
				return // dial loop already yielded a terminal event
			}
			yield(nil, err)
			return
		}
		keep := true
		defer func() { release(keep) }()

		// pi uses connection-scoped cached context for both websocket-cached
		// and auto transports (openai-codex-responses.ts:1201).
		useCachedContext := opts.Transport == TransportWebSocketCached ||
			opts.Transport == TransportAuto || opts.Transport == ""
		requestPayload := payload
		if useCachedContext && entry != nil {
			requestPayload = buildCachedCodexWSRequest(entry, payload)
		}
		data, err := json.Marshal(requestPayload)
		if err != nil {
			keep = false
			yield(nil, fmt.Errorf("marshal codex websocket request: %w", err))
			return
		}

		if err := conn.Write(streamCtx, websocket.MessageText, data); err != nil {
			keep = false
			if entry != nil {
				entry.continuation = nil
			}
			if streamCtx.Err() != nil {
				yield(codexAbortedEvent(model, streamCtx.Err()), nil)
				return
			}
			yield(nil, err)
			return
		}

		var final *Message
		events := streamCodexWebSocketFrames(streamCtx, conn)
		for event, err := range streamCodexEvents(streamCtx, model, events) {
			if err != nil {
				keep = false
				if entry != nil {
					entry.continuation = nil
				}
				yield(nil, err)
				return
			}
			if event.Type == EventMessageComplete {
				final = event.Message
			}
			if !yield(event, nil) {
				keep = false
				if entry != nil {
					entry.continuation = nil
				}
				return
			}
		}

		// pi drops the connection on abort and records continuation state only
		// for a completed response with an id (openai-codex-responses.ts:1243-1257).
		switch {
		case streamCtx.Err() != nil:
			keep = false
		case final == nil || final.ResponseID == "" ||
			final.StopReason == StopReasonError || final.StopReason == StopReasonAborted:
			if entry != nil {
				entry.continuation = nil
			}
			keep = final != nil && final.StopReason != StopReasonError && final.StopReason != StopReasonAborted
		case useCachedContext && entry != nil:
			entry.continuation = &codexWSContinuation{
				lastBodyJSON:      codexWSBodyKey(payload),
				lastInput:         codexWSInputItems(payload),
				lastResponseItems: codexContinuationItems(model, final),
				lastResponseID:    final.ResponseID,
			}
		}
	}
}

// errCodexDialHandled signals that the dial loop already yielded a terminal
// event (abort, OnResponse rejection) and the stream must end silently.
var errCodexDialHandled = errors.New("codex websocket dial handled")

// dialCodexWebSocket runs the retrying dial loop. It returns
// errCodexDialHandled when it already yielded a terminal event.
func dialCodexWebSocket(
	streamCtx context.Context,
	client *http.Client,
	wsURL string,
	model Model,
	opts *StreamOptions,
	yield func(*StreamEvent, error) bool,
) (*websocket.Conn, error) {
	var conn *websocket.Conn
	var resp *http.Response
	var err error
	attempts := codexMaxAttempts(opts)
	for attempt := range attempts {
		conn, resp, err = websocket.Dial(streamCtx, wsURL, &websocket.DialOptions{
			HTTPClient: client,
			HTTPHeader: buildCodexWebSocketHeaders(model, opts),
		})
		if err == nil {
			break
		}
		if opts.OnResponse != nil && resp != nil {
			if responseErr := opts.OnResponse(providerResponseFromHTTP(resp), model); responseErr != nil {
				if resp.Body != nil {
					resp.Body.Close()
				}
				yield(nil, responseErr)
				return nil, errCodexDialHandled
			}
		}
		if streamCtx.Err() != nil {
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			yield(codexAbortedEvent(model, streamCtx.Err()), nil)
			return nil, errCodexDialHandled
		}
		retryable := resp != nil && isRetryableCodexStatus(resp.StatusCode)
		if retryable && attempt+1 < attempts {
			delay := codexRetryDelay(resp.Header, attempt)
			if resp.Body != nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
				resp.Body.Close()
			}
			if err := waitCodexRetry(streamCtx, delay); err != nil {
				yield(codexAbortedEvent(model, err), nil)
				return nil, errCodexDialHandled
			}
		}
		if !retryable || attempt+1 == attempts {
			break
		}
	}
	if err != nil {
		// A handshake rejected with an HTTP status is a transport failure: pi
		// catches it and falls back to SSE rather than surfacing a terminal
		// API error (openai-codex-responses.ts:208-228).
		if resp != nil {
			if resp.StatusCode >= http.StatusBadRequest {
				err = fmt.Errorf("codex websocket handshake rejected (%d): %w", resp.StatusCode, err)
			}
			if resp.Body != nil {
				resp.Body.Close()
			}
		}
		return nil, err
	}
	conn.SetReadLimit(codexWebSocketReadLimit)

	if opts.OnResponse != nil && resp != nil {
		if err := opts.OnResponse(providerResponseFromHTTP(resp), model); err != nil {
			_ = conn.Close(websocket.StatusNormalClosure, "rejected")
			yield(nil, err)
			return nil, errCodexDialHandled
		}
	}
	return conn, nil
}

func buildCodexWebSocketPayload(model Model, opts *StreamOptions) (map[string]any, error) {
	payload := any(buildCodexRequestBody(model, opts))
	if opts.OnPayload != nil {
		nextPayload, changed, err := opts.OnPayload(payload, model)
		if err != nil {
			return nil, err
		}
		if changed {
			payload = nextPayload
		}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal codex websocket payload: %w", err)
	}

	var request map[string]any
	if err := json.Unmarshal(data, &request); err != nil {
		return nil, fmt.Errorf("decode codex websocket payload: %w", err)
	}
	if request == nil {
		return nil, fmt.Errorf("codex websocket payload must be a JSON object")
	}
	request["type"] = "response.create"
	return request, nil
}

func buildCodexWebSocketHeaders(model Model, opts *StreamOptions) http.Header {
	headers := buildCodexBaseHeaders(model, opts)
	headers.Del("accept")
	headers.Del("content-type")
	headers.Del("OpenAI-Beta")
	headers.Set("OpenAI-Beta", codexWebSocketBeta)
	requestID := opts.SessionID
	if requestID == "" {
		requestID = createCodexRequestID()
	}
	headers.Set("session_id", requestID)
	headers.Set("x-client-request-id", requestID)
	return headers
}

func streamCodexWebSocketFrames(
	ctx context.Context,
	conn *websocket.Conn,
) iter.Seq2[codexStreamEvent, error] {
	return func(yield func(codexStreamEvent, error) bool) {
		sawTerminal := false
		for {
			messageType, data, err := conn.Read(ctx)
			if err != nil {
				if ctx.Err() != nil {
					yield(codexStreamEvent{}, err)
					return
				}
				if status := websocket.CloseStatus(err); status == websocket.StatusNormalClosure ||
					status == websocket.StatusGoingAway {
					if !sawTerminal {
						yield(codexStreamEvent{}, fmt.Errorf("codex websocket: closed before response completed"))
					}
					return
				}
				yield(codexStreamEvent{}, err)
				return
			}
			if messageType != websocket.MessageText {
				yield(codexStreamEvent{}, fmt.Errorf("codex websocket: unexpected message type %v", messageType))
				return
			}

			event, ok, err := decodeCodexStreamData(data)
			if err != nil {
				yield(codexStreamEvent{}, err)
				return
			}
			if ok {
				sawTerminal = isCodexTerminalEvent(event.Type)
				if !yield(event, nil) {
					return
				}
				if sawTerminal {
					return
				}
			}
		}
	}
}

// --- session-scoped WebSocket connection cache -----------------------------
//
// pi caches one WebSocket per session with a 5-minute idle TTL and reuses it
// for follow-up requests; on reuse it sends only the input delta plus
// previous_response_id from connection-scoped continuation state
// (openai-codex-responses.ts:611,629-634,802-811,875-960,1131-1170,1199-1262).
// pi registers a process-level session cleanup; along relies on the idle TTL
// and process exit.

const codexWSCacheTTL = 5 * time.Minute

type codexWSContinuation struct {
	// lastBodyJSON is the request body minus type/input/previous_response_id,
	// pi's requestBodiesMatchExceptInput key (openai-codex-responses.ts:1127-1129).
	lastBodyJSON      string
	lastInput         []any
	lastResponseItems []any
	lastResponseID    string
}

type codexWSCacheEntry struct {
	conn         *websocket.Conn
	busy         bool
	idleTimer    *time.Timer
	continuation *codexWSContinuation
}

var (
	codexWSCacheMu sync.Mutex
	codexWSCache   = map[string]*codexWSCacheEntry{}
)

// acquireCodexWebSocket mirrors pi's acquireWebSocket: reuse the session's
// idle cached connection, dial an uncached one while the cached one is busy,
// otherwise dial and cache. release(keep) either returns the connection to
// the cache with a fresh idle timer or closes and evicts it
// (openai-codex-responses.ts:875-960).
func acquireCodexWebSocket(
	dial func() (*websocket.Conn, error),
	sessionID string,
) (*websocket.Conn, *codexWSCacheEntry, func(keep bool), error) {
	if sessionID == "" {
		conn, err := dial()
		if err != nil {
			return nil, nil, nil, err
		}
		release := func(bool) { _ = conn.Close(websocket.StatusNormalClosure, "done") }
		return conn, nil, release, nil
	}

	codexWSCacheMu.Lock()
	if entry, ok := codexWSCache[sessionID]; ok {
		if entry.idleTimer != nil {
			entry.idleTimer.Stop()
			entry.idleTimer = nil
		}
		if !entry.busy {
			entry.busy = true
			codexWSCacheMu.Unlock()
			return entry.conn, entry, releaseCodexWSEntry(sessionID, entry), nil
		}
		// Busy: dial a fresh, uncached connection (pi ts:924-933).
		codexWSCacheMu.Unlock()
		conn, err := dial()
		if err != nil {
			return nil, nil, nil, err
		}
		release := func(bool) { _ = conn.Close(websocket.StatusNormalClosure, "done") }
		return conn, nil, release, nil
	}
	codexWSCacheMu.Unlock()

	conn, err := dial()
	if err != nil {
		return nil, nil, nil, err
	}
	entry := &codexWSCacheEntry{conn: conn, busy: true}
	codexWSCacheMu.Lock()
	codexWSCache[sessionID] = entry
	codexWSCacheMu.Unlock()
	return conn, entry, releaseCodexWSEntry(sessionID, entry), nil
}

func releaseCodexWSEntry(sessionID string, entry *codexWSCacheEntry) func(keep bool) {
	return func(keep bool) {
		codexWSCacheMu.Lock()
		defer codexWSCacheMu.Unlock()
		if !keep {
			_ = entry.conn.Close(websocket.StatusNormalClosure, "done")
			if entry.idleTimer != nil {
				entry.idleTimer.Stop()
			}
			if codexWSCache[sessionID] == entry {
				delete(codexWSCache, sessionID)
			}
			return
		}
		entry.busy = false
		if entry.idleTimer != nil {
			entry.idleTimer.Stop()
		}
		entry.idleTimer = time.AfterFunc(codexWSCacheTTL, func() {
			codexWSCacheMu.Lock()
			defer codexWSCacheMu.Unlock()
			if entry.busy {
				return
			}
			_ = entry.conn.Close(websocket.StatusNormalClosure, "idle_timeout")
			if codexWSCache[sessionID] == entry {
				delete(codexWSCache, sessionID)
			}
		})
	}
}

// codexWSBodyKey marshals the payload minus type/input/previous_response_id.
func codexWSBodyKey(payload map[string]any) string {
	trimmed := make(map[string]any, len(payload))
	for key, value := range payload {
		switch key {
		case "type", "input", "previous_response_id":
			continue
		}
		trimmed[key] = value
	}
	data, err := json.Marshal(trimmed)
	if err != nil {
		return ""
	}
	return string(data)
}

func codexWSInputItems(payload map[string]any) []any {
	items, _ := payload["input"].([]any)
	return items
}

func jsonEqualSlices(a, b []any) bool {
	aJSON, errA := json.Marshal(a)
	bJSON, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(aJSON) == string(bJSON)
}

// buildCachedCodexWSRequest mirrors pi's buildCachedWebSocketRequestBody: when
// the new request matches the continuation baseline (same body except input,
// input extends lastInput+lastResponseItems), send only the suffix plus
// previous_response_id; otherwise clear the continuation and send everything
// (openai-codex-responses.ts:1131-1170).
func buildCachedCodexWSRequest(entry *codexWSCacheEntry, payload map[string]any) map[string]any {
	continuation := entry.continuation
	if continuation == nil {
		return payload
	}

	if continuation.lastResponseID == "" || codexWSBodyKey(payload) != continuation.lastBodyJSON {
		entry.continuation = nil
		return payload
	}
	currentInput := codexWSInputItems(payload)
	baseline := append(append([]any{}, continuation.lastInput...), continuation.lastResponseItems...)
	if len(currentInput) < len(baseline) || !jsonEqualSlices(currentInput[:len(baseline)], baseline) {
		entry.continuation = nil
		return payload
	}

	delta := make(map[string]any, len(payload)+1)
	for key, value := range payload {
		delta[key] = value
	}
	delta["previous_response_id"] = continuation.lastResponseID
	delta["input"] = currentInput[len(baseline):]
	return delta
}

// codexContinuationItems converts the finished assistant message into the
// response items the next request's history will contain, excluding
// function_call_output — pi's convertResponsesMessages([output]) filter
// (openai-codex-responses.ts:1245-1248).
func codexContinuationItems(model Model, output *Message) []any {
	transformed := TransformMessages([]Message{*output}, model, normalizeOpenAIToolCallID)
	items := convertCodexMessages(transformed, model)

	// Round-trip to plain JSON values so baseline comparison sees the same
	// shapes as the payload map.
	data, err := json.Marshal(items)
	if err != nil {
		return nil
	}
	var plain []any
	if err := json.Unmarshal(data, &plain); err != nil {
		return nil
	}
	filtered := make([]any, 0, len(plain))
	for _, item := range plain {
		if obj, ok := item.(map[string]any); ok && obj["type"] == "function_call_output" {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func createCodexRequestID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err == nil {
		return "codex_" + hex.EncodeToString(data[:])
	}
	return fmt.Sprintf("codex_%d", time.Now().UnixNano())
}

func isCodexTerminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.done", "response.incomplete", "response.failed", "error":
		return true
	default:
		return false
	}
}

func resolveCodexWebSocketURL(baseURL string) (string, error) {
	raw := resolveCodexURL(baseURL)
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse codex websocket URL: %w", err)
	}

	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("codex websocket URL has unsupported scheme %q", parsed.Scheme)
	}
	return parsed.String(), nil
}
