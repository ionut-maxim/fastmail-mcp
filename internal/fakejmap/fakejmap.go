// Package fakejmap is an in-process fake of Fastmail's JMAP API with just
// enough of the MaskedEmail extension for tests. It records every
// MaskedEmail/set call so tests can check what was sent.
package fakejmap

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Token is the bearer token the fake accepts.
const Token = "test-fastmail-token"

const (
	accountID  = "u1"
	capability = "https://www.fastmail.com/dev/maskedemail"
)

// Server is a fake JMAP server holding masked emails in memory.
type Server struct {
	*httptest.Server

	mu    sync.Mutex
	items map[string]map[string]any
	order []string
	next  int
	sets  []json.RawMessage
	// RateLimit makes every create fail with a rateLimit SetError.
	RateLimit bool
}

// New starts a fake that is closed when the test ends.
func New(t testing.TB) *Server {
	s := &Server{items: map[string]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /jmap/session", s.session)
	mux.HandleFunc("POST /jmap/api/", s.api)
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// SessionURL is the URL to pass to jmap.WithSessionURL.
func (s *Server) SessionURL() string { return s.URL + "/jmap/session" }

// Add stores a masked email. It must have an "id".
func (s *Server) Add(m map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := m["id"].(string)
	s.items[id] = m
	s.order = append(s.order, id)
}

// Item returns a copy of the stored masked email with id, or nil.
func (s *Server) Item(id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.items[id]
	if !ok {
		return nil
	}
	c := map[string]any{}
	for k, v := range m {
		c[k] = v
	}
	return c
}

// Sets returns the arguments of every MaskedEmail/set call received.
func (s *Server) Sets() []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]json.RawMessage(nil), s.sets...)
}

func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+Token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	caps := map[string]any{
		"urn:ietf:params:jmap:core": map[string]any{"maxCallsInRequest": 50},
		capability:                  map[string]any{},
	}
	writeJSON(w, map[string]any{
		"capabilities": caps,
		"accounts": map[string]any{accountID: map[string]any{
			"name": "user@example.com", "isPersonal": true, "isReadOnly": false, "accountCapabilities": caps,
		}},
		"primaryAccounts": map[string]string{capability: accountID},
		"username":        "user@example.com",
		"apiUrl":          s.URL + "/jmap/api/",
		"downloadUrl":     s.URL + "/jmap/download/{accountId}/{blobId}/{name}?type={type}",
		"uploadUrl":       s.URL + "/jmap/upload/{accountId}/",
		"eventSourceUrl":  s.URL + "/jmap/event/",
		"state":           "s1",
	})
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	var body struct {
		Calls [][]json.RawMessage `json:"methodCalls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "not JSON", http.StatusBadRequest)
		return
	}
	var out [][]any
	for _, c := range body.Calls {
		var name, id string
		if len(c) != 3 || json.Unmarshal(c[0], &name) != nil || json.Unmarshal(c[2], &id) != nil {
			http.Error(w, "not a request", http.StatusBadRequest)
			return
		}
		var rn string
		var resp any
		switch name {
		case "MaskedEmail/get":
			rn, resp = s.get(c[1])
		case "MaskedEmail/set":
			rn, resp = s.set(c[1])
		default:
			rn, resp = "error", map[string]string{"type": "unknownMethod"}
		}
		out = append(out, []any{rn, resp, id})
	}
	writeJSON(w, map[string]any{"methodResponses": out, "sessionState": "s1"})
}

func (s *Server) get(args json.RawMessage) (string, any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var a struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "error", map[string]string{"type": "invalidArguments"}
	}
	ids := a.IDs
	if ids == nil {
		ids = s.order
	}
	list, notFound := []any{}, []string{}
	for _, id := range ids {
		if m, ok := s.items[id]; ok {
			list = append(list, m)
		} else {
			notFound = append(notFound, id)
		}
	}
	return "MaskedEmail/get", map[string]any{"accountId": accountID, "state": "1", "list": list, "notFound": notFound}
}

func (s *Server) set(args json.RawMessage) (string, any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets = append(s.sets, args)
	var a struct {
		Create  map[string]map[string]any `json:"create"`
		Update  map[string]map[string]any `json:"update"`
		Destroy []string                  `json:"destroy"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "error", map[string]string{"type": "invalidArguments", "description": err.Error()}
	}
	res := map[string]any{"accountId": accountID, "oldState": "1", "newState": "2"}
	created, notCreated := map[string]any{}, map[string]any{}
	for cid, p := range a.Create {
		if s.RateLimit {
			notCreated[cid] = map[string]string{"type": "rateLimit"}
			continue
		}
		s.next++
		id := fmt.Sprintf("masked-%d", s.next)
		prefix, _ := p["emailPrefix"].(string)
		if prefix == "" {
			prefix = "random"
		}
		m := map[string]any{
			"id": id, "email": fmt.Sprintf("%s.%d@example.com", prefix, s.next),
			"state": "pending", "forDomain": "", "description": "", "url": "",
			"createdBy": "test", "createdAt": "2026-01-02T03:04:05Z", "lastMessageAt": nil,
		}
		for k, v := range p {
			if k != "emailPrefix" {
				m[k] = v
			}
		}
		s.items[id] = m
		s.order = append(s.order, id)
		created[cid] = map[string]any{"id": id, "email": m["email"], "createdAt": m["createdAt"]}
	}
	updated, notUpdated := map[string]any{}, map[string]any{}
	for id, p := range a.Update {
		m, ok := s.items[id]
		if !ok {
			notUpdated[id] = map[string]string{"type": "notFound"}
			continue
		}
		for k, v := range p {
			m[k] = v
		}
		updated[id] = nil
	}
	notDestroyed := map[string]any{}
	for _, id := range a.Destroy {
		notDestroyed[id] = map[string]string{"type": "forbidden"}
	}
	res["created"], res["notCreated"] = created, notCreated
	res["updated"], res["notUpdated"] = updated, notUpdated
	res["destroyed"], res["notDestroyed"] = []string{}, notDestroyed
	return "MaskedEmail/set", res
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
