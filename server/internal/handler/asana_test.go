package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
)

func withAsanaBox(t *testing.T) *secretbox.Box {
	t.Helper()
	box, err := secretbox.New(bytes.Repeat([]byte("a"), 32))
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	prev := testHandler.AsanaSecretBox
	testHandler.AsanaSecretBox = box
	t.Cleanup(func() { testHandler.AsanaSecretBox = prev })
	return box
}

// fakeAsana serves /users/me with the given workspaces and counts calls.
func fakeAsana(t *testing.T, workspacesJSON string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/users/me" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Not Authorized"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"gid":"u1","name":"Ada","email":"ada@example.com","workspaces":` + workspacesJSON + `}}`))
	}))
	t.Cleanup(srv.Close)
	prev := testHandler.AsanaBaseURL
	testHandler.AsanaBaseURL = srv.URL
	t.Cleanup(func() { testHandler.AsanaBaseURL = prev })
	return srv, &calls
}

func asanaHandlerRequest(method, path string, body any, connectionID string) *http.Request {
	req := newRequest(method, path, body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", testWorkspaceID)
	if connectionID != "" {
		rctx.URLParams.Add("connectionId", connectionID)
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func cleanupAsana(ctx context.Context) {
	testPool.Exec(ctx, `DELETE FROM asana_comment_link WHERE workspace_id = $1`, testWorkspaceID)
	testPool.Exec(ctx, `DELETE FROM asana_task_link WHERE workspace_id = $1`, testWorkspaceID)
	testPool.Exec(ctx, `DELETE FROM asana_project_binding WHERE workspace_id = $1`, testWorkspaceID)
	testPool.Exec(ctx, `DELETE FROM asana_connection WHERE workspace_id = $1`, testWorkspaceID)
}

func connectAsana(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	req := asanaHandlerRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/asana/connections", body, "")
	w := httptest.NewRecorder()
	testHandler.ConnectAsana(w, req)
	return w
}

func listAsana(t *testing.T) (conns []AsanaConnectionResponse, configured bool) {
	t.Helper()
	req := asanaHandlerRequest(http.MethodGet, "/api/workspaces/"+testWorkspaceID+"/asana/connections", nil, "")
	w := httptest.NewRecorder()
	testHandler.ListAsanaConnections(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ListAsanaConnections: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Connections []AsanaConnectionResponse `json:"connections"`
		Configured  bool                      `json:"configured"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Connections, resp.Configured
}

func TestConnectAsanaRequiresSecretKey(t *testing.T) {
	_, calls := fakeAsana(t, `[{"gid":"w1","name":"Acme"}]`)
	prev := testHandler.AsanaSecretBox
	testHandler.AsanaSecretBox = nil
	t.Cleanup(func() { testHandler.AsanaSecretBox = prev })

	if w := connectAsana(t, map[string]any{"access_token": "good-token"}); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("unconfigured connect must not call asana, got %d calls", calls.Load())
	}
	if _, configured := listAsana(t); configured {
		t.Fatal("list must report configured=false without a key")
	}
}

