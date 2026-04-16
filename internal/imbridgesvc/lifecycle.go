package imbridgesvc

import (
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/agentserver/agentserver/internal/imbridge"
)

// RestorePollers restarts long-poll goroutines for all active workspace IM
// channels. Called once during startup to recover from restarts — the cursor
// is persisted in DB, so pollers resume without message loss.
func (s *Server) RestorePollers() {
	restored := 0
	for _, provider := range s.bridge.Providers() {
		channels, err := s.db.ListAllActiveChannels(provider.Name())
		if err != nil {
			log.Printf("imbridge restore: failed to query %s channels: %v", provider.Name(), err)
			continue
		}
		for _, ch := range channels {
			s.bridge.SetChannelRequireMention(ch.ID, ch.RequireMention)
			s.bridge.StartPoller(imbridge.BridgeBinding{
				Provider: provider,
				Credentials: imbridge.Credentials{
					ChannelID: ch.ID,
					BotID:     ch.BotID,
					BotToken:  ch.BotToken,
					BaseURL:   ch.BaseURL,
				},
				ChannelID:   ch.ID,
				Cursor:      ch.Cursor,
				WorkspaceID: ch.WorkspaceID,
				RoutingMode: ch.RoutingMode,
			})
			restored++
		}
	}
	if restored > 0 {
		log.Printf("imbridge restore: started %d poller(s)", restored)
	}
}

// restorePollerForSandbox restarts the poller for the channel bound to a
// sandbox. Called after sandbox resume when the Pod has a new IP.
func (s *Server) restorePollerForSandbox(sandboxID string) {
	ch, err := s.db.GetIMChannelForSandbox(sandboxID)
	if err != nil {
		return
	}
	provider := s.bridge.GetProvider(ch.Provider)
	if provider == nil {
		return
	}
	s.bridge.StartPoller(imbridge.BridgeBinding{
		Provider: provider,
		Credentials: imbridge.Credentials{
			ChannelID: ch.ID,
			BotID:     ch.BotID,
			BotToken:  ch.BotToken,
			BaseURL:   ch.BaseURL,
		},
		ChannelID:   ch.ID,
		Cursor:      ch.Cursor,
		WorkspaceID: ch.WorkspaceID,
		RoutingMode: ch.RoutingMode,
	})
}

// handleRestorePollers is called by agentserver when a sandbox resumes.
// POST /api/internal/imbridge/pollers/{sandboxId}/restore
func (s *Server) handleRestorePollers(w http.ResponseWriter, r *http.Request) {
	sandboxID := chi.URLParam(r, "sandboxId")
	s.restorePollerForSandbox(sandboxID)
	w.WriteHeader(http.StatusNoContent)
}

// handleStopPoller is called to stop a specific channel's poller.
// POST /api/internal/imbridge/pollers/{channelId}/stop
func (s *Server) handleStopPoller(w http.ResponseWriter, r *http.Request) {
	channelID := chi.URLParam(r, "channelId")
	s.bridge.StopPoller(channelID)
	w.WriteHeader(http.StatusNoContent)
}
