package web

import (
	"net/http"

	"github.com/rahuldr07/uptime-monitor/internal/store"
)

// The Settings sections. Each is its own page; they share one template and a
// sub-navigation.
const (
	secProfile       = "profile"
	secSecurity      = "security"
	secNotifications = "notifications"
	secDanger        = "danger"
)

type accountProvider struct {
	Name, Label string
	Enabled     bool // can be started from this instance
	Connected   bool
	Email       string // the provider's email at link time
}

type settingsData struct {
	base
	alertsData  // the Notifications section; empty elsewhere
	Section     string
	CanDelete   bool // operators cannot delete their account here
	HasPassword bool
	MailOn      bool // the server can email links: a first password is set through one
	LinkSent    bool // a link was just emailed (?sent=1): the Password card says to check the inbox
	Providers   []accountProvider
	SignInLabel string      // the connected providers, for "You sign in with …"
	SoleMethod  bool        // the only way to sign in is one provider: it cannot be disconnected
	Recent      bool        // auth_at is within the window
	NeedReauth  bool        // no password and no recent sign-in: sensitive forms are replaced by reauth links
	Reauth      []oauthLink // one per connected, enabled provider
	Zones       []zoneOption
	ZoneNow     string // the zone in use, e.g. "Asia/Kolkata (UTC+05:30)"
}

// settingsBase is the part of the view model every section needs.
func (s *Server) settingsBase(r *http.Request, a *authCtx, section string) settingsData {
	return settingsData{base: s.base(r, a, "settings", sectionTitle[section]+" · Settings"), Section: section, CanDelete: !a.User.IsOperator}
}

var sectionTitle = map[string]string{secProfile: "Profile", secSecurity: "Security", secNotifications: "Notifications", secDanger: "Danger zone"}

// signInLabel names the connected providers for "You sign in with …".
func signInLabel(ids []store.Identity) string {
	out := ""
	for _, name := range []string{"google", "github"} {
		for _, i := range ids {
			if i.Provider == name {
				if out != "" {
					out += " or "
				}
				out += providerLabel(name)
			}
		}
	}
	return out
}

// settingsView builds the view model of the Profile, Security and Danger zone
// sections. Errors from a POST re-render the section the form lives in.
func (s *Server) settingsView(r *http.Request, a *authCtx, section string) (settingsData, error) {
	ids, err := s.myIdentities(r.Context(), a.User.ID)
	if err != nil {
		return settingsData{}, err
	}
	d := s.settingsBase(r, a, section)
	d.HasPassword, d.MailOn = a.HasPassword, s.mail != nil
	d.LinkSent = d.MailOn && !d.HasPassword && r.URL.Query().Get("sent") == "1"
	d.Zones, d.ZoneNow = zoneOptions(a.User, d.Now), zoneLabel(store.CanonicalZone(a.User.Location().String()), d.Now)
	d.Recent = s.recentAuth(a)
	d.NeedReauth, d.Reauth = s.reauthState(a, ids, "/settings/"+section)
	d.SoleMethod = !a.HasPassword && len(ids) == 1
	d.SignInLabel = signInLabel(ids)
	for _, name := range []string{"google", "github"} {
		p := accountProvider{Name: name, Label: providerLabel(name), Enabled: s.oauth[name] != nil}
		for _, i := range ids {
			if i.Provider == name {
				p.Connected, p.Email = true, i.Email
			}
		}
		if p.Enabled || p.Connected {
			d.Providers = append(d.Providers, p)
		}
	}
	return d, nil
}

// settingsPage serves one of the sections that need no data of their own.
func (s *Server) settingsPage(section string) func(http.ResponseWriter, *http.Request, *authCtx) {
	return func(w http.ResponseWriter, r *http.Request, a *authCtx) {
		// Operators cannot delete their account here: no dead-end page.
		if section == secDanger && a.User.IsOperator {
			http.Redirect(w, r, "/settings/profile", http.StatusFound)
			return
		}
		d, err := s.settingsView(r, a, section)
		if err != nil {
			s.fail(w, err)
			return
		}
		s.render(w, r, "settings", d)
	}
}

func (s *Server) settingsIndex(w http.ResponseWriter, r *http.Request, _ *authCtx) {
	http.Redirect(w, r, "/settings/profile", http.StatusSeeOther)
}

// Old pages. 302, not 301: a browser keeps a 301 forever and these are
// temporary aliases.
func (s *Server) accountRedirect(w http.ResponseWriter, r *http.Request, _ *authCtx) {
	http.Redirect(w, r, "/settings/security", http.StatusFound)
}

func (s *Server) alertsRedirect(w http.ResponseWriter, r *http.Request, _ *authCtx) {
	http.Redirect(w, r, "/settings/notifications", http.StatusFound)
}
