package main

import "net/http"

// cfAccessHeaders builds the two Cloudflare Access service-token headers
// CANT-109 exists to add. The deployed Catenary router sits behind Access
// (construct-server/config/traefik/dynamic/routers.yml, the catenary
// router's AUD in CF_ACCESS_AUD_MAP), which refuses a request before it ever
// reaches the service unless it carries these — on the upgrade exactly as
// much as on an ordinary HTTP request, because an upgrade IS one.
//
// nil WHEN BOTH ARE EMPTY, which is the correct client.Config.ExtraHeaders
// for a local server: Access does not gate it, and ExtraHeaders is nil-safe
// on both the /sync path and the WebSocket dial. Set independently — a
// caller that has only one of the two still gets that one header rather than
// silently getting neither, which is what a caller debugging a partially
// configured environment needs to see in the gate's own refusal.
func cfAccessHeaders(clientID, clientSecret string) http.Header {
	if clientID == "" && clientSecret == "" {
		return nil
	}
	h := http.Header{}
	if clientID != "" {
		h.Set("CF-Access-Client-Id", clientID)
	}
	if clientSecret != "" {
		h.Set("CF-Access-Client-Secret", clientSecret)
	}
	return h
}
