package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ionut-maxim/fastmail"
	"github.com/ionut-maxim/fastmail/jmap"
	"github.com/ionut-maxim/fastmail/maskedemail"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ionut-maxim/fastmail-mcp/internal/fakejmap"
)

// seeded returns a fake with one address in each state.
func seeded(t *testing.T) *fakejmap.Server {
	t.Helper()
	f := fakejmap.New(t)
	f.Add(map[string]any{"id": "m1", "email": "shop.1@example.com", "state": "enabled", "forDomain": "https://shop.example.com",
		"description": "Shop account", "url": "", "createdAt": "2026-03-01T10:00:00Z", "lastMessageAt": "2026-04-01T10:00:00Z"})
	f.Add(map[string]any{"id": "m2", "email": "news.2@example.com", "state": "disabled", "forDomain": "https://news.example.org",
		"description": "Newsletter", "url": "https://news.example.org/account", "createdAt": "2026-05-01T10:00:00Z", "lastMessageAt": nil})
	f.Add(map[string]any{"id": "m3", "email": "temp.3@example.com", "state": "pending", "forDomain": "https://example.net",
		"description": "Trial", "url": "", "createdAt": "2026-06-01T10:00:00Z", "lastMessageAt": nil})
	f.Add(map[string]any{"id": "m4", "email": "old.4@example.com", "state": "deleted", "forDomain": "https://example.com",
		"description": "Old forum", "url": "", "createdAt": "2026-01-01T10:00:00Z", "lastMessageAt": nil})
	return f
}

