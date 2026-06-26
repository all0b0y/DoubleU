package core

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"

	_ "github.com/lib/pq"
)

// clusterByPairs must group transitively-connected ids and drop singletons.
func TestClusterByPairs(t *testing.T) {
	got := clusterByPairs([]store.FactPair{{IDA: 1, IDB: 2}, {IDA: 2, IDB: 3}, {IDA: 5, IDB: 6}})
	if !reflect.DeepEqual(got, [][]int64{{1, 2, 3}, {5, 6}}) {
		t.Fatalf("unexpected clusters: %v", got)
	}
	if c := clusterByPairs(nil); len(c) != 0 {
		t.Fatalf("no pairs must yield no clusters, got %v", c)
	}
}

// fakePatternAI satisfies aiclient.Client + aiclient.PatternSynthesizer.
type fakePatternAI struct {
	pattern string
	calls   int32
}

func (f *fakePatternAI) ProcessMessage(ctx context.Context, _ contracts.AIProcessMessageRequest) (contracts.AIProcessMessageResponse, error) {
	return contracts.AIProcessMessageResponse{}, nil
}

func (f *fakePatternAI) SynthesizePattern(ctx context.Context, _ contracts.SynthesizePatternRequest) (contracts.SynthesizePatternResponse, error) {
	atomic.AddInt32(&f.calls, 1)
	return contracts.SynthesizePatternResponse{Pattern: f.pattern, Subject: "contact", Confidence: 0.6}, nil
}

// runReflection is a no-op when reflection is off, without touching the AI.
func TestRunReflectionOffIsNoop(t *testing.T) {
	ai := &fakePatternAI{pattern: "x"}
	svc := NewService(store.NewRepository(), ai, telegram.NoopSender{}) // default mode: off
	svc.runReflection(context.Background(), "tenant-1")
	if atomic.LoadInt32(&ai.calls) != 0 {
		t.Fatalf("reflection must not run when mode is off")
	}
}

// End-to-end: in enabled mode, a cluster of ≥3 similar facts produces one
// written behavior_pattern fact. Gated on a real Postgres (DATABASE_URL_TEST).
func TestReflectChatEnabledWritesPattern(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL_TEST")
	if dsn == "" {
		t.Skip("DATABASE_URL_TEST not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	tenant := fmt.Sprintf("tenant:reflect:%d", time.Now().UnixNano())
	chat := "42"
	vec := "[" + strings.TrimSuffix(strings.Repeat("0.01,", 4096), ",") + "]"
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO tenants (id, name) VALUES ($1,'t') ON CONFLICT DO NOTHING`, tenant)
	mustExec(t, db, `INSERT INTO chats (tenant_id, chat_id, telegram_chat_id, status, transport)
	                 VALUES ($1,$2,$2,'managed','bot') ON CONFLICT DO NOTHING`, tenant, chat)
	// Three distinct facts sharing one embedding (raw inserts bypass upsert dedup
	// so all three coexist as the cluster the reflector must find).
	for i, text := range []string{"опять завал на работе", "устал, ничего не успеваю", "снова перерабатываю допоздна"} {
		var id int64
		if err := db.QueryRow(
			`INSERT INTO semantic_facts (tenant_id, scope_kind, chat_id, fact_text, fact_type, subject,
			    confidence, source, normalized_fact_hash)
			 VALUES ($1,'chat',$2,$3,'event','contact',0.7,'llm_extracted', md5($3||$1))
			 RETURNING id`, tenant, chat, text).Scan(&id); err != nil {
			t.Fatalf("seed fact %d: %v", i, err)
		}
		mustExec(t, db, `INSERT INTO fact_embeddings (fact_id, tenant_id, scope_kind, chat_id, embedding_model, embedding)
		                 VALUES ($1,$2,'chat',$3,'test',$4::vector)`, id, tenant, chat, vec)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM semantic_facts WHERE tenant_id=$1`, tenant)
		_, _ = db.Exec(`DELETE FROM chats WHERE tenant_id=$1`, tenant)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id=$1`, tenant)
	})

	repo, err := store.NewPostgresRepository(dsn)
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	ai := &fakePatternAI{pattern: "регулярно перегружен работой, часто в стрессе"}
	svc := NewService(repo, ai, telegram.NoopSender{}, WithReflectionMode("enabled"))

	// Run twice: the second pass must not duplicate the pattern (hash dedup).
	svc.reflectChat(ctx, tenant, chat)
	svc.reflectChat(ctx, tenant, chat)

	if atomic.LoadInt32(&ai.calls) == 0 {
		t.Fatal("expected the synthesizer to be called for a 3-fact cluster")
	}
	var count int
	if err := db.QueryRow(
		`SELECT count(*) FROM semantic_facts
		  WHERE tenant_id=$1 AND fact_type='behavior_pattern' AND source='llm_reflected'
		    AND deleted_at IS NULL`, tenant).Scan(&count); err != nil {
		t.Fatalf("count patterns: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 behavior_pattern fact (dedup across runs), got %d", count)
	}
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec failed: %v\nquery: %s", err, query)
	}
}
