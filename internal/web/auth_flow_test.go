package web

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/auth"
)

func TestDashboardAuthLifecycle(t *testing.T) {
	const token = "fixture-only-token"
	const ttl = 100 * time.Millisecond

	server := httptest.NewTLSServer(newTestServer(t).WithAuth(auth.New([]string{token}, ttl)).Routes())
	defer server.Close()

	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	anonymous := authFlowRequest(t, client, http.MethodGet, server.URL+"/monitors", nil, "", nil)
	anonymous.Body.Close()
	if anonymous.StatusCode != http.StatusSeeOther {
		t.Fatalf("protected page without a session = %d, want redirect to login", anonymous.StatusCode)
	}
	if location := anonymous.Header.Get("Location"); !strings.HasPrefix(location, loginPath) {
		t.Fatalf("anonymous request redirected to %q, want login", location)
	}

	malformedForm := authFlowRequest(t, client, http.MethodPost, server.URL+loginPath, strings.NewReader("token=%zz"), "application/x-www-form-urlencoded", nil)
	malformedForm.Body.Close()
	if malformedForm.StatusCode != http.StatusBadRequest {
		t.Fatalf("login with a malformed form = %d, want 400", malformedForm.StatusCode)
	}
	if cookie := sessionCookieOf(t, malformedForm); cookie != nil {
		t.Fatal("malformed login form set a session cookie")
	}

	var firstFailureBody []byte
	for i, invalidToken := range []string{"invalid-credential-one", "invalid-credential-two"} {
		form := url.Values{"token": {invalidToken}}
		response := authFlowRequest(t, client, http.MethodPost, server.URL+loginPath, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", nil)
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("login with an invalid credential = %d, want 401", response.StatusCode)
		}
		if cookie := sessionCookieOf(t, response); cookie != nil {
			t.Fatal("failed login set a session cookie")
		}
		if i == 0 {
			firstFailureBody = body
		} else if !bytes.Equal(body, firstFailureBody) {
			t.Fatal("failed login response differs between invalid credentials")
		}
	}

	form := url.Values{"token": {token}}
	loginResponse := authFlowRequest(t, client, http.MethodPost, server.URL+loginPath, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", nil)
	defer loginResponse.Body.Close()
	if loginResponse.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d, want 303", loginResponse.StatusCode)
	}
	if location := loginResponse.Header.Get("Location"); location != "/" {
		t.Fatalf("login redirected to %q, want /", location)
	}
	session := sessionCookieOf(t, loginResponse)
	if session == nil {
		t.Fatal("successful login set no session cookie")
	}
	if session.Value == "" {
		t.Fatal("successful login set an empty session cookie")
	}
	if !session.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if session.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax", session.SameSite)
	}
	if !session.Secure {
		t.Error("session cookie from a TLS request is not Secure")
	}
	if session.Path != "/" {
		t.Errorf("session cookie Path = %q, want /", session.Path)
	}

	protected := authFlowRequest(t, client, http.MethodGet, server.URL+"/monitors", nil, "", session)
	protected.Body.Close()
	if protected.StatusCode != http.StatusOK {
		t.Fatalf("protected page with a live session = %d, want 200", protected.StatusCode)
	}

	logout := authFlowRequest(t, client, http.MethodPost, server.URL+logoutPath, nil, "", session)
	logout.Body.Close()
	if logout.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout = %d, want 303", logout.StatusCode)
	}
	cleared := sessionCookieOf(t, logout)
	if cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("logout did not expire the browser cookie: %v", cleared)
	}
	afterLogout := authFlowRequest(t, client, http.MethodGet, server.URL+"/monitors", nil, "", session)
	afterLogout.Body.Close()
	if afterLogout.StatusCode != http.StatusSeeOther {
		t.Fatalf("old cookie after logout = %d, want redirect to login", afterLogout.StatusCode)
	}

	loginResponse = authFlowRequest(t, client, http.MethodPost, server.URL+loginPath, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", nil)
	defer loginResponse.Body.Close()
	if loginResponse.StatusCode != http.StatusSeeOther {
		t.Fatalf("second login = %d, want 303", loginResponse.StatusCode)
	}
	expiringSession := sessionCookieOf(t, loginResponse)
	if expiringSession == nil {
		t.Fatal("second successful login set no session cookie")
	}
	validBeforeExpiry := authFlowRequest(t, client, http.MethodGet, server.URL+"/monitors", nil, "", expiringSession)
	validBeforeExpiry.Body.Close()
	if validBeforeExpiry.StatusCode != http.StatusOK {
		t.Fatalf("protected page before expiry = %d, want 200", validBeforeExpiry.StatusCode)
	}
	time.Sleep(ttl + 25*time.Millisecond)
	expired := authFlowRequest(t, client, http.MethodGet, server.URL+"/monitors", nil, "", expiringSession)
	expired.Body.Close()
	if expired.StatusCode != http.StatusSeeOther {
		t.Fatalf("protected page with an expired session = %d, want redirect to login", expired.StatusCode)
	}
}

func authFlowRequest(t *testing.T, client *http.Client, method, target string, body io.Reader, contentType string, cookie *http.Cookie) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
