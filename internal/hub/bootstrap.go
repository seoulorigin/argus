package hub

import (
	"errors"
	"log/slog"
	"net/http"

	"argus/internal/hub/utils"

	"ganigeorgiev.com/pocketbase/dbx"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
)

// seedAgentToken registers the permanent agent token given in AGENT_TOKEN, so a
// non-interactive deployment (Helm) needs no login to hand a token to the agents.
// It does nothing when AGENT_TOKEN is unset or no user exists yet.
func (h *Hub) seedAgentToken() error {
	token, _ := utils.GetEnv("AGENT_TOKEN")
	if token == "" {
		return nil
	}
	if len(token) < 16 {
		return errors.New("AGENT_TOKEN must be at least 16 characters")
	}

	var user *core.Record
	if email, _ := utils.GetEnv("USER_EMAIL"); email != "" {
		user, _ = h.FindAuthRecordByEmail("users", email)
	}
	if user == nil {
		users, err := h.FindRecordsByFilter("users", "role = 'admin'", "created", 1, 0)
		if err != nil || len(users) == 0 {
			slog.Warn("AGENT_TOKEN set but no admin user exists yet; token not registered")
			return nil
		}
		user = users[0]
	}

	rec, err := h.FindFirstRecordByFilter("universal_tokens", "user = {:user}", dbx.Params{"user": user.Id})
	if err != nil {
		col, err := h.FindCachedCollectionByNameOrId("universal_tokens")
		if err != nil {
			return err
		}
		rec = core.NewRecord(col)
		rec.Set("user", user.Id)
	}
	if rec.GetString("token") == token {
		return nil
	}
	rec.Set("token", token)
	return h.Save(rec)
}

// getPublicKey returns only the hub's public key. The key is not secret; agents
// use it to verify that the hub they connect to is the one that signs its challenge.
func (h *Hub) getPublicKey(e *core.RequestEvent) error {
	h.sshKeyMu.Lock()
	pubKey := h.pubKey
	h.sshKeyMu.Unlock()
	e.Response.Header().Set("Cache-Control", "no-store")
	return e.JSON(http.StatusOK, map[string]string{"key": pubKey})
}
