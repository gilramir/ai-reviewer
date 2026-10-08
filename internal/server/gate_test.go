package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The gate is what stands between the LAN and a log of every file the model
// read: nothing behind it without the password, a page to log in on, and an
// API that answers 401 rather than a login page the viewer cannot decode.
func TestTheGateKeepsOutWhoeverHasNoPassword(t *testing.T) {
	const secret = "gate-secret"
	behind := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("the log"))
	})
	ts := httptest.NewServer(Gate(NewTokenAuth(secret).WithCookie("log_session"), "log", behind))
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, err := client.Get(ts.URL + "/")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/login", resp.Header.Get("Location"))

	resp, err = client.Get(ts.URL + "/api/frames")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp, err = client.PostForm(ts.URL+"/login", url.Values{"password": {secret}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, "the password was refused")
	cookies := resp.Cookies()
	require.Len(t, cookies, 1)
	assert.Equal(t, "log_session", cookies[0].Name,
		"the gate's session shares the review's cookie, so logging in to one logs out of the other")

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/frames", nil)
	req.AddCookie(cookies[0])
	resp, err = client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
