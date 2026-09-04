package mcpgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The two relayed MCP methods. Everything else — initialize, ping,
// notifications — is handled by the SDK and never reaches a device.
const (
	methodListTools = "tools/list"
	methodCallTool  = "tools/call"
)

// sessionMiddleware intercepts the relayed methods on one MCP session and
// re-authorizes the session principal on every call: token and grant state,
// resource binding, identity, device ownership and binding, entitlement,
// and tool scope. Non-relayed methods pass through to the SDK untouched,
// preserving the initialize/initialized handshake.
func (g *Gateway) sessionMiddleware(digest [32]byte) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case methodListTools:
				return g.handleToolsList(ctx, req, digest)
			case methodCallTool:
				return g.handleToolsCall(ctx, req, digest)
			default:
				return next(ctx, method, req)
			}
		}
	}
}

// handleToolsList relays the catalog request to the device and filters the
// response to the tools the grant's scopes permit.
func (g *Gateway) handleToolsList(ctx context.Context, req mcp.Request, digest [32]byte) (mcp.Result, error) {
	principal, err := g.reauthorize(ctx, digest)
	if err != nil {
		recordDenial(ctx, g.cfg.Audit, auditReasonSessionInvalid, principal.Grant.UserID, "")
		return nil, sessionDeniedError()
	}
	response, err := g.relay.Call(ctx, sessionIDOf(req), principal.Grant,
		methodListTools, marshalParams(req.GetParams()))
	if err != nil {
		return nil, relayError(err)
	}
	result, wireErr := parseRelayResponse(response)
	if wireErr != nil {
		return nil, wireErr
	}
	filtered, err := filterTools(result, principal.Grant.Scopes)
	if err != nil {
		return nil, sanitizedInternalError()
	}
	return filtered, nil
}

// handleToolsCall enforces the tool policy, then relays the call to the
// device. Unknown tools and insufficient scopes are denied locally and
// audited; nothing is delivered to the Bridge for a denied call.
func (g *Gateway) handleToolsCall(ctx context.Context, req mcp.Request, digest [32]byte) (mcp.Result, error) {
	principal, err := g.reauthorize(ctx, digest)
	if err != nil {
		recordDenial(ctx, g.cfg.Audit, auditReasonSessionInvalid, principal.Grant.UserID, "")
		return nil, sessionDeniedError()
	}
	params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
	if !ok || params == nil {
		return nil, invalidParamsError()
	}
	scope, allowed := g.policy.RequiredScope(params.Name)
	if !allowed {
		recordDenial(ctx, g.cfg.Audit, auditReasonUnknownTool, principal.Grant.UserID, principal.Grant.ID)
		return nil, &jsonrpc.Error{Code: codeInvalidParams, Message: unknownToolMessage(params.Name)}
	}
	if !scopeAllowed(principal.Grant.Scopes, scope) {
		recordDenial(ctx, g.cfg.Audit, auditReasonInsufficientScope, principal.Grant.UserID, principal.Grant.ID)
		return nil, &jsonrpc.Error{Code: codeScopeDenied, Message: "insufficient scope for tool"}
	}

	response, err := g.relay.Call(ctx, sessionIDOf(req), principal.Grant,
		methodCallTool, marshalParams(req.GetParams()))
	if err != nil {
		return nil, relayError(err)
	}
	result, wireErr := parseRelayResponse(response)
	if wireErr != nil {
		return nil, wireErr
	}
	var callResult mcp.CallToolResult
	if err := json.Unmarshal(result, &callResult); err != nil {
		return nil, sanitizedInternalError()
	}
	return &callResult, nil
}

// filterTools removes every tool the granted scopes do not permit. The
// device's tool definitions (descriptions, schemas, annotations) pass
// through unmodified for the tools that remain.
func filterTools(result json.RawMessage, granted []string) (*mcp.ListToolsResult, error) {
	var payload struct {
		Tools      []*mcp.Tool `json:"tools"`
		NextCursor string      `json:"nextCursor"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return nil, fmt.Errorf("mcpgateway: decode tool list: %w", err)
	}
	kept := make([]*mcp.Tool, 0, len(payload.Tools))
	for _, tool := range payload.Tools {
		if tool == nil {
			continue
		}
		scope, allowed := Policy{}.RequiredScope(tool.Name)
		if !allowed || !scopeAllowed(granted, scope) {
			continue
		}
		kept = append(kept, tool)
	}
	return &mcp.ListToolsResult{Tools: kept, NextCursor: payload.NextCursor}, nil
}

// parseRelayResponse splits the device's raw JSON-RPC response into its
// result payload or its structured error. Device errors pass through with
// their code and message; malformed responses become a sanitized internal
// error.
func parseRelayResponse(payload json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	var parsed struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, sanitizedInternalError()
	}
	if parsed.Error != nil {
		return nil, &jsonrpc.Error{
			Code:    int64(parsed.Error.Code),
			Message: parsed.Error.Message,
			Data:    parsed.Error.Data,
		}
	}
	if len(parsed.Result) == 0 {
		return nil, sanitizedInternalError()
	}
	return parsed.Result, nil
}

// relayError maps a relay sentinel to its fixed sanitized JSON-RPC error.
// Internal error text never reaches the connector.
func relayError(err error) *jsonrpc.Error {
	switch {
	case errors.Is(err, ErrDeadline):
		return &jsonrpc.Error{Code: codeTimeout, Message: "request timed out"}
	case errors.Is(err, ErrCancelled):
		return &jsonrpc.Error{Code: codeCancelled, Message: "request cancelled"}
	case errors.Is(err, ErrAmbiguousTarget):
		return &jsonrpc.Error{Code: codeTargetUnavailable, Message: "multiple Studios are online; bind a target"}
	case errors.Is(err, ErrStudioUnavailable):
		return &jsonrpc.Error{Code: codeTargetUnavailable, Message: "target Studio is offline"}
	case errors.Is(err, ErrDeviceFailed):
		return &jsonrpc.Error{Code: codeTargetUnavailable, Message: "target device connection lost"}
	case errors.Is(err, ErrDeviceGone):
		return &jsonrpc.Error{Code: codeTargetUnavailable, Message: "target device is unavailable"}
	case errors.Is(err, ErrBusy):
		return &jsonrpc.Error{Code: codeBusy, Message: "server busy, retry later"}
	case errors.Is(err, ErrInvalidRequest):
		return invalidParamsError()
	default:
		return sanitizedInternalError()
	}
}

// sessionDeniedError reports a failed per-call re-authorization with a
// distinct code so connectors re-authenticate instead of retrying the call.
func sessionDeniedError() *jsonrpc.Error {
	return &jsonrpc.Error{Code: codeReauthRequired, Message: "session authorization failed; reconnect"}
}

func invalidParamsError() *jsonrpc.Error {
	return &jsonrpc.Error{Code: codeInvalidParams, Message: "invalid request parameters"}
}

func sanitizedInternalError() *jsonrpc.Error {
	return &jsonrpc.Error{Code: codeInternalError, Message: "internal error"}
}

// sessionIDOf returns the MCP session id of a request; it keys the
// correlation registry so session teardown retires every in-flight call.
func sessionIDOf(req mcp.Request) string {
	if session := req.GetSession(); session != nil {
		return session.ID()
	}
	return ""
}

// marshalParams serializes relayed method params verbatim; a nil params
// value is omitted.
func marshalParams(params any) json.RawMessage {
	if params == nil {
		return nil
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil
	}
	return encoded
}
