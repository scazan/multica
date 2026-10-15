package handler

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/asana"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ── Response shapes ─────────────────────────────────────────────────────────

// AsanaConnectionResponse is the JSON shape for a stored Asana connection. The
// access token is never included.
type AsanaConnectionResponse struct {
	ID                 string `json:"id"`
	WorkspaceID        string `json:"workspace_id"`
	AsanaWorkspaceGID  string `json:"asana_workspace_gid"`
	AsanaWorkspaceName string `json:"asana_workspace_name"`
	AsanaUserGID       string `json:"asana_user_gid"`
	AsanaUserName      string `json:"asana_user_name"`
	AsanaUserEmail     string `json:"asana_user_email"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
}

// AsanaWorkspaceChoice is one Asana workspace a token can see. Returned when
// the token spans several workspaces and the caller did not pick one.
type AsanaWorkspaceChoice struct {
	GID  string `json:"gid"`
	Name string `json:"name"`
}

func (h *Handler) isAsanaConfigured() bool { return h.AsanaSecretBox != nil }

func asanaConnectionToResponse(c db.AsanaConnection) AsanaConnectionResponse {
	return AsanaConnectionResponse{
		ID:                 uuidToString(c.ID),
		WorkspaceID:        uuidToString(c.WorkspaceID),
		AsanaWorkspaceGID:  c.AsanaWorkspaceGid,
		AsanaWorkspaceName: c.AsanaWorkspaceName,
		AsanaUserGID:       c.AsanaUserGid,
		AsanaUserName:      c.AsanaUserName,
		AsanaUserEmail:     c.AsanaUserEmail,
		CreatedAt:          timestampToString(c.CreatedAt),
		UpdatedAt:          timestampToString(c.UpdatedAt),
	}
}

func (h *Handler) sealAsanaSecret(plaintext string) (string, error) {
	sealed, err := h.AsanaSecretBox.Seal([]byte(plaintext))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (h *Handler) openAsanaSecret(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	ciphertext, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	plaintext, err := h.AsanaSecretBox.Open(ciphertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// asanaClientOptions lets tests point the client at a fake Asana server.
func (h *Handler) asanaClientOptions() []asana.Option {
	if h.AsanaBaseURL != "" {
		return []asana.Option{asana.WithBaseURL(h.AsanaBaseURL)}
	}
	return nil
}

// ── Handlers ────────────────────────────────────────────────────────────────

// ListAsanaConnections (GET /workspaces/{id}/asana/connections) is
// member-visible; connect/disconnect are admin-gated by the router. No secrets
// are returned.
func (h *Handler) ListAsanaConnections(w http.ResponseWriter, r *http.Request) {
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	member, _ := middleware.MemberFromContext(r.Context())
	canManage := roleAllowed(member.Role, "owner", "admin")

	rows, err := h.Queries.ListAsanaConnectionsByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list connections")
		return
	}
	out := make([]AsanaConnectionResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, asanaConnectionToResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connections": out,
		"configured":  h.isAsanaConfigured(),
		"can_manage":  canManage,
	})
}

type connectAsanaRequest struct {
	AccessToken string `json:"access_token"`
	// AsanaWorkspaceGID selects which of the token's workspaces to connect.
	// Optional when the token can see exactly one.
	AsanaWorkspaceGID string `json:"asana_workspace_gid"`
}

// ConnectAsana (POST /workspaces/{id}/asana/connections) validates the token
// against Asana, resolves the Asana workspace to connect, stores the token
// encrypted, and returns the connection. Reconnecting the same Asana
// workspace rotates the token in place.
//
// When the token spans several Asana workspaces and none was chosen, the
// handler answers 400 with code asana_workspace_required and the choices, so
// the UI/CLI can prompt without a second validation round trip.
func (h *Handler) ConnectAsana(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	if !h.isAsanaConfigured() {
		writeFeatureDisabled(w, "asana_not_configured", "asana integration not configured (MULTICA_ASANA_SECRET_KEY unset)")
		return
	}

	var req connectAsanaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	token := strings.TrimSpace(req.AccessToken)
	if token == "" {
		writeError(w, http.StatusBadRequest, "access_token is required")
		return
	}

	me, err := asana.NewClient(token, h.asanaClientOptions()...).Me(r.Context())
	if err != nil {
		if errors.Is(err, asana.ErrUnauthorized) {
			writeError(w, http.StatusBadRequest, "asana rejected the access token")
			return
		}
		slog.Warn("asana: connect validation failed", "workspace_id", workspaceID, "err", err)
		writeError(w, http.StatusBadGateway, "could not reach asana")
		return
	}
	if len(me.Workspaces) == 0 {
		writeError(w, http.StatusBadRequest, "the asana user has no workspaces")
		return
	}

	wantGID := strings.TrimSpace(req.AsanaWorkspaceGID)
	var chosen *asana.Workspace
	switch {
	case wantGID != "":
		for i := range me.Workspaces {
			if me.Workspaces[i].GID == wantGID {
				chosen = &me.Workspaces[i]
				break
			}
		}
		if chosen == nil {
			writeError(w, http.StatusBadRequest, "the access token cannot see asana workspace "+wantGID)
			return
		}
	case len(me.Workspaces) == 1:
		chosen = &me.Workspaces[0]
	default:
		choices := make([]AsanaWorkspaceChoice, 0, len(me.Workspaces))
		for _, ws := range me.Workspaces {
			choices = append(choices, AsanaWorkspaceChoice{GID: ws.GID, Name: ws.Name})
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":      "the access token can see several asana workspaces; pass asana_workspace_gid",
			"code":       "asana_workspace_required",
			"workspaces": choices,
		})
		return
	}

	tokenEnc, err := h.sealAsanaSecret(token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encrypt token")
		return
	}

	var connectedBy pgtype.UUID
	if member, ok := middleware.MemberFromContext(r.Context()); ok {
		connectedBy = member.UserID
	}

	conn, err := h.Queries.UpsertAsanaConnection(r.Context(), db.UpsertAsanaConnectionParams{
		WorkspaceID:          wsUUID,
		AsanaWorkspaceGid:    chosen.GID,
		AsanaWorkspaceName:   chosen.Name,
		AsanaUserGid:         me.GID,
		AsanaUserName:        me.Name,
		AsanaUserEmail:       me.Email,
		AccessTokenEncrypted: tokenEnc,
		ConnectedByID:        connectedBy,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save connection")
		return
	}

	resp := asanaConnectionToResponse(conn)
	h.publish(protocol.EventAsanaConnectionCreated, workspaceID, "system", "", map[string]any{"id": resp.ID})
	writeJSON(w, http.StatusOK, resp)
}

// DeleteAsanaConnection (DELETE /workspaces/{id}/asana/connections/{connectionId})
// removes the connection and every binding and link under it. Mirrored
// issues stay.
func (h *Handler) DeleteAsanaConnection(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	connID := chi.URLParam(r, "connectionId")
	connUUID, ok := parseUUIDOrBadRequest(w, connID, "connection id")
	if !ok {
		return
	}
	if err := h.Queries.DeleteAsanaConnection(r.Context(), db.DeleteAsanaConnectionParams{
		ID:          connUUID,
		WorkspaceID: wsUUID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to remove connection")
		return
	}
	h.publish(protocol.EventAsanaConnectionDeleted, workspaceID, "system", "", map[string]any{"id": connID})
	w.WriteHeader(http.StatusNoContent)
}
