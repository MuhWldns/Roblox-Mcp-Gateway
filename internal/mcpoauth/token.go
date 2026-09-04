package mcpoauth

import (
	"net/http"

	"github.com/ory/fosite"
)

// TokenHTTP serves POST /oauth/token. It seeds the presented binding into
// the adapter flow before handing the request to the composed fosite
// authorization-code and refresh handlers.
func (p *Provider) TokenHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil && err != http.ErrNotMultipart {
		writeProviderError(w, http.StatusBadRequest, fosite.ErrInvalidRequest.
			WithHint("Unable to parse the token request body."))
		return
	}
	flow := &tokenFlow{
		presentedClientID:    r.PostFormValue("client_id"),
		presentedRedirectURI: r.PostFormValue("redirect_uri"),
		presentedResource:    r.PostFormValue("resource"),
	}
	ctx := withFlow(r.Context(), flow)

	ar, err := p.fosite.NewAccessRequest(ctx, r, &fosite.DefaultSession{})
	if err != nil {
		p.fosite.WriteAccessError(ctx, w, ar, err)
		return
	}
	resp, err := p.fosite.NewAccessResponse(ctx, ar)
	if err != nil {
		p.fosite.WriteAccessError(ctx, w, ar, err)
		return
	}
	p.fosite.WriteAccessResponse(ctx, w, ar, resp)
}
