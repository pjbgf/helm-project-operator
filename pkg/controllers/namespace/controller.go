package namespace

import (
	"context"
	"fmt"
	"time"

	"github.com/rancher/helm-project-operator/pkg/controllers/common"
	helmprojectcontroller "github.com/rancher/helm-project-operator/pkg/generated/controllers/helm.cattle.io/v1alpha1"
	"github.com/rancher/wrangler/pkg/apply"
	corecontroller "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
)

type handler struct {
	namespaceApply apply.Apply
	apply          apply.Apply

	systemNamespace string
	valuesYaml      string
	questionsYaml   string
	opts            common.Options

	systemNamespaceTracker              Tracker
	projectRegistrationNamespaceTracker Tracker

	namespaces            corecontroller.NamespaceController
	namespaceCache        corecontroller.NamespaceCache
	configmaps            corecontroller.ConfigMapController
	projectHelmCharts     helmprojectcontroller.ProjectHelmChartController
	projectHelmChartCache helmprojectcontroller.ProjectHelmChartCache
}

func Register(
	ctx context.Context,
	apply apply.Apply,
	systemNamespace, valuesYaml, questionsYaml string,
	opts common.Options,
	namespaces corecontroller.NamespaceController,
	namespaceCache corecontroller.NamespaceCache,
	configmaps corecontroller.ConfigMapController,
	projectHelmCharts helmprojectcontroller.ProjectHelmChartController,
	projectHelmChartCache helmprojectcontroller.ProjectHelmChartCache,
	dynamic dynamic.Interface,
) ProjectGetter {

	apply = apply.WithCacheTypes(configmaps)

	h := &handler{
		apply:                               apply,
		systemNamespace:                     systemNamespace,
		valuesYaml:                          valuesYaml,
		questionsYaml:                       questionsYaml,
		opts:                                opts,
		systemNamespaceTracker:              NewTracker(),
		projectRegistrationNamespaceTracker: NewTracker(),
		namespaces:                          namespaces,
		namespaceCache:                      namespaceCache,
		configmaps:                          configmaps,
		projectHelmCharts:                   projectHelmCharts,
		projectHelmChartCache:               projectHelmChartCache,
	}

	h.apply = h.addReconcilers(h.apply, dynamic)

	h.initResolvers(ctx)

	h.initIndexers()

	if len(opts.ProjectLabel) == 0 {
		namespaces.OnChange(ctx, "on-namespace-change", h.OnSingleNamespaceChange)

		return NewSingleNamespaceProjectGetter(systemNamespace, opts.SystemNamespaces, namespaces)
	}

	// the namespaceApply is only needed in a multi-namespace setup
	// note: we never delete namespaces that are created since it's possible that the user may want to leave them around
	// on remove, we only output a log that says that the user should clean it up and add an annotation that it is orphaned
	h.namespaceApply = apply.
		WithSetID("project-registration-namespace-applier").
		WithCacheTypes(namespaces).
		WithNoDeleteGVK(namespaces.GroupVersionKind())

	namespaces.OnChange(ctx, "on-namespace-change", h.OnMultiNamespaceChange)

	h.initSystemNamespaces(h.opts.SystemNamespaces, h.systemNamespaceTracker)

	err := h.initProjectRegistrationNamespaces()
	if err != nil {
		logrus.Fatal(err)
	}

	return NewLabelBasedProjectGetter(h.opts.ProjectLabel, h.isProjectRegistrationNamespace, h.isSystemNamespace, h.namespaces)
}

// Single Namespace Handler

func (h *handler) OnSingleNamespaceChange(name string, namespace *corev1.Namespace) (*corev1.Namespace, error) {
	if namespace.Name != h.systemNamespace {
		// enqueue system namespace to ensure that rolebindings are updated
		h.namespaces.Enqueue(h.systemNamespace)
		return namespace, nil
	}
	if namespace.DeletionTimestamp != nil {
		// When a namespace gets deleted, the ConfigMap deployed in that namespace should also get deleted
		// Therefore, we do not need to apply anything in this situation to avoid spamming logs with trying to apply
		// a resource to a namespace that is being terminated
		return namespace, nil
	}
	// Trigger applying the data for this projectRegistrationNamespace
	var objs []runtime.Object
	objs = append(objs, h.getConfigMap("", namespace))
	return namespace, h.configureApplyForNamespace(namespace).ApplyObjects(objs...)
}

