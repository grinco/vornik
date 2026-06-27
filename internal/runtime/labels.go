// Package runtime provides label constants for vornik-managed containers.
package runtime

// Label key constants for Podman container identification.
// These labels are applied to all containers managed by vornik
// and are used for stateless container discovery via podman CLI filters.
const (
	// LabelManaged identifies a container as managed by vornik.
	// Values: "true"
	LabelManaged = "vornik.managed"

	// LabelProjectID identifies which project a container belongs to.
	LabelProjectID = "vornik.projectId"

	// LabelRole identifies the agent role (e.g., "coder", "tester", "reviewer").
	LabelRole = "vornik.role"

	// LabelTaskID identifies the specific task the container is running.
	LabelTaskID = "vornik.taskId"
)

// Label value constants.
const (
	LabelValueTrue = "true"
)

// StandardLabelSet returns a complete set of labels for a vornik-managed container.
func StandardLabelSet(projectID, role, taskID string) map[string]string {
	return map[string]string{
		LabelManaged:   LabelValueTrue,
		LabelProjectID: projectID,
		LabelRole:      role,
		LabelTaskID:    taskID,
	}
}