func TestConnectAsanaStoresConnectionAndRotatesOnReconnect(t *testing.T) {
	ctx := context.Background()
	box := withAsanaBox(t)
	fakeAsana(t, `[{"gid":"w1","name":"Acme"}]`)
	t.Cleanup(func() { cleanupAsana(context.Background()) })

	w := connectAsana(t, map[string]any{"access_token": "good-token"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp AsanaConnectionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AsanaWorkspaceGID != "w1" || resp.AsanaWorkspaceName != "Acme" || resp.AsanaUserGID != "u1" || resp.AsanaUserEmail != "ada@example.com" {
		t.Fatalf("unexpected response %+v", resp)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("good-token")) {
		t.Fatal("response must not echo the access token")
	}

	// The stored token is secretbox ciphertext that opens back to the input.
	var enc string
	if err := testPool.QueryRow(ctx, `SELECT access_token_encrypted FROM asana_connection WHERE id = $1`, resp.ID).Scan(&enc); err != nil {
		t.Fatalf("read stored token: %v", err)
	}
	if enc == "good-token" || enc == "" {
		t.Fatalf("token must be stored encrypted, got %q", enc)
	}
	plain, err := testHandler.openAsanaSecret(enc)
	if err != nil || plain != "good-token" {
		t.Fatalf("openAsanaSecret: %q, %v", plain, err)
	}
	_ = box

	// Reconnecting the same Asana workspace updates the row in place.
	w2 := connectAsana(t, map[string]any{"access_token": "good-token", "asana_workspace_gid": "w1"})
	if w2.Code != http.StatusOK {
		t.Fatalf("reconnect: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}
	var resp2 AsanaConnectionResponse
	_ = json.Unmarshal(w2.Body.Bytes(), &resp2)
	if resp2.ID != resp.ID {
		t.Fatalf("reconnect must keep the connection id, got %s vs %s", resp2.ID, resp.ID)
	}
	conns, configured := listAsana(t)
	if !configured || len(conns) != 1 || conns[0].ID != resp.ID {
		t.Fatalf("list: configured=%v conns=%+v", configured, conns)
	}
}

func TestConnectAsanaRejectsBadToken(t *testing.T) {
	withAsanaBox(t)
	fakeAsana(t, `[{"gid":"w1","name":"Acme"}]`)
	t.Cleanup(func() { cleanupAsana(context.Background()) })

	if w := connectAsana(t, map[string]any{"access_token": "wrong"}); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if w := connectAsana(t, map[string]any{"access_token": "   "}); w.Code != http.StatusBadRequest {
		t.Fatalf("blank token: expected 400, got %d", w.Code)
	}
	if conns, _ := listAsana(t); len(conns) != 0 {
		t.Fatalf("rejected connect must not store a row, got %+v", conns)
	}
}

func TestConnectAsanaAsksForWorkspaceWhenAmbiguous(t *testing.T) {
	withAsanaBox(t)
	fakeAsana(t, `[{"gid":"w1","name":"Acme"},{"gid":"w2","name":"Lab"}]`)
	t.Cleanup(func() { cleanupAsana(context.Background()) })

	w := connectAsana(t, map[string]any{"access_token": "good-token"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code       string                 `json:"code"`
		Workspaces []AsanaWorkspaceChoice `json:"workspaces"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Code != "asana_workspace_required" || len(resp.Workspaces) != 2 || resp.Workspaces[1].GID != "w2" {
		t.Fatalf("unexpected ambiguity response %s", w.Body.String())
	}

	if w := connectAsana(t, map[string]any{"access_token": "good-token", "asana_workspace_gid": "w9"}); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown gid: expected 400, got %d", w.Code)
	}

	w = connectAsana(t, map[string]any{"access_token": "good-token", "asana_workspace_gid": "w2"})
	if w.Code != http.StatusOK {
		t.Fatalf("explicit gid: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var conn AsanaConnectionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &conn)
	if conn.AsanaWorkspaceGID != "w2" || conn.AsanaWorkspaceName != "Lab" {
		t.Fatalf("unexpected connection %+v", conn)
	}
}

func TestDeleteAsanaConnectionSweepsChildren(t *testing.T) {
	ctx := context.Background()
	withAsanaBox(t)
	fakeAsana(t, `[{"gid":"w1","name":"Acme"}]`)
	t.Cleanup(func() { cleanupAsana(context.Background()) })

	w := connectAsana(t, map[string]any{"access_token": "good-token"})
	if w.Code != http.StatusOK {
		t.Fatalf("connect: %d %s", w.Code, w.Body.String())
	}
	var conn AsanaConnectionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &conn)

	var bindingID, linkID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO asana_project_binding (workspace_id, connection_id, asana_project_gid) VALUES ($1, $2, 'p1') RETURNING id`,
		testWorkspaceID, conn.ID).Scan(&bindingID); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO asana_task_link (workspace_id, binding_id, asana_task_gid, issue_id) VALUES ($1, $2, 't1', gen_random_uuid()) RETURNING id`,
		testWorkspaceID, bindingID).Scan(&linkID); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO asana_comment_link (workspace_id, task_link_id, comment_id, asana_story_gid, direction) VALUES ($1, $2, gen_random_uuid(), 's1', 'inbound')`,
		testWorkspaceID, linkID); err != nil {
		t.Fatalf("seed comment link: %v", err)
	}

	req := asanaHandlerRequest(http.MethodDelete, "/api/workspaces/"+testWorkspaceID+"/asana/connections/"+conn.ID, nil, conn.ID)
	rec := httptest.NewRecorder()
	testHandler.DeleteAsanaConnection(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	for _, table := range []string{"asana_connection", "asana_project_binding", "asana_task_link", "asana_comment_link"} {
		var n int
		if err := testPool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, testWorkspaceID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s: expected 0 rows after delete, got %d", table, n)
		}
	}
}
