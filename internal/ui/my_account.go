package ui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/persistence"
)

// /ui/account — the SELF-SERVICE identity surface, oidc-identity-permissions
// design §5.5's "Linked identities" panel.
//
// This is the one identity page that needs neither an operator capability nor
// the admin surface, which is why it works identically in Community and
// Enterprise: identity is a CE feature, and a page that required
// /ui/admin/* would be absent on the very edition it ships in. It is also the
// only screen on which a person can see EVERYTHING that resolves to them —
// their IdP login, their chat channels and their claimed API keys — which is
// the point of collapsing those onto one `users.id`.

// SessionUserResolver answers "who is signed in on this request", returning
// "" when nobody is. Injected rather than read from a context key because
// api's identity key is unexported, and because injecting it lets the page be
// tested without standing up the auth chain.
type SessionUserResolver func(*http.Request) string

// WithSessionUserResolver wires the resolver. Unset means nobody is ever
// signed in, so the page refuses — which is the safe default for a surface
// whose whole content is "your bindings".
func WithSessionUserResolver(f SessionUserResolver) ServerOption {
	return func(s *Server) { s.sessionUser = f }
}

// WithSlackLinkCommands names EVERY Slack slash command this daemon answers,
// so the panel can tell a person what to type. Empty — the default — omits
// the Slack line entirely, because naming a command a deployment does not
// answer is the same defect as documenting a behaviour nothing implements.
//
// A slice, not one command: a deployment can run several Slack apps (an
// operator here runs /t800 and /holly), and naming whichever loaded first
// tells most of them the wrong thing.
func WithSlackLinkCommands(cmds []string) ServerOption {
	return func(s *Server) {
		s.slackLinkCommands = nil
		for _, c := range cmds {
			if c = strings.TrimSpace(c); c != "" {
				s.slackLinkCommands = append(s.slackLinkCommands, c)
			}
		}
	}
}

// WithDefaultSessionUserResolver wires the production resolver, which reads
// the chain-resolved identity's session user id. Separate from the option
// above so tests can inject a fixed user without standing up the auth chain.
func WithDefaultSessionUserResolver() ServerOption {
	return WithSessionUserResolver(defaultSessionUserResolver)
}

// defaultSessionUserResolver reads the chain-resolved identity's session user
// id. This is the production path; the container wires it.
func defaultSessionUserResolver(r *http.Request) string {
	id := api.IdentityFromContext(r.Context())
	if id == nil || id.Extra == nil {
		return ""
	}
	uid, _ := id.Extra[auth.ExtraSessionUserID].(string)
	return uid
}

// MyAccountData backs my_account.html.
type MyAccountData struct {
	adminCommonData
	Available  bool
	UserID     string
	Display    string
	Role       string
	Projects   []string
	Identities []myIdentityRow
	Notice     string
	// IssuedCode is a freshly minted link code, rendered inline exactly once.
	// It is never placed in a URL, a redirect, a log line or an audit row —
	// the four places a "shown once" secret usually escapes to.
	IssuedCode string
	// IssuedCodeID and IssuedCodeExpiresAt drive §5.5's redemption-observation
	// poll. The ID is a hash prefix, not the code: it may travel in a polling
	// URL, which the code never may.
	IssuedCodeID        string
	IssuedCodeExpiresAt string
	// SlackLinkCommands is every Slack slash command this daemon answers,
	// empty when Slack is not wired. The panel names a redemption command
	// only when that command exists here: the Slack one is CONFIGURABLE
	// (slack.slash_command), so a hardcoded "/vornik link" would tell some
	// operators to type something their workspace does not answer — and
	// naming only the FIRST tells everyone running a second app the same.
	SlackLinkCommands []string
	// LinkCodeTTLMinutes is the code lifetime the page quotes, derived from
	// authz.LinkCodeTTL rather than written again — a second literal is a
	// number that drifts from the one the service enforces, and the drift
	// shows as a page promising ten minutes for a code expired in five.
	LinkCodeTTLMinutes int
}

// myIdentityRow is one binding, rendered with the vocabulary a person
// recognises rather than the column values.
type myIdentityRow struct {
	Channel    string
	ExternalID string
	Display    string
	// Kind is the human label: "Login", "Chat", "API key". A person does not
	// think of their Telegram account and their API key as the same kind of
	// thing, even though both are rows in one table.
	Kind string
}

