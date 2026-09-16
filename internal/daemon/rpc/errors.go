package rpc

import (
	"fmt"

	"github.com/raskrebs/sonar/internal/state"
)

// ErrorData is the contract §2 payload every error carries. Clients branch on
// Code (the stable string), never on the numeric JSON-RPC code.
type ErrorData struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
	Hint   string `json:"hint"`
	// Share is the share a refusal is about, when naming it is what makes the
	// refusal answerable. `share_limit_reached` (1107) is the case that needs
	// it: the second attempt should be an offer rather than an error, and both
	// clients turn it into "you're already sharing web at <url>; stop that and
	// share api instead?" — which they can only do if they are told which
	// share is in the way (sonar-relay/docs/SHARE.md, "One share at a time").
	Share *state.Share `json:"share,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int       `json:"code"`
	Message string    `json:"message"`
	Data    ErrorData `json:"data"`
}

func (e *Error) Error() string { return e.Message }

// Error code registry, contract §2. Numeric codes are the JSON-RPC wire codes;
// CodeName maps each to the stable string in error.data.code.
const (
	CodeInvalidParams   = -32602
	CodeInternal        = 1000
	CodeNotFound        = 1001
	CodeAmbiguous       = 1002
	CodePermission      = 1003
	CodeUnsupported     = 1004
	CodeBusy            = 1005
	CodeInvalidConfig   = 1006
	CodeAlreadyRunning  = 1007
	CodeTimeout         = 1008
	CodeInvalidSelector = 1009
	CodeOutsideHome     = 1010
	CodeConflict        = 1011

	// 1100-1199 are owned by spec 3 (shares and proxies). 1101-1106 and 1109
	// belonged to the provider design (ngrok, cloudflared) and were deleted
	// before anything returned them; they stay unused rather than reassigned.
	CodeTargetNotListening = 1100
	CodeShareLimitReached  = 1107
	CodeListenPortInUse    = 1108
	CodeNotSignedIn        = 1110
	CodeShareExpired       = 1111
	CodeRelayUnreachable   = 1112
	CodeShareBlocked       = 1113
	CodeTargetNotHTTP      = 1114

	// 1200-1299 are owned by spec 2 (MCP, sessions and claims).
	CodeSessionNotFound = 1200
	CodeClaimConflict   = 1201
)

var codeNames = map[int]string{
	CodeInvalidParams:   "invalid_params",
	CodeInternal:        "internal",
	CodeNotFound:        "not_found",
	CodeAmbiguous:       "ambiguous",
	CodePermission:      "permission_denied",
	CodeUnsupported:     "unsupported_platform",
	CodeBusy:            "daemon_busy",
	CodeInvalidConfig:   "invalid_config",
	CodeAlreadyRunning:  "already_running",
	CodeTimeout:         "timeout",
	CodeInvalidSelector: "invalid_selector",
	CodeOutsideHome:     "outside_home",
	CodeConflict:        "conflict",

	CodeTargetNotListening: "target_not_listening",
	CodeShareLimitReached:  "share_limit_reached",
	CodeListenPortInUse:    "listen_port_in_use",
	CodeNotSignedIn:        "not_signed_in",
	CodeShareExpired:       "share_expired",
	CodeRelayUnreachable:   "relay_unreachable",
	CodeShareBlocked:       "share_blocked",
	CodeTargetNotHTTP:      "target_not_http",

	CodeSessionNotFound: "session_not_found",
	CodeClaimConflict:   "claim_conflict",
}

// CodeName maps a numeric code to its stable data.code string. Unknown codes
// degrade to "internal" so a client never has to handle an empty code.
func CodeName(code int) string {
	if name, ok := codeNames[code]; ok {
		return name
	}
	return codeNames[CodeInternal]
}

// NewError builds an error carrying both the numeric code and the contract's
// {code, detail, hint} data. Hint is a ready-to-print CLI suggestion.
func NewError(code int, detail, hint string) *Error {
	return &Error{
		Code:    code,
		Message: detail,
		Data:    ErrorData{Code: CodeName(code), Detail: detail, Hint: hint},
	}
}

// Errorf is NewError with a formatted detail and no hint.
func Errorf(code int, format string, args ...any) *Error {
	return NewError(code, fmt.Sprintf(format, args...), "")
}

// ShareLimitError is 1107 with the live share attached, so a client can make
// the offer instead of reporting a refusal.
func ShareLimitError(detail, hint string, live state.Share) *Error {
	e := NewError(CodeShareLimitReached, detail, hint)
	e.Data.Share = &live
	return e
}