// Multiple Namespaces Handler

func (h *handler) OnMultiNamespaceChange(name string, namespace *corev1.Namespace) (*corev1.Namespace, error) {
	if namespace == nil {
		return namespace, nil
	}

	switch {
	// note: the check for a project registration namespace must happen before
	// we check for whether it is a system namespace to address the scenario where
	// the 'projectLabel: systemProjectLabelValue' is added to the project registration
	// namespace, which will cause it to be ignored and left in the System Project unless
	// we apply the ProjectRegistrationNamespace logic first.
	case h.isProjectRegistrationNamespace(namespace):
		err := h.enqueueProjectNamespaces(namespace)
		if err != nil {
			return namespace, err
		}
		if namespace.DeletionTimestamp != nil {
			h.projectRegistrationNamespaceTracker.Delete(namespace)
		}
		return namespace, nil
	case h.isSystemNamespace(namespace):
		// nothing to do, we always ignore system namespaces
		return namespace, nil
	default:
		err := h.applyProjectRegistrationNamespaceForNamespace(namespace)
		if err != nil {
			return namespace, err
		}
		return namespace, nil
	}
}

func (h *handler) enqueueProjectNamespaces(projectRegistrationNamespace *corev1.Namespace) error {
	if projectRegistrationNamespace == nil {
		return nil
	}
	// ensure that we are working with the projectRegistrationNamespace that we expect, not the one we found
	expectedNamespace, exists := h.projectRegistrationNamespaceTracker.Get(projectRegistrationNamespace.Name)
	if !exists {
		// we no longer expect this namespace to exist, so don't enqueue any namespaces
		return nil
	}
	// projectRegistrationNamespace was modified or removed, so we should re-enqueue any namespaces tied to it
	projectID, ok := expectedNamespace.Labels[h.opts.ProjectLabel]
	if !ok {
		return fmt.Errorf("could not find project that projectRegistrationNamespace %s is tied to", projectRegistrationNamespace.Name)
	}
	projectNamespaces, err := h.namespaceCache.GetByIndex(NamespacesByProjectExcludingRegistrationID, projectID)
	if err != nil {
		return err
	}
	for _, ns := range projectNamespaces {
		h.namespaces.EnqueueAfter(ns.Name, time.Second)
	}
	return nil
}

func (h *handler) applyProjectRegistrationNamespaceForNamespace(namespace *corev1.Namespace) error {
	// get the project ID and generate the namespace object to be applied
	projectID, inProject := h.getProjectIDFromNamespaceLabels(namespace)

	// update the namespace with the appropriate label on it
	err := h.updateNamespaceWithHelmOperatorProjectLabel(namespace, projectID, inProject)
	if err != nil {
		return nil
	}
	if !inProject {
		return nil
	}

	// Calculate whether to add the orphaned label
	var isOrphaned bool
	projectNamespaces, err := h.namespaceCache.GetByIndex(NamespacesByProjectExcludingRegistrationID, projectID)
	if err != nil {
		return err
	}
	var numNamespaces int
	for _, ns := range projectNamespaces {
		if ns.DeletionTimestamp != nil && ns.Name == namespace.Name {
			// ignore the namespace we are deleting, which can still be in the index
			continue
		}
		numNamespaces++
	}
	if numNamespaces == 0 {
		// add orphaned label and trigger a warning
		isOrphaned = true
	}

	// get the resources and validate them
	projectRegistrationNamespace := h.getProjectRegistrationNamespace(projectID, isOrphaned, namespace)
	// ensure that the projectRegistrationNamespace created from this projectID is valid
	if len(projectRegistrationNamespace.Name) > 63 {
		// ensure that we don't try to create a namespace with too big of a name
		logrus.Errorf("could not apply namespace with name %s: name is above 63 characters", projectRegistrationNamespace.Name)
		return nil
	}
	if projectRegistrationNamespace.Name == namespace.Name {
		// the only way this would happen is if h.isProjectRegistrationNamespace(namespace), which means the
		// the project registration namespace was removed from the cluster after it was orphaned (but still in the project
		// since it has the projectID label on it). In this case, we can safely ignore and continue
		return nil
	}

	// Trigger the apply and set the projectRegistrationNamespace
	err = h.namespaceApply.ApplyObjects(projectRegistrationNamespace)
	if err != nil {
		return err
	}

	// get the projectRegistrationNamespace after applying to get a valid object to pass in as the owner of the next apply
	projectRegistrationNamespace, err = h.namespaces.Get(projectRegistrationNamespace.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("unable to get project registration namespace from cache after create: %s", err)
	}
	h.projectRegistrationNamespaceTracker.Set(projectRegistrationNamespace)

	if projectRegistrationNamespace.DeletionTimestamp != nil {
		// When a namespace gets deleted, the ConfigMap deployed in that namespace and all ProjectHelmCharts should also get deleted
		// Therefore, we do not need to apply anything in this situation to avoid spamming logs with trying to apply
		// a resource to a namespace that is being terminated
		//
		// We expect this to be recalled when the project registration namespace is recreated anyways
		return nil
	}

	// Trigger applying the data for this projectRegistrationNamespace
	var objs []runtime.Object
	objs = append(objs, h.getConfigMap(projectID, projectRegistrationNamespace))
	err = h.configureApplyForNamespace(projectRegistrationNamespace).ApplyObjects(objs...)
	if err != nil {
		return err
	}

	// ensure that all ProjectHelmCharts are re-enqueued within this projectRegistrationNamespace
	err = h.enqueueProjectHelmChartsForNamespace(projectRegistrationNamespace)
	if err != nil {
		return fmt.Errorf("unable to re-enqueue ProjectHelmCharts on reconciling change to namespaces in project %s: %s", projectID, err)
	}

	return nil
}

