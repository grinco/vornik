// Package graph implements the multi-stage knowledge-graph
// extraction pipeline that turns project_memory_chunks rows into
// knowledge_entities + knowledge_edges rows.
//
// Stages (each its own file):
//
//	extractor.go   — pure NER from chunk text → []Candidate
//	resolver.go    — match candidates against existing entities
//	relationship.go — derive edges between resolved entities
//	validator.go   — faithfulness check on proposed edges
//	pipeline.go    — orchestrator stitching the four stages together
//
// Each stage is independently testable with a fake chat.Provider.
// The orchestrator owns DB writes and never lets a partial failure
// poison the existing graph (chunk stays flagged for re-extraction
// until the full pipeline succeeds).
//
// Design lives in https://docs.vornik.io
package graph
