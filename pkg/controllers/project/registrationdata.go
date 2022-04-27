package project

import (
	"fmt"

	v1alpha1 "github.com/aiyengar2/helm-project-operator/pkg/apis/helm.cattle.io/v1alpha1"
	"github.com/aiyengar2/helm-project-operator/pkg/controllers/common"
	rbacv1 "k8s.io/api/rbac/v1"
)

// Note: each resource created here should have a resolver set in resolvers.go

func (h *handler) getSubjectRoleToSubjectsFromBindings(projectHelmChart *v1alpha1.ProjectHelmChart) (map[string][]rbacv1.Subject, error) {
	defaultClusterRoles := common.GetDefaultClusterRoles(h.opts)
	subjectRoleToSubjects := make(map[string][]rbacv1.Subject)
	subjectRoleToSubjectMap := make(map[string]map[string]rbacv1.Subject)
	if len(defaultClusterRoles) == 0 {
		// no roles to get get subjects for
		return subjectRoleToSubjects, nil
	}
	for subjectRole := range defaultClusterRoles {
		subjectRoleToSubjectMap[subjectRole] = make(map[string]rbacv1.Subject)
	}
	roleBindings, err := h.rolebindingCache.GetByIndex(
		RoleBindingInRegistrationNamespaceByRoleRef,
		NamespacedBindingReferencesDefaultOperatorRole(projectHelmChart.Namespace),
	)
	if err != nil {
		return nil, err
	}
	for _, rb := range roleBindings {
		if rb == nil {
			continue
		}
		subjectRole, isDefaultRoleRef := common.IsDefaultClusterRoleRef(h.opts, rb.RoleRef.Name)
		if !isDefaultRoleRef {
			continue
		}
		filteredSubjects := common.FilterToUsersAndGroups(rb.Subjects)
		currSubjects := subjectRoleToSubjectMap[subjectRole]
		for _, filteredSubject := range filteredSubjects {
			// collect into a map to avoid putting duplicates of the same subject
			// we use an index of kind and name since a Group can have the same name as a User, but should be considered separate
			currSubjects[fmt.Sprintf("%s-%s", filteredSubject.Kind, filteredSubject.Name)] = filteredSubject
		}
	}
	clusterRoleBindings, err := h.clusterrolebindingCache.GetByIndex(ClusterRoleBindingByRoleRef, BindingReferencesDefaultOperatorRole)
	if err != nil {
		return nil, err
	}
	for _, crb := range clusterRoleBindings {
		if crb == nil {
			continue
		}
		subjectRole, isDefaultRoleRef := common.IsDefaultClusterRoleRef(h.opts, crb.RoleRef.Name)
		if !isDefaultRoleRef {
			continue
		}
		filteredSubjects := common.FilterToUsersAndGroups(crb.Subjects)
		currSubjects := subjectRoleToSubjectMap[subjectRole]
		for _, filteredSubject := range filteredSubjects {
			// collect into a map to avoid putting duplicates of the same subject
			// we use an index of kind and name since a Group can have the same name as a User, but should be considered separate
			currSubjects[fmt.Sprintf("%s-%s", filteredSubject.Kind, filteredSubject.Name)] = filteredSubject
		}
	}
	// convert back into list so that no duplicates are created
	for subjectRole := range defaultClusterRoles {
		subjects := []rbacv1.Subject{}
		for _, subject := range subjectRoleToSubjectMap[subjectRole] {
			subjects = append(subjects, subject)
		}
		subjectRoleToSubjects[subjectRole] = subjects
	}
	return subjectRoleToSubjects, nil
}
