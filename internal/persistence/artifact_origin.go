package persistence

// ArtifactOrigin classifies the provenance of an artifact's content.
// Used by the outputguard to decide whether injection-class rules apply
// when the artifact is read back via read_artifact.
type ArtifactOrigin string

const (
	// ArtifactOriginTaskOutput is content authored by the agent/task itself.
	// read_artifact maps this to ProvenanceFirstParty.
	ArtifactOriginTaskOutput ArtifactOrigin = "task_output"
	// ArtifactOriginWebScrape is content fetched from an external source
	// (scraper, web_fetch, MCP-derived). Always third-party.
	ArtifactOriginWebScrape ArtifactOrigin = "web_scrape"
	// ArtifactOriginUpload is content supplied by the operator/user
	// (Telegram upload, API attachment, email attachment). Third-party
	// from the injection-guard's perspective.
	ArtifactOriginUpload ArtifactOrigin = "upload"
	// ArtifactOriginUnknown is the default for legacy rows or sites where
	// origin cannot be determined. Treated as third-party (fail-safe).
	ArtifactOriginUnknown ArtifactOrigin = "unknown"
)
