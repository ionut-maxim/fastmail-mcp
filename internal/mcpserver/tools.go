package mcpserver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/ionut-maxim/fastmail/maskedemail"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultLimit = 50
	maxLimit     = 500

	maxDescription = 256
	maxURL         = 2048
	// maxEcho is how much of a rejected value an error message repeats.
	maxEcho = 64
)

// listStates are the states list_masked_emails accepts. They are filters,
// never values written back.
var listStates = []any{"enabled", "disabled", "pending", "deleted"}

func (s *server) addTools(srv *mcp.Server) {
	listSchema, err := jsonschema.For[listIn](nil)
	if err != nil {
		panic(err)
	}
	listSchema.Properties["states"].Items.Enum = listStates

	addTool(srv, &mcp.Tool{
		Name:  "list_masked_emails",
		Title: "List masked emails",
		Description: "List Fastmail masked email addresses, newest first. By default only enabled and disabled addresses are returned. " +
			"Filter by site with domain (matches subdomains too), and by text with query (matches the description, the address and the site). " +
			"Use it to find an address for a site before creating a new one, or to look up an id.",
		Annotations: readOnly,
		InputSchema: listSchema,
	}, s.list)
	addTool(srv, &mcp.Tool{
		Name:  "create_masked_email",
		Title: "Create masked email",
		Description: "Create a new masked email address for a site and return it. The address is created enabled, so it keeps working. " +
			"Check list_masked_emails first if the site may already have one. Fastmail limits how many addresses can be created in a short time.",
		Annotations: additive,
	}, s.create)
	addTool(srv, &mcp.Tool{
		Name:  "enable_masked_email",
		Title: "Enable masked email",
		Description: "Enable a masked email address, by id or by address, so its mail is delivered to the inbox again. " +
			"Addresses in the deleted state are refused: restoring one is only possible in the Fastmail app.",
		Annotations: reversible,
	}, s.enable)
	addTool(srv, &mcp.Tool{
		Name:  "disable_masked_email",
		Title: "Disable masked email",
		Description: "Disable a masked email address, by id or by address. Mail sent to it then goes straight to the trash. " +
			"This is the way to stop an address; it can be undone with enable_masked_email. " +
			"Addresses in the deleted state are refused and stay deleted.",
		Annotations: reversible,
	}, s.disable)
	addTool(srv, &mcp.Tool{
		Name:  "update_masked_email",
		Title: "Update masked email",
		Description: "Change the description, url or site (forDomain) of a masked email address, by id or by address. " +
			"Only the fields given change. Use enable_masked_email or disable_masked_email to change its state.",
		Annotations: reversible,
	}, s.update)
}

// maskedEmailOut is the compact form returned by every tool.
type maskedEmailOut struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	State         string `json:"state"`
	ForDomain     string `json:"forDomain,omitempty"`
	Description   string `json:"description,omitempty"`
	URL           string `json:"url,omitempty"`
	CreatedAt     string `json:"createdAt,omitempty"`
	LastMessageAt string `json:"lastMessageAt,omitempty"`
}

func toOut(m *maskedemail.MaskedEmail) maskedEmailOut {
	out := maskedEmailOut{
		ID:          m.ID,
		Email:       m.Email,
		State:       string(m.State),
		ForDomain:   m.ForDomain,
		Description: m.Description,
		URL:         m.URL,
	}
	if !m.CreatedAt.IsZero() {
		out.CreatedAt = m.CreatedAt.UTC().Format(time.RFC3339)
	}
	if m.LastMessageAt != nil && !m.LastMessageAt.IsZero() {
		out.LastMessageAt = m.LastMessageAt.UTC().Format(time.RFC3339)
	}
	return out
}

type listIn struct {
	States []string `json:"states,omitempty" jsonschema:"Only addresses in these states. Defaults to enabled and disabled."`
	Domain string   `json:"domain,omitempty" jsonschema:"Only addresses for this site or its subdomains, such as example.com or https://example.com"`
	Query  string   `json:"query,omitempty" jsonschema:"Only addresses whose description, address or site contains this text, ignoring case"`
	Limit  int      `json:"limit,omitempty" jsonschema:"Most addresses to return, 1 to 500. Defaults to 50."`
}

type listOut struct {
	// Total is how many addresses matched before the limit.
	Total        int              `json:"total"`
	MaskedEmails []maskedEmailOut `json:"maskedEmails"`
}

