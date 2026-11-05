package main

import (
	"context"
	"fmt"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/connectors/documents"
)

type documentParser struct{ documents.Parser }

func (documentParser) Validate(name string, data []byte) error {
	return documents.Validate(name, data)
}

func (p documentParser) Extract(ctx context.Context, name string, data []byte) ([]niromem.DocumentPage, error) {
	pages, err := p.Parser.Extract(ctx, name, data)
	if err != nil {
		return nil, err
	}
	out := make([]niromem.DocumentPage, len(pages))
	for i, page := range pages {
		out[i] = niromem.DocumentPage{Number: page.Number, Text: page.Text}
	}
	return out, nil
}

func configureDocuments(cfg *Config, o *niromem.Options) error {
	if !cfg.Documents.Enabled {
		return nil
	}
	if o.Blob == nil {
		return fmt.Errorf("documents.enabled requires durable blob storage (configure blob.dir or blob.s3)")
	}
	n := cfg.Documents.Concurrency
	if n == 0 {
		n = 2
	}
	if n < 1 || n > 4 {
		return fmt.Errorf("documents.concurrency must be 1..4")
	}
	o.Documents = documentParser{documents.Parser{Concurrency: n}}
	return nil
}