func (h *handler) updateNamespaceWithHelmOperatorProjectLabel(namespace *corev1.Namespace, projectID string, inProject bool) error {
	if namespace.DeletionTimestamp != nil {
		// no need to update a namespace about to be deleted
		return nil
	}
	if len(h.opts.ProjectReleaseLabelValue) == 0 {
		// do nothing, this annotation is irrelevant unless we create release namespaces
		return nil
	}
	if len(projectID) == 0 || !inProject {
		// ensure that the HelmProjectOperatorProjectLabel is removed if added
		if namespace.Labels == nil {
			return nil
		}
		if _, ok := namespace.Labels[common.HelmProjectOperatorProjectLabel]; !ok {
			return nil
		}
		namespaceCopy := namespace.DeepCopy()
		delete(namespaceCopy.Labels, common.HelmProjectOperatorProjectLabel)
		_, err := h.namespaces.Update(namespaceCopy)
		if err != nil {
			return err
		}
	}

	namespaceCopy := namespace.DeepCopy()
	if namespaceCopy.Labels == nil {
		namespaceCopy.Labels = map[string]string{}
	}
	currLabel, ok := namespaceCopy.Labels[common.HelmProjectOperatorProjectLabel]
	if !ok || currLabel != projectID {
		namespaceCopy.Labels[common.HelmProjectOperatorProjectLabel] = projectID
	}
	_, err := h.namespaces.Update(namespaceCopy)
	if err != nil {
		return err
	}
	return nil
}

func (h *handler) isProjectRegistrationNamespace(namespace *corev1.Namespace) bool {
	if namespace == nil {
		return false
	}
	return h.projectRegistrationNamespaceTracker.Has(namespace.Name)
}

func (h *handler) isSystemNamespace(namespace *corev1.Namespace) bool {
	if namespace == nil {
		return false
	}
	isTrackedSystemNamespace := h.systemNamespaceTracker.Has(namespace.Name)
	if isTrackedSystemNamespace {
		return true
	}

	var systemProjectLabelValues []string
	if len(h.opts.SystemProjectLabelValues) != 0 {
		systemProjectLabelValues = append(systemProjectLabelValues, h.opts.SystemProjectLabelValues...)
	}
	if len(h.opts.ProjectReleaseLabelValue) != 0 {
		systemProjectLabelValues = append(systemProjectLabelValues, h.opts.ProjectReleaseLabelValue)
	}
	projectID, inProject := h.getProjectIDFromNamespaceLabels(namespace)
	if !inProject {
		return false
	}
	for _, systemProjectLabelValue := range systemProjectLabelValues {
		// check if labels indicate this is a system project
		if projectID == systemProjectLabelValue {
			return true
		}
	}
	return false
}
