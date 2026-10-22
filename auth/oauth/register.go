package oauth

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/xaroth/speki/auth/seal"
)

// registration is what a dynamically registered client's ID holds.
type registration struct {
	Name         string   `json:"name"`
	RedirectURIs []string `json:"redirect_uris"`
}

// register handles dynamic client registration. Hosted clients use metadata documents, so only local
// programs register and only loopback redirects are accepted: a registration can't send codes elsewhere.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "the request isn't valid JSON")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 4 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "give between 1 and 4 redirect URIs")
		return
	}
	for _, ru := range req.RedirectURIs {
		if len(ru) > 200 || !isLoopback(ru) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri",
				"registered clients may only redirect to this computer (http://127.0.0.1, http://[::1] or http://localhost)")
			return
		}
	}
	name := oneLine(req.ClientName)
	if name == "" {
		name = "unnamed program"
	}
	id, err := s.keys.Seal(seal.PurposeClient, registration{Name: name, RedirectURIs: req.RedirectURIs}, time.Time{})
	if err != nil {
		s.logger.ErrorContext(r.Context(), "sealing client registration", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "registration failed")
		return
	}
	if len(id) > maxClientID { // authorize would refuse it
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "the registration is too long: use fewer or shorter redirect URIs, or a shorter name")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  id,
		"client_name":                name,
		"redirect_uris":              req.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}
