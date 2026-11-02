// Package mcpserver exposes Fastmail Masked Email as Model Context Protocol
// tools.
//
// The tools can list, create, enable, disable and edit addresses. They can
// never delete one or move it to the deleted state: disabling can be undone,
// deleting cannot. Store has no Delete method, and every update passes
// through a guard that only lets the enabled and disabled states through.
package mcpserver

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ionut-maxim/fastmail/jmap"
	"github.com/ionut-maxim/fastmail/maskedemail"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is stamped by the release build.
var Version = "dev"

// Store is the part of the Masked Email API the server uses.
// *maskedemail.Service implements it. It deliberately has no Delete.
type Store interface {
	List(ctx context.Context, opts *maskedemail.ListOptions) ([]maskedemail.MaskedEmail, error)
	Get(ctx context.Context, id string) (*maskedemail.MaskedEmail, error)
	Create(ctx context.Context, p maskedemail.CreateParams) (*maskedemail.MaskedEmail, error)
	Update(ctx context.Context, id string, p maskedemail.UpdateParams) (*maskedemail.MaskedEmail, error)
}

const instructions = `Tools for Fastmail Masked Email: generated addresses that forward to the user's inbox, one per site.

States: enabled (mail is delivered), disabled (mail goes to the trash), pending (new, never received mail; Fastmail removes it after 24 hours) and deleted (mail bounces).

New addresses are always created enabled. To stop an address, disable it; that can be undone with enable_masked_email. These tools cannot delete an address or set it to deleted; that has to be done in the Fastmail app.

Addresses can be named by id or by email address. Use list_masked_emails with domain or query to find one before changing it.`

type server struct {
	store Store
	log   *slog.Logger
}

// New returns an MCP server with the Masked Email tools. Every tool call is
// logged with its name, outcome and duration, never its arguments.
func New(store Store, log *slog.Logger) *mcp.Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &server{store: guard{store}, log: log}
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "fastmail-mcp", Title: "Fastmail Masked Email", Version: Version},
		&mcp.ServerOptions{Instructions: instructions},
	)
	s.addTools(srv)
	srv.AddReceivingMiddleware(auditLog(log))
	return srv
}

// guard refuses any update that would move an address to a state other than
// enabled or disabled. The tools never ask for one; this keeps it that way.
// It also marks every error from the store as a Fastmail error, so that
// friendly can tell them from the tools' own validation errors.
type guard struct{ Store }

var errStateNotAllowed = errors.New("this server only sets addresses to enabled or disabled")

// fastmailError is an error returned by the store.
type fastmailError struct{ err error }

func (e fastmailError) Error() string { return e.err.Error() }
func (e fastmailError) Unwrap() error { return e.err }

func wrap[T any](v T, err error) (T, error) {
	if err != nil {
		return v, fastmailError{err}
	}
	return v, nil
}

func (g guard) List(ctx context.Context, opts *maskedemail.ListOptions) ([]maskedemail.MaskedEmail, error) {
	return wrap(g.Store.List(ctx, opts))
}

func (g guard) Get(ctx context.Context, id string) (*maskedemail.MaskedEmail, error) {
	return wrap(g.Store.Get(ctx, id))
}

func (g guard) Create(ctx context.Context, p maskedemail.CreateParams) (*maskedemail.MaskedEmail, error) {
	if p.State != maskedemail.StateEnabled {
		return nil, errStateNotAllowed
	}
	return wrap(g.Store.Create(ctx, p))
}

func (g guard) Update(ctx context.Context, id string, p maskedemail.UpdateParams) (*maskedemail.MaskedEmail, error) {
	if p.State != nil && *p.State != maskedemail.StateEnabled && *p.State != maskedemail.StateDisabled {
		return nil, errStateNotAllowed
	}
	return wrap(g.Store.Update(ctx, id, p))
}

// addTool registers a handler and turns client errors into messages a model
// can act on.
func addTool[In, Out any](s *server, srv *mcp.Server, t *mcp.Tool, fn func(context.Context, In) (Out, error)) {
	mcp.AddTool(srv, t, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		out, err := fn(ctx, in)
		if err != nil {
			var zero Out
			return nil, zero, s.friendly(ctx, t.Name, err)
		}
		return nil, out, nil
	})
}

var errFastmail = errors.New("Fastmail request failed; try again later")

// friendly turns an error into a message for the model. The tools' own
// validation errors pass through. Fastmail errors with a known meaning get a
// specific message; any other is logged here and reported generically, so
// server details do not reach the client.
func (s *server) friendly(ctx context.Context, tool string, err error) error {
	var fe fastmailError
	if !errors.As(err, &fe) {
		return err
	}
	switch {
	case errors.Is(err, jmap.ErrUnauthorized):
		return errors.New("Fastmail rejected the API token; it may be wrong, expired or revoked")
	case errors.Is(err, jmap.ErrUnsupportedCapability):
		return errors.New("the Fastmail API token does not have the Masked Email scope")
	case errors.Is(err, jmap.ErrRateLimit):
		return errors.New("Fastmail is rate limiting requests (it limits how many addresses can be created); try again later")
	case errors.Is(err, jmap.ErrNotFound):
		return errors.New("no masked email with that id")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errors.New("the request was cancelled")
	}
	s.log.WarnContext(ctx, "fastmail request failed", "tool", tool, "err", fe.err)
	return errFastmail
}

// auditLog records which tool was called. Arguments and results are left
// out since they hold addresses and the sites they are used for.
func auditLog(log *slog.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			p, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if !ok {
				log.Debug("mcp request", "method", method)
				return next(ctx, method, req)
			}
			start := time.Now()
			res, err := next(ctx, method, req)
			failed := err != nil
			if r, ok := res.(*mcp.CallToolResult); ok && r.IsError {
				failed = true
			}
			log.Info("tool call", "method", method, "tool", p.Name, "failed", failed, "took", time.Since(start).Round(time.Millisecond))
			return res, err
		}
	}
}

var (
	readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true}
	// additive creates something new; nothing existing changes.
	additive = &mcp.ToolAnnotations{DestructiveHint: new(false)}
	// reversible changes an address in a way that can be set back.
	reversible = &mcp.ToolAnnotations{DestructiveHint: new(false), IdempotentHint: true}
)
