package mcpoauth

import (
	"net/http"
)

// RevocationHTTP serves POST /oauth/revoke with RFC 7009 semantics: unknown
// tokens and foreign clients revoke silently with 200, and a refresh token
// revocation invalidates its whole family, its grant, and the grant's access
// tokens.
func (p *Provider) RevocationHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := p.fosite.NewRevocationRequest(ctx, r); err != nil {
		p.fosite.WriteRevocationResponse(ctx, w, err)
		return
	}
	p.fosite.WriteRevocationResponse(ctx, w, nil)
}
