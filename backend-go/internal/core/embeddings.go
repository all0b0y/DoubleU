package core

import (
	"context"
	"time"

	"doubleu/backend-go/internal/aiclient"
)

// nudgeEmbeddingWorker signals the embedding worker that new facts need
// vectors (ARCH-12). Non-blocking: a pending nudge already covers the batch.
func (s *Service) nudgeEmbeddingWorker() {
	select {
	case s.embedNudge <- struct{}{}:
	default:
	}
}

// StartEmbeddingWorker generates embeddings for any semantic facts inserted
// without one. It wakes on a 5-minute ticker OR immediately on a nudge from
// saveTypedFacts (ARCH-12) — previously fresh facts were invisible to vector
// recall for up to 5 minutes. Runs in a separate goroutine; safe to call
// concurrently. Activates only when the AI client implements TextEmbedder.
func (s *Service) StartEmbeddingWorker(ctx context.Context) {
	embedder, ok := s.ai.(aiclient.TextEmbedder)
	if !ok {
		return // AI client does not support embedding generation.
	}
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.runEmbeddingBatch(ctx, embedder)
		case <-s.embedNudge:
			s.runEmbeddingBatch(ctx, embedder)
		case <-ctx.Done():
			return
		}
	}
}

// runEmbeddingBatch fetches facts and episode summaries without embeddings (up to 50
// at a time), calls the Python embedding endpoint, and persists vectors.
func (s *Service) runEmbeddingBatch(ctx context.Context, embedder aiclient.TextEmbedder) {
	const batchSize = 50
	s.runFactEmbeddingBatch(ctx, embedder, batchSize)
	s.runEpisodeSummaryEmbeddingBatch(ctx, embedder, batchSize)
}

func (s *Service) runFactEmbeddingBatch(ctx context.Context, embedder aiclient.TextEmbedder, batchSize int) {
	facts := s.repository.FactsWithoutEmbeddings(batchSize)
	if len(facts) == 0 {
		return
	}
	texts := make([]string, len(facts))
	for i, f := range facts {
		texts[i] = f.FactText
	}
	resp, err := embedder.EmbedTexts(ctx, aiclient.EmbedTextsRequest{
		Texts: texts,
		Model: "qwen/qwen3-embedding-8b",
	})
	if err != nil || resp.ModelUsed == "none" || resp.ModelUsed == "error" {
		s.logger.Warn("fact embedding batch failed", "error", err, "model", resp.ModelUsed)
		return
	}
	for i, f := range facts {
		if i >= len(resp.Embeddings) {
			break
		}
		if emb := resp.Embeddings[i]; len(emb) > 0 {
			s.repository.SaveFactEmbedding(f.ID, f.TenantID, f.ScopeKind, f.ChatID, emb, resp.ModelUsed)
		}
	}
	s.logger.Info("fact embedding batch completed", "facts_embedded", len(facts), "model", resp.ModelUsed)
}

func (s *Service) runEpisodeSummaryEmbeddingBatch(ctx context.Context, embedder aiclient.TextEmbedder, batchSize int) {
	summaries := s.repository.EpisodeSummariesWithoutEmbeddings(batchSize)
	if len(summaries) == 0 {
		return
	}
	texts := make([]string, len(summaries))
	for i, sm := range summaries {
		texts[i] = sm.Content
	}
	resp, err := embedder.EmbedTexts(ctx, aiclient.EmbedTextsRequest{
		Texts: texts,
		Model: "qwen/qwen3-embedding-8b",
	})
	if err != nil || resp.ModelUsed == "none" || resp.ModelUsed == "error" {
		s.logger.Warn("episode summary embedding batch failed", "error", err, "model", resp.ModelUsed)
		return
	}
	for i, sm := range summaries {
		if i >= len(resp.Embeddings) {
			break
		}
		if emb := resp.Embeddings[i]; len(emb) > 0 {
			s.repository.SaveEpisodeSummaryEmbedding(sm.ID, sm.TenantID, emb, resp.ModelUsed)
		}
	}
	s.logger.Info("episode summary embedding batch completed", "summaries_embedded", len(summaries), "model", resp.ModelUsed)
}