func newClient(t *testing.T, f *fakejmap.Server) *fastmail.Client {
	t.Helper()
	c, err := fastmail.New(fakejmap.Token, jmap.WithSessionURL(f.SessionURL()))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func connect(t *testing.T, store Store) *mcp.ClientSession {
	t.Helper()
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := New(store, nil).Connect(t.Context(), serverT, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(t.Context(), clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func decode[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	var out T
	if res.IsError {
		t.Fatalf("tool error: %s", text(res))
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// wantToolError checks the call failed, either as a tool error or as a
// protocol error (schema validation), with msg in the message.
func wantToolError(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, msg string) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	switch {
	case err != nil:
		if !strings.Contains(err.Error(), msg) {
			t.Errorf("%s: error %q, want it to contain %q", name, err, msg)
		}
	case !res.IsError:
		t.Errorf("%s: succeeded, want an error containing %q", name, msg)
	case !strings.Contains(text(res), msg):
		t.Errorf("%s: error %q, want it to contain %q", name, text(res), msg)
	}
}

func emails(out listOut) []string {
	var e []string
	for _, m := range out.MaskedEmails {
		e = append(e, m.Email)
	}
	return e
}

func TestListDefaultsToEnabledAndDisabledNewestFirst(t *testing.T) {
	cs := connect(t, newClient(t, seeded(t)).MaskedEmail)
	out := decode[listOut](t, call(t, cs, "list_masked_emails", nil))
	if want := []string{"news.2@example.com", "shop.1@example.com"}; !slices.Equal(emails(out), want) || out.Total != 2 {
		t.Errorf("got %v (total %d), want %v", emails(out), out.Total, want)
	}
	m := out.MaskedEmails[1]
	want := maskedEmailOut{ID: "m1", Email: "shop.1@example.com", State: "enabled", ForDomain: "https://shop.example.com",
		Description: "Shop account", CreatedAt: "2026-03-01T10:00:00Z", LastMessageAt: "2026-04-01T10:00:00Z"}
	if m != want {
		t.Errorf("got %+v, want %+v", m, want)
	}
}

func TestListFilters(t *testing.T) {
	cs := connect(t, newClient(t, seeded(t)).MaskedEmail)
	for _, tc := range []struct {
		name string
		args map[string]any
		want []string
	}{
		{"states", map[string]any{"states": []string{"pending", "deleted"}}, []string{"temp.3@example.com", "old.4@example.com"}},
		{"domain matches subdomains", map[string]any{"domain": "example.com", "states": []string{"enabled", "disabled", "deleted"}}, []string{"shop.1@example.com", "old.4@example.com"}},
		{"domain as origin", map[string]any{"domain": "https://news.example.org"}, []string{"news.2@example.com"}},
		{"query on description", map[string]any{"query": "NEWSLETTER"}, []string{"news.2@example.com"}},
		{"query on address", map[string]any{"query": "shop.1"}, []string{"shop.1@example.com"}},
		{"query on site", map[string]any{"query": "example.org"}, []string{"news.2@example.com"}},
		{"no match", map[string]any{"query": "nothing"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := decode[listOut](t, call(t, cs, "list_masked_emails", tc.args))
			if !slices.Equal(emails(out), tc.want) {
				t.Errorf("got %v, want %v", emails(out), tc.want)
			}
		})
	}
}

func TestListLimit(t *testing.T) {
	cs := connect(t, newClient(t, seeded(t)).MaskedEmail)
	out := decode[listOut](t, call(t, cs, "list_masked_emails", map[string]any{"limit": 1}))
	if out.Total != 2 || len(out.MaskedEmails) != 1 || out.MaskedEmails[0].ID != "m2" {
		t.Errorf("got %+v", out)
	}
	wantToolError(t, cs, "list_masked_emails", map[string]any{"limit": 501}, "limit must be")
	wantToolError(t, cs, "list_masked_emails", map[string]any{"states": []string{"gone"}}, "states")
}

func TestCreateIsEnabled(t *testing.T) {
	f := fakejmap.New(t)
	cs := connect(t, newClient(t, f).MaskedEmail)
	out := decode[maskedEmailOut](t, call(t, cs, "create_masked_email", map[string]any{
		"forDomain": "Shop.Example.com/login", "description": " Shop account ", "url": "https://example.com/acct", "emailPrefix": "shop",
	}))
	if out.State != "enabled" || !strings.HasPrefix(out.Email, "shop.") || out.ID == "" {
		t.Errorf("got %+v", out)
	}
	stored := f.Item(out.ID)
	if stored["state"] != "enabled" || stored["forDomain"] != "https://shop.example.com" || stored["description"] != "Shop account" || stored["url"] != "https://example.com/acct" {
		t.Errorf("stored %v", stored)
	}
}

func TestCreateValidates(t *testing.T) {
	f := fakejmap.New(t)
	cs := connect(t, newClient(t, f).MaskedEmail)
	wantToolError(t, cs, "create_masked_email", map[string]any{"forDomain": "", "description": "x"}, "forDomain is required")
	wantToolError(t, cs, "create_masked_email", map[string]any{"forDomain": "example.com", "description": "  "}, "description is required")
	wantToolError(t, cs, "create_masked_email", map[string]any{"forDomain": "example.com", "description": "x", "emailPrefix": "Not-OK"}, "emailPrefix")
	wantToolError(t, cs, "create_masked_email", map[string]any{"forDomain": "ftp://example.com", "description": "x"}, "is not a site")
	wantToolError(t, cs, "create_masked_email", map[string]any{"description": "x"}, "forDomain")
	if n := len(f.Sets()); n != 0 {
		t.Errorf("%d invalid creates reached Fastmail", n)
	}
}

func TestCreateRateLimited(t *testing.T) {
	f := fakejmap.New(t)
	f.RateLimit = true
	cs := connect(t, newClient(t, f).MaskedEmail)
	wantToolError(t, cs, "create_masked_email", map[string]any{"forDomain": "example.com", "description": "x"}, "rate limiting")
}

func TestEnableDisable(t *testing.T) {
	f := seeded(t)
	cs := connect(t, newClient(t, f).MaskedEmail)

	out := decode[maskedEmailOut](t, call(t, cs, "disable_masked_email", map[string]any{"id": "m1"}))
	if out.State != "disabled" || f.Item("m1")["state"] != "disabled" {
		t.Errorf("disable by id: got %+v, stored %v", out, f.Item("m1")["state"])
	}
	out = decode[maskedEmailOut](t, call(t, cs, "enable_masked_email", map[string]any{"email": "SHOP.1@example.com"}))
	if out.State != "enabled" || out.ID != "m1" || f.Item("m1")["state"] != "enabled" {
		t.Errorf("enable by email: got %+v", out)
	}
	sets := len(f.Sets())
	decode[maskedEmailOut](t, call(t, cs, "enable_masked_email", map[string]any{"id": "m1"}))
	if len(f.Sets()) != sets {
		t.Errorf("enabling an enabled address still sent an update")
	}

	wantToolError(t, cs, "disable_masked_email", map[string]any{"id": "nope"}, "no masked email with that id")
	wantToolError(t, cs, "disable_masked_email", map[string]any{"email": "nobody@example.com"}, "no masked email with that address")
	wantToolError(t, cs, "disable_masked_email", map[string]any{}, "give the id or the email")
	wantToolError(t, cs, "disable_masked_email", map[string]any{"id": "m1", "email": "shop.1@example.com"}, "not both")
}

func TestUpdate(t *testing.T) {
	f := seeded(t)
	cs := connect(t, newClient(t, f).MaskedEmail)

	out := decode[maskedEmailOut](t, call(t, cs, "update_masked_email", map[string]any{
		"email": "news.2@example.com", "description": "Weekly news", "url": "", "forDomain": "example.org",
	}))
	if out.Description != "Weekly news" || out.URL != "" || out.ForDomain != "https://example.org" || out.State != "disabled" {
		t.Errorf("got %+v", out)
	}
	var sent struct {
		Update map[string]map[string]any `json:"update"`
	}
	sets := f.Sets()
	if err := json.Unmarshal(sets[len(sets)-1], &sent); err != nil {
		t.Fatal(err)
	}
	// The client clears a field by sending null.
	if got := sent.Update["m2"]; len(got) != 3 || got["url"] != nil || got["description"] != "Weekly news" {
		t.Errorf("sent %v", got)
	}

	before := len(f.Sets())
	wantToolError(t, cs, "update_masked_email", map[string]any{"id": "m2"}, "nothing to change")
	if len(f.Sets()) != before {
		t.Errorf("an empty update reached Fastmail")
	}
}

// TestNoDeleteReachable checks that no tool can delete an address or move it
// to the deleted (or pending) state, whatever arguments it is given.
func TestNoDeleteReachable(t *testing.T) {
	f := seeded(t)
	cs := connect(t, newClient(t, f).MaskedEmail)

	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if strings.Contains(tool.Name, "delete") || strings.Contains(tool.Name, "destroy") {
			t.Errorf("tool %s offered", tool.Name)
		}
		if tool.Name == "list_masked_emails" {
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Errorf("list_masked_emails is not marked read-only")
			}
			continue
		}
		if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Errorf("%s is not marked non-destructive", tool.Name)
		}
		schema, _ := json.Marshal(tool.InputSchema)
		if strings.Contains(string(schema), `"state`) {
			t.Errorf("%s takes a state: %s", tool.Name, schema)
		}
	}
	slices.Sort(names)
	want := []string{"create_masked_email", "disable_masked_email", "enable_masked_email", "list_masked_emails", "update_masked_email"}
	if !slices.Equal(names, want) {
		t.Errorf("tools %v, want %v", names, want)
	}

	// Smuggled state arguments are rejected by the input schemas.
	for _, name := range []string{"create_masked_email", "enable_masked_email", "disable_masked_email", "update_masked_email"} {
		for _, state := range []string{"deleted", "pending"} {
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{
				"id": "m1", "forDomain": "example.com", "description": "x", "state": state,
			}})
			if err == nil && !res.IsError {
				t.Errorf("%s accepted state %q", name, state)
			}
		}
	}

	// Exercise every tool, then check nothing sent a destroy or a forbidden state.
	call(t, cs, "create_masked_email", map[string]any{"forDomain": "example.com", "description": "x"})
	call(t, cs, "disable_masked_email", map[string]any{"id": "m1"})
	wantToolError(t, cs, "enable_masked_email", map[string]any{"id": "m4"}, "deleted")
	call(t, cs, "update_masked_email", map[string]any{"id": "m2", "description": "y"})
	for _, raw := range f.Sets() {
		var set struct {
			Create  map[string]map[string]any `json:"create"`
			Update  map[string]map[string]any `json:"update"`
			Destroy []string                  `json:"destroy"`
		}
		if err := json.Unmarshal(raw, &set); err != nil {
			t.Fatal(err)
		}
		if len(set.Destroy) > 0 {
			t.Errorf("destroy sent: %s", raw)
		}
		for _, p := range set.Create {
			if p["state"] != "enabled" {
				t.Errorf("create with state %v: %s", p["state"], raw)
			}
		}
		for _, p := range set.Update {
			if st, ok := p["state"]; ok && st != "enabled" && st != "disabled" {
				t.Errorf("update to state %v: %s", st, raw)
			}
		}
	}
}

