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
}

// New returns an MCP server with the Masked Email tools. Every tool call is
// logged with its name, outcome and duration, never its arguments.
func New(store Store, log *slog.Logger) *mcp.Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &server{store: guard{store}}
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
type guard struct{ Store }

var errStateNotAllowed = errors.New("this server only sets addresses to enabled or disabled")

func (g guard) Create(ctx context.Context, p maskedemail.CreateParams) (*maskedemail.MaskedEmail, error) {
	if p.State != maskedemail.StateEnabled {
		return nil, errStateNotAllowed
	}
	return g.Store.Create(ctx, p)
}

func (g guard) Update(ctx context.Context, id string, p maskedemail.UpdateParams) (*maskedemail.MaskedEmail, error) {
	if p.State != nil && *p.State != maskedemail.StateEnabled && *p.State != maskedemail.StateDisabled {
		return nil, errStateNotAllowed
	}
	return g.Store.Update(ctx, id, p)
}

// addTool registers a handler and turns client errors into messages a model
// can act on.
func addTool[In, Out any](srv *mcp.Server, t *mcp.Tool, fn func(context.Context, In) (Out, error)) {
	mcp.AddTool(srv, t, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		out, err := fn(ctx, in)
		if err != nil {
			var zero Out
			return nil, zero, friendly(err)
		}
		return nil, out, nil
	})
}

func friendly(err error) error {
	switch {
	case errors.Is(err, jmap.ErrUnauthorized):
		return errors.New("Fastmail rejected the API token; it may be wrong, expired or revoked")
	case errors.Is(err, jmap.ErrUnsupportedCapability):
		return errors.New("the Fastmail API token does not have the Masked Email scope")
	case errors.Is(err, jmap.ErrRateLimit):
		return errors.New("Fastmail is rate limiting requests (it limits how many addresses can be created); try again later")
	case errors.Is(err, jmap.ErrNotFound):
		return errors.New("no masked email with that id")
	}
	return err
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
