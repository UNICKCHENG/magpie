package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
)

// nativeScope is an explicitly named session under this gateway key and
// client. Never use conversationID's prompt hash as an isolation boundary.
// Missing session or client identity deliberately means a one-shot process.
func nativeScope(ctx context.Context, p provider.Provider, in http.Header) string {
	session := ""
	for _, h := range append([]string{SessionHeader}, sessionHeaders...) {
		if v := strings.TrimSpace(in.Get(h)); v != "" {
			session = v
			break
		}
	}
	if session == "" || in.Get("User-Agent") == "" {
		return ""
	}
	who := access.Caller(ctx).KeyID
	if who == "" {
		who = in.Get("Authorization") + "\x00" + in.Get("X-Api-Key")
	}
	// Full values prevent truncated session ids from sharing a native run.
	b, _ := json.Marshal([]string{p.ID, who, session, in.Get("User-Agent"), in.Get("X-App"), in.Get("Originator"), in.Get("X-Claude-Code-Agent-Id"), in.Get("X-Claude-Code-Parent-Agent-Id")})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