// recordingStore records what reaches the store.
type recordingStore struct {
	Store
	calls int
}

func (r *recordingStore) Update(ctx context.Context, id string, p maskedemail.UpdateParams) (*maskedemail.MaskedEmail, error) {
	r.calls++
	return &maskedemail.MaskedEmail{ID: id}, nil
}

func (r *recordingStore) Create(ctx context.Context, p maskedemail.CreateParams) (*maskedemail.MaskedEmail, error) {
	r.calls++
	return &maskedemail.MaskedEmail{}, nil
}

func TestGuardRefusesOtherStates(t *testing.T) {
	rec := &recordingStore{}
	g := guard{rec}
	for _, st := range []maskedemail.State{maskedemail.StateDeleted, maskedemail.StatePending, "other"} {
		if _, err := g.Update(t.Context(), "m1", maskedemail.UpdateParams{State: &st}); !errors.Is(err, errStateNotAllowed) {
			t.Errorf("update to %q: err %v", st, err)
		}
		if _, err := g.Create(t.Context(), maskedemail.CreateParams{State: st}); !errors.Is(err, errStateNotAllowed) {
			t.Errorf("create as %q: err %v", st, err)
		}
	}
	if _, err := g.Create(t.Context(), maskedemail.CreateParams{}); !errors.Is(err, errStateNotAllowed) {
		t.Errorf("create with the server default (pending): err %v", err)
	}
	if rec.calls != 0 {
		t.Errorf("%d forbidden calls reached the store", rec.calls)
	}
	for _, st := range []maskedemail.State{maskedemail.StateEnabled, maskedemail.StateDisabled} {
		if _, err := g.Update(t.Context(), "m1", maskedemail.UpdateParams{State: &st}); err != nil {
			t.Errorf("update to %q: %v", st, err)
		}
	}
}

