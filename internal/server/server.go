// Package server exposes a Review over HTTP.
//
// Two deployments are supported by the same binary: bound to loopback and
// reached through an SSH tunnel, or bound to a LAN address behind a password,
// in the style of a Jupyter notebook server.
package server

import (
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gilramir/ai-reviewer/internal/review"
	"github.com/gilramir/ai-reviewer/web"
)

// Options configures a Server.
type Options struct {
	// Review is the review this server exposes.
	Review *review.Review
	// Auth is nil when the daemon was started with authentication disabled,
	// which main refuses to combine with a non-loopback bind address.
	Auth *Auth
	// AllowedOrigins names extra origins permitted to open a WebSocket, beyond
	// the one the page was served from. Normally empty.
	AllowedOrigins []string
	// OverTLS reports whether this server is behind TLS, which decides whether
	// the session cookie may be marked Secure.
	OverTLS bool
}

// Server routes requests to the review.
type Server struct {
	opts Options
	mux  *http.ServeMux
}

// New builds the router.
func New(opts Options) *Server {
	s := &Server{opts: opts, mux: http.NewServeMux()}

	s.mux.HandleFunc("/favicon.ico", s.handleFavicon)
	s.mux.HandleFunc("/login", s.handleLogin)
	s.mux.HandleFunc("/session", s.handleSession)
	s.mux.HandleFunc("/logout", s.handleLogout)
	s.mux.Handle("/ws", s.protect(http.HandlerFunc(s.handleWebSocket)))
	s.mux.Handle("/static/", s.protect(http.StripPrefix("/static/", http.FileServer(http.FS(web.Static())))))
	s.mux.Handle("/dist/", s.protect(http.StripPrefix("/dist/", http.FileServer(http.FS(web.Dist())))))
	s.mux.Handle(review.AssetRoute, s.protect(http.HandlerFunc(s.handleFile)))
	s.mux.Handle("/", s.protect(http.HandlerFunc(s.handleIndex)))

	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// protect refuses anything without a live session. Authentication is on by
// default; disabling it is only permitted on a loopback bind.
func (s *Server) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Auth == nil || s.opts.Auth.Authenticated(r) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Upgrade") != "" {
			http.Error(w, "unauthorised", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

// handleSession reports whether the caller still has a live session, as a
// status code rather than a redirect.
//
// The browser needs this because the WebSocket API does not expose the
// handshake's HTTP status: a rejected upgrade and an unreachable server look
// identical from JavaScript. Without it a client whose session died cannot tell
// that reconnecting will never succeed, and retries forever while every click
// silently does nothing.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.opts.Auth == nil || s.opts.Auth.Authenticated(r) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, "unauthorised", http.StatusUnauthorized)
}

// handleFavicon serves web/static/favicon.ico, if there is one.
//
// Unprotected, unlike everything else under /static/: browsers ask for this on
// the login page too, and a session check there answers a request for an icon
// with a redirect to the page the browser is already looking at. An icon is not
// worth a login, and it says nothing about the review.
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	data, err := web.Static().Open("favicon.ico")
	if err != nil {
		// No icon was built in. The browser asks once and gives up.
		http.NotFound(w, r)
		return
	}
	defer data.Close()

	file, ok := data.(readSeeker)
	if !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "image/x-icon")
	w.Header().Set("Cache-Control", "max-age=3600")
	http.ServeContent(w, r, "favicon.ico", time.Time{}, file)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := web.Static().Open("index.html")
	if err != nil {
		http.Error(w, "missing index.html; run `make web`", http.StatusInternalServerError)
		return
	}
	defer data.Close()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "index.html", time.Time{}, data.(readSeeker))
}

// readSeeker is what embed.FS files satisfy; naming it keeps the assertion in
// handleIndex readable.
type readSeeker interface {
	Read([]byte) (int, error)
	Seek(int64, int) (int64, error)
}

