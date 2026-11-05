package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func TestQwenExampleLongDeadlineAndDisabledThinking(t *testing.T) {
	cfg, err := loadConfig("../../configs/lfm2.5-350m.example.json")
	if err != nil {
		t.Fatal(err)
	}
	old := http.DefaultTransport
	http.DefaultTransport = deadlineTransport{t: t, minimum: 19 * time.Minute}
	defer func() { http.DefaultTransport = old }()
	p, err := knowledgePlanner(cfg.Knowledge.Planner)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if _, err := p.Plan(ctx, knowledge.Input{OperationKey: "deadline", Text: "Atlas uses Redis."}, knowledge.State{}); err != nil {
		t.Fatal(err)
	}
	s := memstore.New(nil)
	defer s.Close()
	e, err := niromem.Open(niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{Timeout: 20 * time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.Worker().Timeout < 21*time.Minute {
		t.Fatal("worker truncates configured inference deadline")
	}
	for _, bad := range []string{`{"timeout_ms":-1}`, `{"timeout_ms":1860001}`} {
		_, err := knowledgePlanner(Choice{Use: "ollama", Options: map[string]json.RawMessage{"ollama": json.RawMessage(bad)}})
		if err == nil {
			t.Fatal("invalid provider timeout accepted")
		}
	}
}
