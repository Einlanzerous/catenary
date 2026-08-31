// Package fakecatenary is a stand-in for the Catenary admin API that does not
// exist yet — R6 (IDEA-29).
//
// It models the one thing that makes Catenary a different SHAPE from Purser's
// existing four connectors: an account owns a list of devices, each with its
// own individually revocable refresh token. That is the structure the gate is
// asking about, so it is the structure the fake has.
//
// Fault injection is deliberate. The interesting question about Deprovision is
// not "does it work" but "what does a HALF-COMPLETED revoke leave behind, and
// can Purser see it?" — which needs a device revoke that fails on demand.
package fakecatenary

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Device is one registered client install. D2 makes the device the unit of
// credential and of revocation.
type Device struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Revoked bool   `json:"revoked"`
}

// Account is the person's Catenary identity. Status is account-level and is
// deliberately distinct from "has any live device": a person with zero live
// devices but an active account can still enrol a new one, so they still have
// access.
type Account struct {
	ID          string   `json:"id"`
	Email       string   `json:"email"`
	DisplayName string   `json:"display_name"`
	Status      string   `json:"status"` // "active" | "disabled"
	Devices     []Device `json:"devices"`
}

// Server is the in-memory upstream.
type Server struct {
	mu       sync.Mutex
	accounts map[string]*Account // by lowercased email
	nextID   int

	// FailRevokeDevice, when set, makes a revoke of that device id return 500.
	// This is how the test reaches the partial-failure state that the whole
	// finding turns on.
	FailRevokeDevice string

	// Calls records every request path in order, so a test can assert the
	// ORDER of operations and not merely the end state.
	Calls []string
}

func New() *Server {
	return &Server{accounts: map[string]*Account{}}
}

// Seed adds an account with pre-registered devices, standing in for a person
// who has been using Catenary for a while.
func (s *Server) Seed(email, name string, devices ...string) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	a := &Account{
		ID: fmt.Sprintf("acct_%d", s.nextID), Email: strings.ToLower(email),
		DisplayName: name, Status: "active",
	}
	for i, d := range devices {
		a.Devices = append(a.Devices, Device{ID: fmt.Sprintf("%s_dev%d", a.ID, i+1), Name: d})
	}
	s.accounts[a.Email] = a
	return a
}

// Lookup returns the live account, for assertions.
func (s *Server) Lookup(email string) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accounts[strings.ToLower(email)]
}

func (s *Server) record(p string) {
	s.Calls = append(s.Calls, p)
}

// Handler routes the admin API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/admin/accounts", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		switch r.Method {
		case http.MethodGet:
			email := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("email")))
			s.record("GET /admin/accounts?email=" + email)
			a, ok := s.accounts[email]
			if !ok {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			writeJSON(w, http.StatusOK, a)

		case http.MethodPost:
			var body struct {
				Email       string `json:"email"`
				DisplayName string `json:"display_name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			email := strings.ToLower(strings.TrimSpace(body.Email))
			s.record("POST /admin/accounts " + email)
			if _, exists := s.accounts[email]; exists {
				http.Error(w, `{"error":"already exists"}`, http.StatusConflict)
				return
			}
			s.nextID++
			a := &Account{
				ID: fmt.Sprintf("acct_%d", s.nextID), Email: email,
				DisplayName: body.DisplayName, Status: "active",
			}
			s.accounts[email] = a
			// ONE enrolment token, not a list. The person has no devices yet;
			// per-device credentials are minted by Catenary when a device
			// redeems this, which is the reason the mismatch the gate worried
			// about does not reach the connector boundary.
			writeJSON(w, http.StatusCreated, map[string]any{
				"account": a, "enrolment_token": "ctn_enrol_" + a.ID,
			})

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// /admin/accounts/{id}/... — enrolment, disable, device revoke.
	mux.HandleFunc("/admin/accounts/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/accounts/"), "/"), "/")
		id := parts[0]
		var acct *Account
		for _, a := range s.accounts {
			if a.ID == id {
				acct = a
				break
			}
		}
		if acct == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}

		switch {
		case len(parts) == 2 && parts[1] == "enrolment" && r.Method == http.MethodPost:
			s.record("POST /admin/accounts/" + id + "/enrolment")
			writeJSON(w, http.StatusOK, map[string]any{"enrolment_token": "ctn_enrol_" + id + "_reissued"})

		case len(parts) == 2 && parts[1] == "disable" && r.Method == http.MethodPost:
			s.record("POST /admin/accounts/" + id + "/disable")
			acct.Status = "disabled"
			w.WriteHeader(http.StatusNoContent)

		case len(parts) == 4 && parts[1] == "devices" && parts[3] == "revoke" && r.Method == http.MethodPost:
			did := parts[2]
			s.record("POST /admin/accounts/" + id + "/devices/" + did + "/revoke")
			if did == s.FailRevokeDevice {
				http.Error(w, `{"error":"upstream boom"}`, http.StatusInternalServerError)
				return
			}
			for i := range acct.Devices {
				if acct.Devices[i].ID == did {
					acct.Devices[i].Revoked = true
				}
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})

	return mux
}

// Start runs the fake on an httptest server.
func (s *Server) Start() *httptest.Server { return httptest.NewServer(s.Handler()) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
