package project

import (
	helmlockerv1alpha1 "github.com/aiyengar2/helm-locker/pkg/apis/helm.cattle.io/v1alpha1"
	v1alpha1 "github.com/aiyengar2/helm-project-operator/pkg/apis/helm.cattle.io/v1alpha1"
	"github.com/aiyengar2/helm-project-operator/pkg/controllers/common"
	helmcontrollerv1 "github.com/k3s-io/helm-controller/pkg/apis/helm.cattle.io/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Note: each resource created here should have a resolver set in resolvers.go
// The only exception is ProjectHelmCharts since those are handled by the main generating controller

func (h *handler) getHelmChart(projectID string, valuesContent string, projectHelmChart *v1alpha1.ProjectHelmChart) *helmcontrollerv1.HelmChart {
	// must be in system namespace since helm controllers are configured to only watch one namespace
	jobImage := DefaultJobImage
	if len(h.opts.HelmJobImage) > 0 {
		jobImage = h.opts.HelmJobImage
	}
	releaseNamespace, releaseName := h.getReleaseNamespaceAndName(projectHelmChart)
	helmChart := helmcontrollerv1.NewHelmChart(h.systemNamespace, releaseName, helmcontrollerv1.HelmChart{
		Spec: helmcontrollerv1.HelmChartSpec{
			TargetNamespace: releaseNamespace,
			Chart:           releaseName,
			JobImage:        jobImage,
			ChartContent:    h.opts.ChartContent,
			ValuesContent:   valuesContent,
		},
	})
	helmChart.SetLabels(common.GetHelmResourceLabels(projectID, projectHelmChart.Spec.HelmApiVersion))
	return helmChart
}

func (h *handler) getHelmRelease(projectID string, projectHelmChart *v1alpha1.ProjectHelmChart) *helmlockerv1alpha1.HelmRelease {
	// must be in system namespace since helmlocker controllers are configured to only watch one namespace
	releaseNamespace, releaseName := h.getReleaseNamespaceAndName(projectHelmChart)
	helmRelease := helmlockerv1alpha1.NewHelmRelease(h.systemNamespace, releaseName, helmlockerv1alpha1.HelmRelease{
		Spec: helmlockerv1alpha1.HelmReleaseSpec{
			Release: helmlockerv1alpha1.ReleaseKey{
				Namespace: releaseNamespace,
				Name:      releaseName,
			},
		},
	})
	helmRelease.SetLabels(common.GetHelmResourceLabels(projectID, projectHelmChart.Spec.HelmApiVersion))
	return helmRelease
}

func (h *handler) getProjectReleaseNamespace(projectID string, isOrphaned bool, projectHelmChart *v1alpha1.ProjectHelmChart) *v1.Namespace {
	releaseNamespace, _ := h.getReleaseNamespaceAndName(projectHelmChart)
	if releaseNamespace == h.systemNamespace || releaseNamespace == projectHelmChart.Namespace {
		return nil
	}
	projectReleaseNamespace := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        releaseNamespace,
			Annotations: common.GetProjectNamespaceAnnotations(h.opts.SystemProjectLabelValue, h.opts.ProjectLabel, h.opts.ClusterID),
			Labels:      common.GetProjectNamespaceLabels(projectID, h.opts.ProjectLabel, h.opts.SystemProjectLabelValue, isOrphaned),
		},
	}
	return projectReleaseNamespace
}

func (h *handler) getRoleBindings(projectID string, k8sRoleToRoleRefs map[string][]rbacv1.RoleRef, k8sRoleToSubjects map[string][]rbacv1.Subject, projectHelmChart *v1alpha1.ProjectHelmChart) []runtime.Object {
	var objs []runtime.Object
	releaseNamespace, _ := h.getReleaseNamespaceAndName(projectHelmChart)

	for subjectRole := range common.GetDefaultClusterRoles(h.opts) {
		// note: these role refs point to roles in the release namespace
		roleRefs := k8sRoleToRoleRefs[subjectRole]
		// note: these subjects are inferred from the rolebindings tied to the default roles in the registration namespace
		subjects := k8sRoleToSubjects[subjectRole]
		if len(subjects) == 0 {
			// no need to create empty RoleBindings
			continue
		}
		for _, roleRef := range roleRefs {
			objs = append(objs, &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:      roleRef.Name,
					Namespace: releaseNamespace,
					Labels:    common.GetCommonLabels(projectID),
				},
				RoleRef:  roleRef,
				Subjects: subjects,
			})
		}
	}

	return objs
}
