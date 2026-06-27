package ui

// AI-assist grounding resolution.
//
// The swarm/workflow editors' AI-assist grounds on a project (its brief,
// budget, scope) identified by projectId. That id is normally carried in
// the ?projectId= query when the editor is reached FROM a project page.
// Opened directly via the /ui/swarms or /ui/workflows list there is no
// such param, so the assist used to POST an empty projectId and get back
// "project not found" (incident 2026-06-17).
//
// A swarm/workflow has no single owning project (it can be shared), but
// for grounding any project that references it is a sound default. These
// resolvers pick the first such project; the editor uses it only when no
// explicit ?projectId= was supplied (explicit always wins). These
// surfaces are admin-only (see sessionUserGlobalAuthoringPrefixes), so
// the resolved project doesn't cross a tenant scope.

// defaultAssistProjectForSwarm returns the first project whose swarmId
// references swarmID, or "" when none do (assist stays disabled rather
// than grounding on an unrelated project).
func (s *Server) defaultAssistProjectForSwarm(swarmID string) string {
	if s.projectReg == nil || swarmID == "" {
		return ""
	}
	for _, p := range s.projectReg.ListProjects() {
		if p.SwarmID == swarmID {
			return p.ID
		}
	}
	return ""
}

// defaultAssistProjectForWorkflow returns the first project that uses
// workflowID as its default or an adaptive candidate, or "" when none do.
func (s *Server) defaultAssistProjectForWorkflow(workflowID string) string {
	if s.projectReg == nil || workflowID == "" {
		return ""
	}
	for _, p := range s.projectReg.ListProjects() {
		if p.DefaultWorkflowID == workflowID {
			return p.ID
		}
		for _, w := range p.AdaptiveCandidateWorkflows {
			if w == workflowID {
				return p.ID
			}
		}
	}
	return ""
}

// assistProjectFromRequest returns the explicit ?projectId= when present,
// otherwise the resolved fallback. Centralises the "explicit wins, else
// resolve owner" rule the editors share.
func assistProjectFromRequest(explicit, fallback string) string {
	if explicit != "" {
		return explicit
	}
	return fallback
}