func TestFriendlyErrors(t *testing.T) {
	f := seeded(t)
	c, err := fastmail.New("wrong-token", jmap.WithSessionURL(f.SessionURL()))
	if err != nil {
		t.Fatal(err)
	}
	cs := connect(t, c.MaskedEmail)
	wantToolError(t, cs, "list_masked_emails", nil, "Fastmail rejected the API token")
}

func TestSiteOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"example.com":                       "https://example.com",
		"https://Example.com/path?q=1":      "https://example.com",
		"http://localhost:3000":             "http://localhost:3000",
		"  shop.example.com  ":              "https://shop.example.com",
		"https://user@example.com":          "",
		"mailto:someone@example.com":        "",
		"not a site":                        "",
		"javascript://example.com/%0Aalert": "",
		"https://:80":                       "",
		"example.com:99999":                 "",
		"example.com:0":                     "",
		"example.com:":                      "",
		"example.com:http":                  "",
		"example.com:8443":                  "https://example.com:8443",
		"https://[::1]:8080/x":              "https://[::1]:8080",
	} {
		got, err := siteOrigin(in)
		if want == "" {
			if err == nil {
				t.Errorf("siteOrigin(%q) = %q, want an error", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("siteOrigin(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestCheckURL(t *testing.T) {
	for in, ok := range map[string]bool{
		"":                                     true,
		"  ":                                   true,
		"https://example.com/account?id=1":     true,
		"http://example.com":                   true,
		"example.com/account":                  false,
		"javascript:alert(1)":                  false,
		"ftp://example.com/file":               false,
		"https://user:pw@example.com":          false,
		"https://example.com:99999/":           false,
		"https://" + strings.Repeat("a", 2049): false,
	} {
		if _, err := checkURL(in); (err == nil) != ok {
			t.Errorf("checkURL(%.40q): err %v, want ok=%v", in, err, ok)
		}
	}
}

func TestValidationLimits(t *testing.T) {
	f := seeded(t)
	cs := connect(t, newClient(t, f).MaskedEmail)
	long := strings.Repeat("x", 300)
	wantToolError(t, cs, "create_masked_email", map[string]any{"forDomain": "example.com", "description": long}, "longer than 256")
	wantToolError(t, cs, "create_masked_email", map[string]any{"forDomain": "example.com", "description": "x", "url": "javascript:alert(1)"}, "not an http or https link")
	wantToolError(t, cs, "create_masked_email", map[string]any{"forDomain": "example.com" + strings.Repeat("a", 2048), "description": "x"}, "longer than 2048")
	wantToolError(t, cs, "update_masked_email", map[string]any{"id": "m1", "description": long}, "longer than 256")
	wantToolError(t, cs, "update_masked_email", map[string]any{"id": "m1", "url": "ftp://example.com"}, "not an http or https link")
	decode[maskedEmailOut](t, call(t, cs, "create_masked_email", map[string]any{"forDomain": "example.com", "description": strings.Repeat("é", 256)}))

	// A rejected value is echoed only in part.
	bad := "not a site " + strings.Repeat("y", 500)
	res := call(t, cs, "create_masked_email", map[string]any{"forDomain": bad, "description": "x"})
	if !res.IsError || strings.Contains(text(res), bad[:100]) || !strings.Contains(text(res), bad[:64]) {
		t.Errorf("error %q", text(res))
	}
	if n := len(f.Sets()); n != 1 {
		t.Errorf("%d creates reached Fastmail, want only the valid one", n)
	}
}

func TestDeletedAddressesStayDeleted(t *testing.T) {
	f := seeded(t)
	cs := connect(t, newClient(t, f).MaskedEmail)
	wantToolError(t, cs, "enable_masked_email", map[string]any{"id": "m4"}, "deleted")
	wantToolError(t, cs, "disable_masked_email", map[string]any{"email": "old.4@example.com"}, "deleted")
	if f.Item("m4")["state"] != "deleted" || len(f.Sets()) != 0 {
		t.Errorf("deleted address changed: %v, %d sets", f.Item("m4"), len(f.Sets()))
	}
	// Pending addresses can still be enabled, which keeps them.
	out := decode[maskedEmailOut](t, call(t, cs, "enable_masked_email", map[string]any{"id": "m3"}))
	if out.State != "enabled" {
		t.Errorf("got %+v", out)
	}
}