func (s *server) list(ctx context.Context, in listIn) (listOut, error) {
	opts := &maskedemail.ListOptions{
		States: []maskedemail.State{maskedemail.StateEnabled, maskedemail.StateDisabled},
	}
	if len(in.States) > 0 {
		opts.States = nil
		for _, st := range in.States {
			state := maskedemail.State(st)
			if !state.Valid() {
				return listOut{}, fmt.Errorf("unknown state %q: use enabled, disabled, pending or deleted", st)
			}
			opts.States = append(opts.States, state)
		}
	}
	if d := strings.TrimSpace(in.Domain); d != "" {
		host, err := siteHost(d)
		if err != nil {
			return listOut{}, err
		}
		opts.ForDomain = host
	}
	limit := in.Limit
	switch {
	case limit == 0:
		limit = defaultLimit
	case limit < 0 || limit > maxLimit:
		return listOut{}, fmt.Errorf("limit must be between 1 and %d", maxLimit)
	}

	all, err := s.store.List(ctx, opts)
	if err != nil {
		return listOut{}, err
	}
	query := strings.ToLower(strings.TrimSpace(in.Query))
	matched := make([]maskedemail.MaskedEmail, 0, len(all))
	for _, m := range all {
		if query == "" || matches(m, query) {
			matched = append(matched, m)
		}
	}
	slices.SortStableFunc(matched, func(a, b maskedemail.MaskedEmail) int {
		return cmp.Compare(b.CreatedAt.UnixNano(), a.CreatedAt.UnixNano())
	})
	out := listOut{Total: len(matched), MaskedEmails: []maskedEmailOut{}}
	for i := range matched[:min(limit, len(matched))] {
		out.MaskedEmails = append(out.MaskedEmails, toOut(&matched[i]))
	}
	return out, nil
}

func matches(m maskedemail.MaskedEmail, query string) bool {
	for _, f := range []string{m.Description, m.Email, m.ForDomain} {
		if strings.Contains(strings.ToLower(f), query) {
			return true
		}
	}
	return false
}

type createIn struct {
	ForDomain   string `json:"forDomain" jsonschema:"The site the address is for, such as example.com or https://example.com. Stored as the site's origin."`
	Description string `json:"description" jsonschema:"Short note on what the address is for, such as Newsletter or Shop account. At most 256 characters."`
	URL         string `json:"url,omitempty" jsonschema:"Optional http or https link to keep with the address, such as the account page"`
	EmailPrefix string `json:"emailPrefix,omitempty" jsonschema:"Optional start of the generated address: up to 64 characters of a-z, 0-9 and underscore"`
}

var prefixPattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

func (s *server) create(ctx context.Context, in createIn) (maskedEmailOut, error) {
	origin, err := siteOrigin(in.ForDomain)
	if err != nil {
		return maskedEmailOut{}, err
	}
	desc, err := checkDescription(in.Description)
	if err != nil {
		return maskedEmailOut{}, err
	}
	if desc == "" {
		return maskedEmailOut{}, errors.New("description is required")
	}
	link, err := checkURL(in.URL)
	if err != nil {
		return maskedEmailOut{}, err
	}
	prefix := strings.TrimSpace(in.EmailPrefix)
	if prefix != "" && !prefixPattern.MatchString(prefix) {
		return maskedEmailOut{}, errors.New("emailPrefix must be 1 to 64 characters of a-z, 0-9 and underscore")
	}
	m, err := s.store.Create(ctx, maskedemail.CreateParams{
		State:       maskedemail.StateEnabled,
		ForDomain:   origin,
		Description: desc,
		URL:         link,
		EmailPrefix: prefix,
	})
	if err != nil {
		return maskedEmailOut{}, err
	}
	return toOut(m), nil
}

// refIn names one address by id or by email address.
type refIn struct {
	ID    string `json:"id,omitempty" jsonschema:"Id of the masked email, from list_masked_emails. Give this or email."`
	Email string `json:"email,omitempty" jsonschema:"The masked email address itself. Give this or id."`
}

// resolve finds the address ref names.
func (s *server) resolve(ctx context.Context, ref refIn) (*maskedemail.MaskedEmail, error) {
	id, email := strings.TrimSpace(ref.ID), strings.TrimSpace(ref.Email)
	switch {
	case id != "" && email != "":
		return nil, errors.New("give id or email, not both")
	case id != "":
		return s.store.Get(ctx, id)
	case email != "":
		all, err := s.store.List(ctx, nil)
		if err != nil {
			return nil, err
		}
		for i := range all {
			if strings.EqualFold(all[i].Email, email) {
				return &all[i], nil
			}
		}
		return nil, errors.New("no masked email with that address")
	}
	return nil, errors.New("give the id or the email of the masked email")
}

