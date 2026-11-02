package mcpserver

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewHTTPHandler serves the tools over streamable HTTP at /mcp, for clients
// that send "Authorization: Bearer <token>". GET /healthz answers 200
// without a token and without calling Fastmail.
func NewHTTPHandler(store Store, token string, log *slog.Logger) (http.Handler, error) {
	if token == "" {
		return nil, errors.New("an HTTP bearer token is required")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	srv := New(store, log)
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{
			Stateless: true,
			Logger:    log,
			// A client that disconnects cancels its Fastmail call. The SDK
			// applies this to clients on protocol 2026-07-28 or later.
			PropagateRequestCancellation: true,
		})

	mux := http.NewServeMux()
	mux.Handle("/mcp", requireBearer(token, log, streamable))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok\n")
	})
	return mux, nil
}

// requireBearer lets a request through only with the right bearer token.
// Both sides are hashed first so the comparison takes the same time whatever
// the token's length.
func requireBearer(token string, log *slog.Logger, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := bearerToken(r)
		sum := sha256.Sum256([]byte(got))
		if !ok || subtle.ConstantTimeCompare(sum[:], want[:]) != 1 {
			log.Warn("unauthorized request", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr, "token_present", ok)
			w.Header().Set("WWW-Authenticate", `Bearer realm="fastmail-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}
