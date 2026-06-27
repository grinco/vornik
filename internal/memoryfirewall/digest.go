package memoryfirewall

// Canonicalisation + sha256 of a Policy. The digest gives
// external verifiers a stable identifier for "this chunk's
// policy revision at the time of decision X". Two retrievals
// against the same chunk at the same metadata revision produce
// the same digest; a digest change signals a policy update.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

// canonicalPolicy is the wire shape the digest is computed
// over. Ordering matters: fields are arranged alphabetically;
// slice contents are sorted; pointer-typed times serialise as
// RFC3339Nano (nil = empty string). The result feeds sha256.
//
// Why a separate canonical form vs. just hashing the Policy
// struct: Go's json.Marshal preserves struct field order but
// gives no guarantees about map iteration. The slice fields
// (PermittedRoles, AllowedPurposes) have caller-controlled
// order which we explicitly normalise.
type canonicalPolicy struct {
	AllowedPurposes      []string `json:"allowed_purposes"`
	ExpiresAt            string   `json:"expires_at"`
	PermittedRoles       []string `json:"permitted_roles"`
	ProvenanceProducerID string   `json:"provenance_producer_id"`
	ProvenanceSource     string   `json:"provenance_source"`
	ProvenanceSourceURL  string   `json:"provenance_source_url"`
	ProvenanceTrustLevel int      `json:"provenance_trust_level"`
	Sensitivity          string   `json:"sensitivity"`
	TenantID             string   `json:"tenant_id"`
}

// PolicyDigest returns the canonical sha256 hex over p.
func PolicyDigest(p Policy) string {
	c := canonicalPolicy{
		AllowedPurposes:      purposesToStrings(p.AllowedPurposes),
		ExpiresAt:            timeOrEmpty(p.ExpiresAt),
		PermittedRoles:       sortedCopy(p.PermittedRoles),
		ProvenanceProducerID: p.Provenance.ProducerID,
		ProvenanceSource:     string(p.Provenance.Source),
		ProvenanceSourceURL:  p.Provenance.SourceURL,
		ProvenanceTrustLevel: p.Provenance.TrustLevel,
		Sensitivity:          string(p.Sensitivity),
		TenantID:             p.TenantID,
	}
	// json.Marshal on a struct with stable field order +
	// pre-sorted slices is deterministic on Go's encoding/json
	// (per the package's documented behaviour for non-map
	// values).
	raw, err := json.Marshal(c)
	if err != nil {
		// Unreachable: the canonical struct has only string /
		// int / []string fields. A panic here means encoding/json
		// itself is broken; surface a sentinel digest rather
		// than crash the recall hot path.
		return "digest_error"
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// purposesToStrings sorts + de-duplicates the slice. Empty
// input returns nil so the canonical JSON marshals as `null`
// (matches the "no purpose restriction" semantic).
func purposesToStrings(ps []Purpose) []string {
	if len(ps) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ps))
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		s := string(p)
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// sortedCopy returns a sorted de-duped copy of in. Same null
// semantics as purposesToStrings.
func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func timeOrEmpty(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
