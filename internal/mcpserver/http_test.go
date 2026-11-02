package mcpserver

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ionut-maxim/fastmail/maskedemail"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ionut-maxim/fastmail-mcp/internal/fakejmap"
)

const testAuthToken = "s3cret-bearer-token"

type bearer struct{ header string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.header != "" {
		r.Header.Set("Authorization", b.header)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func httpServer(t *testing.T, store Store, log *slog.Logger) *httptest.Server {
	t.Helper()
	h, err := NewHTTPHandler(store, testAuthToken, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func connectHTTP(ctx context.Context, srv *httptest.Server, header string) (*mcp.ClientSession, error) {
	return mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{header}},
	}, nil)
}

func TestHTTPRequiresToken(t *testing.T) {
	srv := httpServer(t, newClient(t, seeded(t)).MaskedEmail, nil)
	for name, header := range map[string]string{
		"missing":      "",
		"wrong":        "Bearer not-the-token",
		"prefix":       "Bearer " + testAuthToken[:5],
		"longer":       "Bearer " + testAuthToken + "x",
		"basic scheme": "Basic " + testAuthToken,
		"no scheme":    testAuthToken,
		"empty bearer": "Bearer ",
	} {
		t.Run(name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(res.Header.Get("WWW-Authenticate"), "Bearer") {
				t.Errorf("got %d, WWW-Authenticate %q", res.StatusCode, res.Header.Get("WWW-Authenticate"))
			}
		})
	}
}

func TestHTTPWithToken(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	srv := httpServer(t, newClient(t, seeded(t)).MaskedEmail, log)

	for _, header := range []string{"Bearer " + testAuthToken, "bearer " + testAuthToken} {
		cs, err := connectHTTP(t.Context(), srv, header)
		if err != nil {
			t.Fatalf("%q: %v", header, err)
		}
		out := decode[listOut](t, call(t, cs, "list_masked_emails", nil))
		if out.Total != 2 {
			t.Errorf("got %+v", out)
		}
		cs.Close()
	}

	if _, err := connectHTTP(t.Context(), srv, "Bearer wrong"); err == nil {
		t.Errorf("connected with a wrong token")
	}

	if !strings.Contains(logs.String(), `"tool":"list_masked_emails"`) {
		t.Errorf("tool call not logged: %s", logs.String())
	}
	for _, secret := range []string{testAuthToken, fakejmap.Token, "shop.1@example.com", "wrong"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain %q: %s", secret, logs.String())
		}
	}
}

// panicStore fails the test if anything reaches Fastmail.
type panicStore struct {
	Store
	t *testing.T
}

func (p panicStore) List(context.Context, *maskedemail.ListOptions) ([]maskedemail.MaskedEmail, error) {
	p.t.Error("healthz called Fastmail")
	return nil, nil
}

func TestHealthzIsOpenAndOffline(t *testing.T) {
	srv := httpServer(t, panicStore{t: t}, nil)
	res, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Errorf("got %d %q", res.StatusCode, body)
	}
}

func TestHTTPNeedsAuthToken(t *testing.T) {
	if _, err := NewHTTPHandler(panicStore{t: t}, "", nil); err == nil {
		t.Error("NewHTTPHandler accepted an empty token")
	}
}