// noPersonalLoginNotice is what a deployment with no session-login backend
// tells someone who reaches a self-service page.
//
// The self-service account surface — sign in, self-link a chat identity,
// self-claim a key, unlink — requires a browser session, and production
// session-login wiring comes exclusively from the enterprise identity
// provider. A Community deployment constructs none, so an ordinary CE user
// cannot reach any of it; owning an API key is not a browser session. These
// pages answered "sign in to see your account", sending that person to look
// for a sign-in that does not exist on their edition (audit 2026-09-15
// CA-12).
//
// This does NOT implement CE personal login. That is an unresolved
// prerequisite (identity review amendment R3) and inventing a login
// mechanism is not an audit's job. It replaces a remedy that cannot be
// followed with an accurate statement of what is missing — a documented
// behaviour nothing implements is worse than an absent one, because it stops
// the next person looking.
const noPersonalLoginNotice = "Personal sign-in is not available on this deployment: no browser session backend is configured, so the self-service account pages cannot be used. An operator can still link identities and assign keys from the admin account pages."

// personalLoginAvailable reports whether this deployment has any path to a
// browser session at all.
func (s *Server) personalLoginAvailable() bool { return s != nil && s.sessionUser != nil }

// MyAccount renders GET /ui/account.
func (s *Server) MyAccount(w http.ResponseWriter, r *http.Request) {
	if !s.personalLoginAvailable() {
		http.Error(w, noPersonalLoginNotice, http.StatusNotImplemented)
		return
	}
	userID := s.sessionUser(r)
	if userID == "" {
		http.Error(w, "sign in to see your account", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	s.render(w, "my_account.html", s.myAccountData(ctx, r, userID))
}

// buildMyIdentityRows labels each binding by what it IS to the person.
//
// An api_key binding is named from the key record rather than from the stored
// display, which until 2026-09-16 was a copy of the key id — so this page,
// the one place §5.5 promises shows every identity that resolves to you,
// listed your keys by the single identifier §5.4 guarantees you do not have
// (operator report 2026-09-16). Resolved per binding rather than from a
// listing: a personal page has no business enumerating the whole deployment's
// keys, and a person holds a handful.
func (s *Server) buildMyIdentityRows(ctx context.Context, in []persistence.UserIdentityRef) []myIdentityRow {
	out := make([]myIdentityRow, 0, len(in))
	for _, i := range in {
		display := i.Display
		if i.Channel == "api_key" && keyDisplayIsOpaque(display, i.ExternalID) {
			// "" when the key cannot be named; the template then falls back
			// to the id, which is worse than a name and better than a
			// vanished binding.
			display = s.nameKeyIdentity(ctx, i.ExternalID)
		}
		out = append(out, myIdentityRow{
			Channel:    i.Channel,
			ExternalID: i.ExternalID,
			Display:    display,
			Kind:       identityKindLabel(i.Channel),
		})
	}
	return out
}

func identityKindLabel(channel string) string {
	switch channel {
	case "github", "google", "microsoft", "gitlab":
		return "Login"
	case "telegram", "slack", "email":
		return "Chat"
	case "api_key":
		return "API key"
	default:
		return channel
	}
}

// MyAccountAction handles the panel's three POST forms. They call the same
// authz.Accounts methods the API routes do — §5.5 requires every surface to
// route through one service method per operation, so the UI must not grow its
// own policy.
//
// CSRF: these are cookie-authed mutating requests, so they are covered by the
// header-based ladder in api's middleware (Sec-Fetch-Site, falling back to
// Origin, failing closed when neither is present). There is no per-form token
// in this codebase, and inventing one here would be a second mechanism for a
// property the middleware already enforces uniformly.
func (s *Server) MyAccountAction(w http.ResponseWriter, r *http.Request) {
	if !s.personalLoginAvailable() {
		http.Error(w, noPersonalLoginNotice, http.StatusNotImplemented)
		return
	}
	userID := s.sessionUser(r)
	if userID == "" {
		http.Error(w, "sign in to manage your account", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.accounts == nil {
		http.Error(w, "identity core not wired", http.StatusServiceUnavailable)
		return
	}
	_ = r.ParseForm()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	actor := authz.Actor{Principal: "session:" + userID, Source: "ui", UserAgent: r.UserAgent()}

	var notice string
	switch {
	case strings.HasSuffix(r.URL.Path, "/link-code"):
		// RENDERED INLINE, never redirected. A freshly issued code is a
		// one-time secret, and a redirect would put it in the query string —
		// where it lands in browser history, in the Referer header of every
		// request the page subsequently makes, and in proxy and server access
		// logs. The first cut of this handler did exactly that (found by
		// security review, 2026-09-14): the storage hygiene of §5.2 was
		// intact, and the TRANSPORT leaked it anyway.
		s.renderIssuedLinkCode(ctx, w, r, userID, actor)
		return
	case strings.HasSuffix(r.URL.Path, "/claim-key"):
		err := s.accounts.ClaimKey(ctx, userID, r.FormValue("keyId"), r.FormValue("keySecret"), actor)
		switch {
		case err == nil:
			notice = "Key claimed. Its recorded history now attributes to you."
		case errors.Is(err, authz.ErrKeyClaimRateLimited):
			// The §5.4 attempt bound. This form used to reach ClaimKey with
			// no bound at all while the REST doors had one, so a caller
			// could exhaust the API bucket and carry on here (audit
			// 2026-09-15 CA-09). The bound now lives in the service; this
			// branch is how a human sees it.
			notice = "Too many claim attempts for that key. Try again later."
		case errors.Is(err, authz.ErrKeyClaimRefused):
			// The same single refusal the API returns: a caller must not learn
			// whether the key exists or is already claimed (§5.4).
			notice = "Key claim refused."
		default:
			notice = "could not claim the key: " + err.Error()
		}
	case strings.HasSuffix(r.URL.Path, "/unlink"):
		if err := s.accounts.Unlink(ctx, userID, r.FormValue("channel"), r.FormValue("externalId"), actor); err != nil {
			notice = "could not unlink: " + err.Error()
		} else {
			notice = "Unlinked."
		}
	default:
		http.Error(w, "unknown account action", http.StatusNotFound)
		return
	}
	http.Redirect(w, r, "/ui/account?notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

// renderIssuedLinkCode issues a code and renders the page with it, without a
// redirect, so the code never reaches a URL.
//
// The cost is that a browser refresh re-POSTs and issues a second code. That
// is acceptable and arguably correct: codes are single-use and expire in ten
// minutes, so a spare one is harmless, whereas the post-redirect-get that
// would avoid it is precisely what leaked the first one.
func (s *Server) renderIssuedLinkCode(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string, actor authz.Actor) {
	code, err := s.accounts.IssueLinkCode(ctx, userID, actor)
	data := s.myAccountData(ctx, r, userID)
	if err != nil {
		data.Notice = "could not issue a link code: " + err.Error()
	} else {
		data.IssuedCode = code
		data.IssuedCodeID = authz.LinkCodeID(code)
		data.IssuedCodeExpiresAt = time.Now().UTC().Add(authz.LinkCodeTTL).Format(time.RFC3339)
	}
	// A page displaying a one-time secret must not be cached, and must not
	// hand the code's page URL to anything it loads.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.render(w, "my_account.html", data)
}

// myAccountData assembles the page model, shared by the GET handler and the
// inline link-code render so the two cannot drift.
func (s *Server) myAccountData(ctx context.Context, r *http.Request, userID string) MyAccountData {
	data := MyAccountData{
		SlackLinkCommands:  s.slackLinkCommands,
		LinkCodeTTLMinutes: int(authz.LinkCodeTTL / time.Minute),
		adminCommonData:    adminCommonData{Title: "My account", CurrentPage: "my-account"},
		Available:          s.accounts != nil,
		UserID:             userID,
		Notice:             r.URL.Query().Get("notice"),
	}
	if !data.Available {
		return data
	}
	v, err := s.accounts.Get(ctx, userID)
	if err != nil {
		s.logger.Warn().Err(err).Msg("my account: load failed")
		data.Notice = "could not load your account: " + err.Error()
		return data
	}
	data.Display, data.Role, data.Projects = v.DisplayName, v.Role, v.Projects
	data.Identities = s.buildMyIdentityRows(ctx, v.Identities)
	return data
}
