// Long-polling update source for one user-supplied bot.
//
// MVP transport decision: getUpdates needs no public URL, which makes local
// debugging trivial. The transport is encapsulated here so a webhook-based
// source can replace it later without touching the manager surface or core.
package botservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

const (
	// longPollTimeoutSeconds is how long Telegram holds an empty getUpdates call.
	longPollTimeoutSeconds = 50
	backoffMin             = time.Second
	backoffMax             = time.Minute
)

// poller pulls updates for a single bot and feeds them to the handler.
// One goroutine per bot: updates are processed sequentially, which preserves
// per-chat ordering without the webhook worker shards.
type poller struct {
	bot     store.UserBotRecord
	baseURL string
	client  *http.Client
	handler Handler
	repo    Store
	logger  *slog.Logger
}

// run polls until ctx is cancelled. Telegram errors back off exponentially;
// a 409 Conflict (webhook still registered for this bot) triggers a one-time
// deleteWebhook, and a 401 marks the token invalid and stops the poller.
func (p *poller) run(ctx context.Context) {
	offset := p.bot.LastUpdateOffset
	backoff := backoffMin
	webhookDeleted := false
	for ctx.Err() == nil {
		updates, err := getUpdates(ctx, p.client, p.baseURL, p.bot.Token, offset+1)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var apiErr *apiError
			switch {
			case errors.As(err, &apiErr) && apiErr.Code == http.StatusConflict && !webhookDeleted:
				// getUpdates and webhook are mutually exclusive; drop the stale webhook once.
				webhookDeleted = true
				if delErr := deleteWebhook(ctx, p.client, p.baseURL, p.bot.Token); delErr != nil {
					p.logger.Warn("bot poller deleteWebhook failed",
						"bot_id", p.bot.BotID, "error", redactToken(delErr, p.bot.Token))
				} else {
					p.logger.Info("bot poller removed stale webhook", "bot_id", p.bot.BotID)
				}
				continue
			case errors.As(err, &apiErr) && apiErr.Code == http.StatusUnauthorized:
				p.repo.UpdateUserBotStatus(p.bot.BotID, "invalid")
				p.logger.Error("bot token rejected by telegram; poller stopped",
					"tenant_id", p.bot.TenantID, "bot_id", p.bot.BotID)
				return
			}
			p.logger.Warn("bot poller getUpdates failed",
				"bot_id", p.bot.BotID, "backoff", backoff.String(),
				"error", redactToken(err, p.bot.Token))
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, backoffMax)
			continue
		}
		backoff = backoffMin
		maxUpdateID := offset
		for _, raw := range updates {
			updateID, ok := peekUpdateID(raw)
			if !ok {
				continue
			}
			if updateID > maxUpdateID {
				maxUpdateID = updateID
			}
			event, isMessage, err := telegram.NormalizeBotUpdate(raw, p.bot.BotID, p.bot.TenantID, time.Now().UTC())
			if err != nil {
				p.logger.Warn("bot update normalization failed",
					"bot_id", p.bot.BotID, "update_id", updateID, "error", err)
				continue
			}
			if !isMessage {
				continue
			}
			// Parse any downloadable attachment so the handler can turn it into
			// text. A parse failure is non-fatal: the message is still processed
			// (as text/placeholder), never dropped.
			media, mediaErr := telegram.MediaFromBotUpdate(raw)
			if mediaErr != nil {
				p.logger.Warn("bot media descriptor parse failed",
					"bot_id", p.bot.BotID, "update_id", updateID, "error", mediaErr)
			}
			if err := p.handler.HandleBotMessage(ctx, event, media); err != nil {
				// The receive idempotency key makes redelivery safe, but we do not
				// retry here: the next message will surface persistent failures.
				p.logger.Error("bot message processing failed",
					"tenant_id", p.bot.TenantID, "bot_id", p.bot.BotID,
					"event_id", event.EventID, "error", err)
			}
		}
		if maxUpdateID > offset {
			offset = maxUpdateID
			p.repo.UpdateUserBotOffset(p.bot.BotID, offset)
		}
	}
}

// peekUpdateID extracts update_id without full normalization so the offset
// always advances past every delivered update (even unparseable ones).
func peekUpdateID(raw json.RawMessage) (int64, bool) {
	var probe struct {
		UpdateID *int64 `json:"update_id"`
	}
	if json.Unmarshal(raw, &probe) != nil || probe.UpdateID == nil {
		return 0, false
	}
	return *probe.UpdateID, true
}

// sleepCtx waits d or until ctx is done; returns false on cancellation.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// apiError is a non-2xx Telegram Bot API response.
type apiError struct {
	Code        int
	Description string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("telegram api status %d: %s", e.Code, e.Description)
}

// botUser is the getMe result payload.
type botUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// getMe validates a bot token and returns the bot identity.
func getMe(ctx context.Context, client *http.Client, baseURL, token string) (botUser, error) {
	var result botUser
	err := callAPI(ctx, client, baseURL, token, "getMe", nil, &result)
	return result, err
}

// getUpdates long-polls Telegram for the next batch of message updates.
func getUpdates(ctx context.Context, client *http.Client, baseURL, token string, offset int64) ([]json.RawMessage, error) {
	body := map[string]any{
		"offset":          offset,
		"timeout":         longPollTimeoutSeconds,
		"allowed_updates": []string{"message"},
	}
	var result []json.RawMessage
	err := callAPI(ctx, client, baseURL, token, "getUpdates", body, &result)
	return result, err
}

// deleteWebhook removes a webhook registration that blocks getUpdates (409).
func deleteWebhook(ctx context.Context, client *http.Client, baseURL, token string) error {
	return callAPI(ctx, client, baseURL, token, "deleteWebhook", nil, nil)
}

// callAPI posts one Bot API method call and decodes result into out (when non-nil).
func callAPI(ctx context.Context, client *http.Client, baseURL, token, method string, body map[string]any, out any) error {
	payload := []byte("{}")
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = data
	}
	url := baseURL + "/bot" + token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return redactToken(err, token)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return redactToken(err, token)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || !envelope.OK {
		code := res.StatusCode
		if envelope.ErrorCode != 0 {
			code = envelope.ErrorCode
		}
		return &apiError{Code: code, Description: envelope.Description}
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Result, out); err != nil {
			return fmt.Errorf("telegram %s: decode result: %w", method, err)
		}
	}
	return nil
}