func (s *server) setState(ctx context.Context, ref refIn, state maskedemail.State) (maskedEmailOut, error) {
	m, err := s.resolve(ctx, ref)
	if err != nil {
		return maskedEmailOut{}, err
	}
	if m.State == maskedemail.StateDeleted {
		return maskedEmailOut{}, errors.New("this masked email is deleted; these tools leave deleted addresses alone, restore it in the Fastmail app if needed")
	}
	if m.State == state {
		return toOut(m), nil
	}
	m, err = s.store.Update(ctx, m.ID, maskedemail.UpdateParams{State: &state})
	if err != nil {
		return maskedEmailOut{}, err
	}
	return toOut(m), nil
}

func (s *server) enable(ctx context.Context, in refIn) (maskedEmailOut, error) {
	return s.setState(ctx, in, maskedemail.StateEnabled)
}

func (s *server) disable(ctx context.Context, in refIn) (maskedEmailOut, error) {
	return s.setState(ctx, in, maskedemail.StateDisabled)
}

type updateIn struct {
	refIn
	Description *string `json:"description,omitempty" jsonschema:"New description, at most 256 characters. An empty string clears it."`
	URL         *string `json:"url,omitempty" jsonschema:"New http or https link. An empty string clears it."`
	ForDomain   *string `json:"forDomain,omitempty" jsonschema:"New site, such as example.com or https://example.com"`
}

func (s *server) update(ctx context.Context, in updateIn) (maskedEmailOut, error) {
	var p maskedemail.UpdateParams
	if in.Description != nil {
		desc, err := checkDescription(*in.Description)
		if err != nil {
			return maskedEmailOut{}, err
		}
		p.Description = &desc
	}
	if in.URL != nil {
		link, err := checkURL(*in.URL)
		if err != nil {
			return maskedEmailOut{}, err
		}
		p.URL = &link
	}
	if in.ForDomain != nil {
		origin, err := siteOrigin(*in.ForDomain)
		if err != nil {
			return maskedEmailOut{}, err
		}
		p.ForDomain = &origin
	}
	if p.Description == nil && p.URL == nil && p.ForDomain == nil {
		return maskedEmailOut{}, errors.New("nothing to change: give description, url or forDomain")
	}
	m, err := s.resolve(ctx, in.refIn)
	if err != nil {
		return maskedEmailOut{}, err
	}
	m, err = s.store.Update(ctx, m.ID, p)
	if err != nil {
		return maskedEmailOut{}, err
	}
	return toOut(m), nil
}

// siteOrigin turns "example.com", "https://example.com" or a full URL into
// the origin Fastmail stores in forDomain, such as "https://example.com".
func siteOrigin(site string) (string, error) {
	site = strings.TrimSpace(site)
	if site == "" {
		return "", errors.New("forDomain is required: the site the address is for, such as example.com")
	}
	if len(site) > maxURL {
		return "", fmt.Errorf("forDomain is longer than %d characters", maxURL)
	}
	full := site
	if !strings.Contains(full, "://") {
		full = "https://" + full
	}
	u, err := parseWebURL(full)
	if err != nil {
		return "", fmt.Errorf("forDomain %q is not a site such as example.com or https://example.com", clip(site))
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

// siteHost is the host part of siteOrigin, for filtering.
func siteHost(site string) (string, error) {
	origin, err := siteOrigin(site)
	if err != nil {
		return "", err
	}
	return origin[strings.Index(origin, "://")+3:], nil
}

// checkURL trims link and checks it is empty or an http(s) URL.
func checkURL(link string) (string, error) {
	link = strings.TrimSpace(link)
	if link == "" {
		return "", nil
	}
	if len(link) > maxURL {
		return "", fmt.Errorf("url is longer than %d characters", maxURL)
	}
	if _, err := parseWebURL(link); err != nil {
		return "", fmt.Errorf("url %q is not an http or https link", clip(link))
	}
	return link, nil
}

// parseWebURL parses an absolute http or https URL with a host, an optional
// valid port and no user info.
func parseWebURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, errors.New("not http or https")
	case u.User != nil:
		return nil, errors.New("user info")
	case u.Hostname() == "" || strings.ContainsAny(u.Hostname(), " @"):
		return nil, errors.New("no host")
	case strings.HasSuffix(u.Host, ":"):
		return nil, errors.New("empty port")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return nil, errors.New("bad port")
		}
	}
	return u, nil
}

// checkDescription trims desc and checks its length.
func checkDescription(desc string) (string, error) {
	desc = strings.TrimSpace(desc)
	if utf8.RuneCountInString(desc) > maxDescription {
		return "", fmt.Errorf("description is longer than %d characters", maxDescription)
	}
	return desc, nil
}

// clip shortens a rejected value before an error message repeats it.
func clip(s string) string {
	if utf8.RuneCountInString(s) <= maxEcho {
		return s
	}
	return string([]rune(s)[:maxEcho]) + "…"
}
