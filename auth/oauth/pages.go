package oauth

import (
	"embed"
	"html/template"
	"net/http"
)

//go:embed pages/*.html pages/fonts/*.woff2
var pages embed.FS

var templates = template.Must(template.ParseFS(pages, "pages/*.html"))

// pageCSP keeps the pages from loading anything but speki's fonts, or being framed. It has no
// form-action: browsers apply it to the redirects after the consent form (to GitHub, or back to the
// client on Cancel), and the client's address isn't known in advance.
const pageCSP = "default-src 'none'; style-src 'unsafe-inline'; font-src 'self'; frame-ancestors 'none'"

func (s *Server) page(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", pageCSP)
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	err := templates.ExecuteTemplate(w, name, data)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "rendering page", "page", name, "err", err)
	}
}

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	s.errorPageAs(w, r, status, title, detail, "")
}

// errorPageAs is errorPage after GitHub sign-in: it says which GitHub account was used, so a wrong one
// is obvious.
func (s *Server) errorPageAs(w http.ResponseWriter, r *http.Request, status int, title, detail, login string) {
	step := 1 // choosing access
	if r.URL.Path == callbackPath {
		step = 2 // signing in with GitHub
	}
	s.page(w, r, status, "error.html", map[string]any{
		"Name":   s.title(),
		"Step":   step,
		"Failed": true,
		"Title":  title,
		"Detail": detail,
		"Login":  login,
	})
}

// font serves a typeface the pages use. The fonts are in the binary, so they only change with it.
func font(w http.ResponseWriter, r *http.Request) {
	data, err := pages.ReadFile("pages/fonts/" + r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "font/woff2")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

func (s *Server) title() string {
	if s.cfg.Title == "" {
		return "speki"
	}
	return s.cfg.Title
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.ErrorContext(r.Context(), "sign-in failed", "err", err)
	s.errorPage(w, r, http.StatusInternalServerError, "Something went wrong.", "Try again in a moment.")
}
