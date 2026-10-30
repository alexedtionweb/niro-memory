package search

import (
	"math"
	"time"
)

// Policy configures retrieval. Policies are plain data: they can be stored
// per tenant and selected by name per request.
type Policy struct {
	Name string `json:"name"`
	// Fusion is "additive" (normalized weighted sum, thresholdable) or "rrf".
	Fusion string `json:"fusion,omitempty"`
	// Relevance signal weights (additive fusion).
	Lexical float64 `json:"lexical"`
	Vector  float64 `json:"vector"`
	Entity  float64 `json:"entity"`
	// Prior weights: recency, confidence and importance blend into the score
	// with weight PriorShare.
	PriorShare      float64       `json:"prior_share"`
	Recency         float64       `json:"recency"`
	Confidence      float64       `json:"confidence"`
	Importance      float64       `json:"importance"`
	RecencyHalfLife time.Duration `json:"recency_half_life"`
	// RRFK is the RRF constant (60 in Cormack et al. 2009).
	RRFK float64 `json:"rrf_k,omitempty"`
	// MMRLambda trades relevance (1) against diversity (0). 1 disables MMR.
	MMRLambda float64 `json:"mmr_lambda"`
	// EmbedBudget bounds how long a search waits for a query embedding.
	// Zero never embeds query text (vectors passed by the caller still work).
	EmbedBudget time.Duration `json:"embed_budget"`
	// Overfetch multiplies the limit for candidate generation (min 60 total).
	Overfetch int `json:"overfetch"`
	// GraphHops is how far entity expansion goes: 0 none, 1 direct, 2 neighbours.
	GraphHops int `json:"graph_hops"`
	// Threshold drops hits whose final score is lower.
	Threshold float64 `json:"threshold"`
}

// Default policies. "default" balances quality and latency; "fast" and
// "voice" never wait for embeddings; "deep" spends more for recall.
var Default = map[string]Policy{
	"default": {Name: "default", Fusion: "additive", Lexical: 1, Vector: 1, Entity: 0.5,
		PriorShare: 0.2, Recency: 1, Confidence: 1, Importance: 1, RecencyHalfLife: 30 * 24 * time.Hour,
		RRFK: 60, MMRLambda: 0.7, EmbedBudget: 50 * time.Millisecond, Overfetch: 4, GraphHops: 1},
	"fast": {Name: "fast", Fusion: "additive", Lexical: 1, Vector: 1, Entity: 0.5,
		PriorShare: 0.2, Recency: 1, Confidence: 1, Importance: 1, RecencyHalfLife: 30 * 24 * time.Hour,
		RRFK: 60, MMRLambda: 1, Overfetch: 2, GraphHops: 1},
	"voice": {Name: "voice", Fusion: "additive", Lexical: 1, Vector: 1, Entity: 0.7,
		PriorShare: 0.3, Recency: 2, Confidence: 1, Importance: 1, RecencyHalfLife: 7 * 24 * time.Hour,
		RRFK: 60, MMRLambda: 1, Overfetch: 2},
	"deep": {Name: "deep", Fusion: "additive", Lexical: 1, Vector: 1.2, Entity: 0.5,
		PriorShare: 0.15, Recency: 1, Confidence: 1, Importance: 1, RecencyHalfLife: 90 * 24 * time.Hour,
		RRFK: 60, MMRLambda: 0.6, EmbedBudget: 500 * time.Millisecond, Overfetch: 8, GraphHops: 2},
}

// bm25Sigmoid squashes a raw BM25 score into [0,1]. Longer queries produce
// larger raw scores, so the midpoint and steepness depend on the number of
// query terms (parameters as in Mem0's scoring).
func bm25Sigmoid(raw float64, terms int) float64 {
	mid, steep := 5.0, 0.7
	switch {
	case terms > 15:
		mid, steep = 12, 0.5
	case terms > 9:
		mid, steep = 10, 0.5
	case terms > 6:
		mid, steep = 9, 0.5
	case terms > 3:
		mid, steep = 7, 0.6
	}
	return 1 / (1 + math.Exp(-steep*(raw-mid)))
}

// entityBoost dampens entities linked to many memories: a mention of a
// ubiquitous entity (the user themself) says little about relevance.
func entityBoost(similarity float64, linked int) float64 {
	n := float64(linked - 1)
	if n < 0 {
		n = 0
	}
	return similarity / (1 + 0.001*n*n)
}

// recency is an exponential decay with the policy half-life.
func recency(age, halfLife time.Duration) float64 {
	if halfLife <= 0 || age <= 0 {
		return 1
	}
	return math.Exp2(-float64(age) / float64(halfLife))
}
