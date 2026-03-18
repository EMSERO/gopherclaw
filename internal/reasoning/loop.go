package reasoning

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agent"
	"github.com/EMSERO/gopherclaw/internal/agentapi"
	"github.com/EMSERO/gopherclaw/internal/eidetic"
	"github.com/EMSERO/gopherclaw/internal/surfaces"
)

// Loop runs the periodic reasoning cycle.
type Loop struct {
	interval   time.Duration
	agent      agent.PrimaryAgent
	eidetic    eidetic.Client
	store      *surfaces.Store
	logger     *zap.SugaredLogger
	triggerCh  chan struct{}            // external trigger for early cycle
	deliverers []agentapi.Deliverer    // channel bots for high-priority notifications
}

// New creates a reasoning Loop.
func New(interval time.Duration, ag agent.PrimaryAgent, eideticClient eidetic.Client, store *surfaces.Store, logger *zap.SugaredLogger) *Loop {
	return &Loop{
		interval:  interval,
		agent:     ag,
		eidetic:   eideticClient,
		store:     store,
		logger:    logger,
		triggerCh: make(chan struct{}, 1),
	}
}

// Trigger requests an early reasoning cycle. Safe to call from any goroutine.
// Multiple rapid triggers coalesce into a single debounced cycle.
func (l *Loop) Trigger() {
	select {
	case l.triggerCh <- struct{}{}:
	default: // already pending, coalesce
	}
}

// AddDeliverer registers a channel bot for high-priority surface notifications.
func (l *Loop) AddDeliverer(d agentapi.Deliverer) {
	l.deliverers = append(l.deliverers, d)
}

// Run starts the reasoning loop. Blocks until ctx is cancelled.
func (l *Loop) Run(ctx context.Context) {
	l.logger.Infof("reasoning: starting (interval=%s)", l.interval)

	// Initial delay to let the system settle.
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}

	l.cycle(ctx)

	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()

	var debounceCh <-chan time.Time // nil until a trigger arrives
	for {
		select {
		case <-ctx.Done():
			l.logger.Info("reasoning: shutting down")
			return
		case <-ticker.C:
			debounceCh = nil // cancel any pending debounce
			l.cycle(ctx)
		case <-l.triggerCh:
			if debounceCh == nil {
				l.logger.Debug("reasoning: triggered, debouncing 30s")
				debounceCh = time.After(30 * time.Second)
			}
		case <-debounceCh:
			debounceCh = nil
			l.logger.Info("reasoning: running triggered cycle")
			l.cycle(ctx)
			ticker.Reset(l.interval)
		}
	}
}

func (l *Loop) cycle(ctx context.Context) {
	// Promote due reminders before the main reasoning cycle.
	l.promoteDueReminders(ctx)

	cycleID := uuid.New()
	l.logger.Infof("reasoning: cycle %s starting", cycleID)

	// Gather context from Eidetic.
	var entries []eidetic.MemoryEntry
	if l.eidetic != nil {
		var err error
		entries, err = l.eidetic.GetRecent(ctx, "", 20)
		if err != nil {
			l.logger.Warnf("reasoning: get recent entries: %v", err)
		}
	}

	activeSurfaces, err := l.store.List(ctx, surfaces.ListFilter{Status: "active"})
	if err != nil {
		l.logger.Warnf("reasoning: list active surfaces: %v", err)
	}

	// Get recently dismissed/acted surfaces so Claude won't recreate them.
	dismissedSurfaces, _ := l.store.List(ctx, surfaces.ListFilter{Status: "dismissed", Limit: 20})
	actedSurfaces, _ := l.store.List(ctx, surfaces.ListFilter{Status: "acted", Limit: 10})
	recentlyResolved := append(dismissedSurfaces, actedSurfaces...)

	answeredSurfaces, _ := l.store.List(ctx, surfaces.ListFilter{Status: "answered", Limit: 10})

	// Build prompt.
	prompt := BuildPrompt(entries, activeSurfaces, recentlyResolved, answeredSurfaces)

	// Call the agent using ChatLight (no tools, lightweight system prompt).
	resp, err := l.agent.ChatLight(ctx, "reasoning:cycle", prompt)
	if err != nil {
		l.logger.Warnf("reasoning: agent call failed: %v", err)
		return
	}

	// Parse response.
	parsed, err := ParseResponse(resp.Text)
	if err != nil {
		l.logger.Warnf("reasoning: parse response: %v", err)
		return
	}

	// Expire stale surfaces.
	expired := 0
	for _, idStr := range parsed.Expire {
		id, err := uuid.Parse(idStr)
		if err != nil {
			l.logger.Warnf("reasoning: invalid expire id %q: %v", idStr, err)
			continue
		}
		expiredStatus := surfaces.StatusExpired
		_, err = l.store.Update(ctx, id, surfaces.UpdateRequest{Status: &expiredStatus})
		if err != nil {
			l.logger.Warnf("reasoning: expire surface %s: %v", id, err)
			continue
		}
		expired++
	}

	// Create new surfaces.
	created := 0
	for _, rs := range parsed.Create {
		_, err := l.store.Create(ctx, surfaces.CreateRequest{
			Content:        rs.Content,
			SurfaceType:    surfaces.SurfaceType(rs.SurfaceType),
			Priority:       rs.Priority,
			Tags:           rs.Tags,
			ReasoningCycle: cycleID,
			TriggerAt:      rs.ParseTriggerAt(),
		})
		if err != nil {
			l.logger.Warnf("reasoning: create surface: %v", err)
			continue
		}
		created++

		// Broadcast high-priority surfaces to channel bots.
		if rs.Priority <= 2 && len(l.deliverers) > 0 {
			msg := formatSurfaceNotification(rs.SurfaceType, rs.Content)
			for _, d := range l.deliverers {
				d.SendToAllPaired(msg)
			}
		}
	}

	l.logger.Infof("reasoning: cycle %s — expired %d, created %d surfaces", cycleID, expired, created)
}

// promoteDueReminders finds active surfaces whose trigger_at has passed
// and broadcasts them to channel bots.
func (l *Loop) promoteDueReminders(ctx context.Context) {
	due, err := l.store.DueReminders(ctx)
	if err != nil {
		l.logger.Warnf("reasoning: check due reminders: %v", err)
		return
	}
	for _, s := range due {
		// Broadcast to channels regardless of original priority.
		if len(l.deliverers) > 0 {
			msg := formatSurfaceNotification(string(s.SurfaceType), s.Content)
			for _, d := range l.deliverers {
				d.SendToAllPaired(msg)
			}
		}
		// Mark as acted so it doesn't fire again.
		acted := surfaces.StatusActed
		if _, err := l.store.Update(ctx, s.ID, surfaces.UpdateRequest{Status: &acted}); err != nil {
			l.logger.Warnf("reasoning: mark reminder acted %s: %v", s.ID, err)
		}
		l.logger.Infof("reasoning: promoted due reminder %s", s.ID)
	}
}

func formatSurfaceNotification(surfaceType, content string) string {
	icon := "💡"
	switch surfaceType {
	case "warning":
		icon = "⚠️"
	case "question":
		icon = "❓"
	case "reminder":
		icon = "⏰"
	case "connection":
		icon = "🔗"
	}
	return fmt.Sprintf("%s [%s] %s", icon, surfaceType, content)
}
