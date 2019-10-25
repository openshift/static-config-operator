package sync

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// isDouble indicates if we should ignore a given GroupKind because it is
// accessible via a different API route.
func isDouble(gk schema.GroupKind) bool {
	switch gk.String() {
	case "ClusterRole.authorization.openshift.io", // ClusterRole.rbac.authorization.k8s.io
		"ClusterRoleBinding.authorization.openshift.io", // ClusterRoleBinding.rbac.authorization.k8s.io
		"Role.authorization.openshift.io",               // Role.rbac.authorization.k8s.io
		"RoleBinding.authorization.openshift.io",        // RoleBinding.rbac.authorization.k8s.io
		"DaemonSet.extensions",                          // DaemonSet.apps
		"Deployment.extensions",                         // Deployment.apps
		"ImageStreamTag.image.openshift.io",             // ImageStream.image.openshift.io
		"ReplicaSet.extensions",                         // ReplicaSet.apps
		"Project.project.openshift.io",                  // Namespace
		"SecurityContextConstraints":                    // SecurityContextConstraints.security.openshift.io
		return true
	}

	return false
}
