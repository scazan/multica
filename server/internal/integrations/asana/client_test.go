package asana

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMeReturnsUserAndWorkspaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/me" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.URL.Query().Get("opt_fields"); got != "name,email,workspaces.name" {
			t.Errorf("opt_fields = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"gid":"1","name":"Ada","email":"ada@example.com","workspaces":[{"gid":"10","name":"Acme"},{"gid":"11","name":"Lab"}]}}`))
	}))
	defer srv.Close()

	me, err := NewClient("tok", WithBaseURL(srv.URL)).Me(context.Background())
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if me.GID != "1" || me.Name != "Ada" || me.Email != "ada@example.com" {
		t.Fatalf("unexpected user %+v", me)
	}
	if len(me.Workspaces) != 2 || me.Workspaces[1].GID != "11" || me.Workspaces[1].Name != "Lab" {
		t.Fatalf("unexpected workspaces %+v", me.Workspaces)
	}
}

func TestMeMapsStatusCodes(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{http.StatusUnauthorized, `{"errors":[{"message":"Not Authorized"}]}`, ErrUnauthorized},
		{http.StatusForbidden, `{"errors":[{"message":"Forbidden"}]}`, ErrUnauthorized},
		{http.StatusNotFound, `{"errors":[{"message":"Not Found"}]}`, ErrNotFound},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := NewClient("tok", WithBaseURL(srv.URL)).Me(context.Background())
		srv.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: got %v, want %v", tc.status, err, tc.want)
		}
	}
}

func TestMeSurfacesAPIErrorMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Rate Limit Enforced"}]}`))
	}))
	defer srv.Close()

	_, err := NewClient("tok", WithBaseURL(srv.URL)).Me(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %v", err)
	}
	if apiErr.Status != http.StatusTooManyRequests || apiErr.Message != "Rate Limit Enforced" {
		t.Fatalf("unexpected APIError %+v", apiErr)
	}
}

func TestMeRejectsMalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"nope":true}`))
	}))
	defer srv.Close()

	if _, err := NewClient("tok", WithBaseURL(srv.URL)).Me(context.Background()); err == nil {
		t.Fatal("expected an error for a response without data")
	}
}
