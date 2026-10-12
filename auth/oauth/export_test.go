package oauth

import "net/http"

// SetMetadataTransport sends client metadata fetches through rt instead of the network, which tests can't
// reach: the real transport refuses every address that isn't public.
func SetMetadataTransport(s *Server, rt http.RoundTripper) {
	s.clients.fetch = newMetadataFetcher(s.clients.hosts, rt)
}