// --- login ------------------------------------------------------------------

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.opts.Auth == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.renderLogin(w, "")

	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			s.renderLogin(w, "Could not read the form.")
			return
		}
		if !s.opts.Auth.Check(r.PostFormValue("password")) {
			// Deliberately vague, and slow enough to make scripted guessing
			// tedious without being noticeable to a person.
			time.Sleep(time.Second)
			s.renderLogin(w, "That password was not accepted.")
			return
		}
		http.SetCookie(w, s.opts.Auth.Issue(s.overTLS(r)))
		http.Redirect(w, r, "/", http.StatusSeeOther)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.opts.Auth != nil {
		s.opts.Auth.Revoke(r)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// overTLS reports whether this particular request arrived over TLS. A cookie
// marked Secure is dropped outright on a plain-HTTP LAN address, which looks
// exactly like a login page that never accepts the password.
func (s *Server) overTLS(r *http.Request) bool {
	return s.opts.OverTLS || r.TLS != nil
}

var loginPage = template.Must(template.New("login").Parse(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ai-reviewer</title>
<style>
 body { font: 15px/1.5 system-ui, sans-serif; display: grid; place-items: center;
        min-height: 100vh; margin: 0; background: #fbfaf7; color: #1c1b19; }
 form { background: #fff; border: 1px solid #e3ded4; border-radius: 10px;
        padding: 1.5rem; width: min(22rem, 90vw); }
 h1 { font-size: 1rem; margin: 0 0 1rem; font-weight: 600; }
 input { width: 100%; padding: 0.5rem; font: inherit; border: 1px solid #e3ded4;
         border-radius: 6px; margin-bottom: 0.75rem; }
 button { width: 100%; padding: 0.5rem; font: inherit; border: 0;
          border-radius: 6px; background: #7a4b1e; color: #fff; cursor: pointer; }
 .err { color: #a3341f; margin: 0 0 0.75rem; font-size: 0.875rem; }
 @media (prefers-color-scheme: dark) {
   body { background: #16151a; color: #e8e5df; }
   form { background: #1d1c22; border-color: #33313a; }
   input { background: #16151a; color: #e8e5df; border-color: #33313a; }
 }
</style>
<form method="post" action="/login">
  <h1>ai-reviewer</h1>
  {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
  <input type="password" name="password" placeholder="Password" autofocus
         autocomplete="current-password">
  <button type="submit">Open review</button>
</form>
`))

func (s *Server) renderLogin(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = loginPage.Execute(w, struct{ Error string }{message})
}

// --- origin checking --------------------------------------------------------

// originAllowed decides whether a WebSocket handshake may proceed.
//
// This is the one check that genuinely matters on a LAN bind. WebSocket
// handshakes are exempt from CORS and the browser sends the session cookie on a
// cross-origin upgrade, so without it any page the reviewer happens to visit
// could open a socket to the daemon and drive Claude against their documents.
//
// A missing Origin header is refused rather than allowed. Browsers always send
// one; only non-browser clients omit it, and those are not what this daemon is
// for.
func (s *Server) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}

	if strings.EqualFold(parsed.Host, r.Host) {
		return true
	}
	for _, allowed := range s.opts.AllowedOrigins {
		if strings.EqualFold(allowed, origin) || strings.EqualFold(allowed, parsed.Host) {
			return true
		}
	}
	return false
}

// IsLoopback reports whether an address binds only to the local machine. Used
// by main to refuse the combination of a public bind and disabled auth.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		// An empty host in a listen address means every interface.
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DisplayURL renders the address a person should open.
func DisplayURL(addr string, overTLS bool) string {
	scheme := "http"
	if overTLS {
		scheme = "https"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Sprintf("%s://%s/", scheme, addr)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = firstLANAddress()
	}
	return fmt.Sprintf("%s://%s/", scheme, net.JoinHostPort(host, port))
}

// firstLANAddress guesses the address a reviewer on the same network would use.
func firstLANAddress() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "localhost"
	}
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			return ip4.String()
		}
	}
	return "localhost"
}
