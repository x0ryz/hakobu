package cloudflare

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRefusedTokenSaysWhy(t *testing.T) {
	var verify string // the account's tokens/verify answer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/accounts/acc/tokens/verify":
			fmt.Fprint(w, verify)
		case "/user/tokens/verify":
			fmt.Fprint(w, `{"success":false,"errors":[{"code":1000,"message":"Invalid API Token"}]}`)
		default:
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`)
		}
	}))
	defer srv.Close()
	old := APIURL
	APIURL = srv.URL
	defer func() { APIURL = old }()

	for _, c := range []struct{ name, verify, want string }{
		{"valid", `{"success":true,"result":{"id":"t","status":"active"}}`, TokenLacksPermission},
		{"disabled", `{"success":true,"result":{"id":"t","status":"disabled"}}`, "the token is disabled"},
		{"wrong address", `{"success":false,"errors":[{"code":9109,"message":"Cannot use the access token from location: 2a01:4f9::1"}]}`, "Client IP Address Filtering"},
		{"deleted", `{"success":false,"errors":[{"code":1000,"message":"Invalid API Token"}]}`, "doesn't exist any more"},
	} {
		verify = c.verify
		err := Client{Token: "tok", AccountID: "acc"}.call("GET", "/zones", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "Authentication error") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want it to say %q", c.name, err, c.want)
		}
	}
}
