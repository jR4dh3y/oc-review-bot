package server

import (
	"errors"
	"net/http"

	"github.com/jR4dh3y/samik-bot/internal/orca"
	"github.com/jR4dh3y/samik-bot/internal/store"
)

// partnerJSON is the admin-facing view of the deployment's OrcaRouter partner
// dashboard facts: the referral identity, the connect flow, and where the
// gateway constants live.
type partnerJSON struct {
	ReferralCode           string `json:"referral_code"`
	ReferralURL            string `json:"referral_url"`
	AppName                string `json:"app_name"`
	CallbackURL            string `json:"callback_url"`
	APIBaseURL             string `json:"api_base_url"`
	ListingRepo            string `json:"listing_repo"`
	ConnectScript          string `json:"connect_script"`
	ConnectScriptIntegrity string `json:"connect_script_integrity"`
}

const partnerRedirectPath = "/admin/partner"

func (s *Server) handlePartnerInfo(u *store.User, w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.partnerInfo())
}

func (s *Server) partnerInfo() partnerJSON {
	return partnerJSON{
		ReferralCode:           s.orca.ReferralCode,
		ReferralURL:            orca.WebBaseURL + "/ref/" + s.orca.ReferralCode,
		AppName:                s.orca.AppName,
		CallbackURL:            s.orca.CallbackURL,
		APIBaseURL:             orca.APIBaseURL,
		ListingRepo:            "jr4dh3y/samik-bot",
		ConnectScript:          orcaConnectScriptURL,
		ConnectScriptIntegrity: orcaConnectSRI,
	}
}

// orcaConnectScriptURL is the drop-in button loader from the partner
// integration guide. The pinned integrity value keeps the bytes verifiable.
const (
	orcaConnectScriptURL = orca.APIBaseURL + "/cdn/orca-connect-v1.js"
	orcaConnectSRI       = "sha384-lYB7XPuzXDvjiYcgcpW5cOrdR5S6U+hWubqNlaQ9hPadOPq8ecUyz+oMICcCCzVM"
)

// handleOrcaConnectURL mints one PKCE authorization URL for the drop-in
// button. The endpoint answers with {auth_url} exactly as the integration
// guide's data-endpoint contract requires; the verifier stays server-side.
func (s *Server) handleOrcaConnectURL(u *store.User, w http.ResponseWriter, r *http.Request) {
	authURL, err := s.orca.ConnectURL()
	if err != nil {
		s.log.Error("mint orca connect URL", "err", err)
		writeErr(w, http.StatusInternalServerError, "could not start the connect flow")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"auth_url": authURL})
}

// handleOrcaCallback completes the connect flow: the state check proves the
// redirect belongs to an attempt this server minted, then the code is
// exchanged for an API key which joins the pooled gateway credentials.
func (s *Server) handleOrcaCallback(w http.ResponseWriter, r *http.Request) {
	if s.orca == nil || s.st == nil {
		http.Redirect(w, r, partnerRedirectPath+"?error=unavailable", http.StatusFound)
		return
	}
	key, err := s.orca.Exchange(r.Context(), r.URL.Query().Get("code"), r.URL.Query().Get("state"))
	if err != nil {
		if errors.Is(err, orca.ErrUnknownState) {
			s.log.Warn("orca connect callback rejected", "cause", "unknown_state")
			http.Redirect(w, r, partnerRedirectPath+"?error=state", http.StatusFound)
			return
		}
		s.log.Warn("orca connect exchange failed", "err", err)
		http.Redirect(w, r, partnerRedirectPath+"?error=exchange", http.StatusFound)
		return
	}
	if _, err := s.st.AddKey("orcarouter-connect", key); err != nil {
		s.log.Error("store connected orca key", "err", err)
		http.Redirect(w, r, partnerRedirectPath+"?error=store", http.StatusFound)
		return
	}
	s.log.Info("orca connect key added to the pool")
	http.Redirect(w, r, partnerRedirectPath+"?connected=1", http.StatusFound)
}
