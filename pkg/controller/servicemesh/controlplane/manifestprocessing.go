package controlplane

import (
	"context"
	"strings"

	"github.com/ghodss/yaml"

	v1 "github.com/maistra/istio-operator/pkg/apis/maistra/v1"
	"github.com/maistra/istio-operator/pkg/controller/common"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	"k8s.io/helm/pkg/manifest"
	"k8s.io/helm/pkg/releaseutil"

	"k8s.io/kubernetes/pkg/kubectl"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *ControlPlaneReconciler) processComponentManifests(componentName string) (err error) {
	origLogger := r.Log
	r.Log = r.Log.WithValues("Component", componentName)
	defer func() { r.Log = origLogger }()

	renderings, hasRenderings := r.renderings[componentName]
	if !hasRenderings {
		r.Log.Info("no renderings for component")
		return nil
	}

	r.Log.Info("reconciling component resources")
	status := r.Instance.Status.FindComponentByName(componentName)
	if status == nil {
		status = v1.NewComponentStatus()
		status.Resource = componentName
	} else {
		status.RemoveCondition(v1.ConditionTypeReconciled)
	}

	status, err = r.processManifests(renderings, status) // TODO: handle error
	status.ObservedGeneration = r.Instance.GetGeneration()
	if err := r.postProcessNewComponent(componentName, status); err != nil {
		r.Log.Error(err, "unexpected error occurred during postprocessing of new component")
		return err
	}
	r.Status.ComponentStatus = append(r.Status.ComponentStatus, status)
	r.Log.Info("component reconciliation complete")
	return err
}

func (r *ControlPlaneReconciler) processManifests(manifests []manifest.Manifest,
	oldStatus *v1.ComponentStatus) (*v1.ComponentStatus, error) {

	allErrors := []error{}
	resourcesProcessed := map[v1.ResourceKey]struct{}{}
	newStatus := v1.NewComponentStatus()
	newStatus.StatusType = oldStatus.StatusType
	newStatus.Resource = oldStatus.Resource

	origLogger := r.Log
	defer func() { r.Log = origLogger }()
	for _, manifest := range manifests {
		r.Log = origLogger.WithValues("manifest", manifest.Name)
		if !strings.HasSuffix(manifest.Name, ".yaml") {
			r.Log.V(2).Info("Skipping rendering of manifest")
			continue
		}
		r.Log.V(2).Info("Processing resources from manifest")
		// split the manifest into individual objects
		objects := releaseutil.SplitManifests(manifest.Content)
		for _, raw := range objects {
			rawJSON, err := yaml.YAMLToJSON([]byte(raw))
			if err != nil {
				r.Log.Error(err, "unable to convert raw data to JSON")
				allErrors = append(allErrors, err)
				continue
			}
			obj := &unstructured.Unstructured{}
			_, _, err = unstructured.UnstructuredJSONScheme.Decode(rawJSON, nil, obj)
			if err != nil {
				r.Log.Error(err, "unable to decode object into Unstructured")
				allErrors = append(allErrors, err)
				continue
			}
			err = r.processObject(obj, resourcesProcessed, oldStatus, newStatus)
			if err != nil {
				allErrors = append(allErrors, err)
			}
		}
	}

	// handle deletions
	// XXX: should these be processed in reverse order of creation?
	for index := len(oldStatus.Resources) - 1; index >= 0; index-- {
		status := oldStatus.Resources[index]
		resourceKey := v1.ResourceKey(status.Resource)
		if _, ok := resourcesProcessed[resourceKey]; !ok {
			r.Log = origLogger.WithValues("Resource", resourceKey)
			if condition := status.GetCondition(v1.ConditionTypeInstalled); condition.Status != v1.ConditionStatusFalse {
				r.Log.Info("deleting resource")
				unstructured := resourceKey.ToUnstructured()
				err := r.Client.Delete(context.TODO(), unstructured, client.PropagationPolicy(metav1.DeletePropagationForeground))
				updateDeleteStatus(status, err)
				newStatus.Resources = append(newStatus.Resources, status)
				if err == nil || errors.IsNotFound(err) || errors.IsGone(err) {
					status.ObservedGeneration = 0
					// special handling
					if err := r.processDeletedObject(unstructured); err != nil {
						r.Log.Error(err, "unexpected error occurred during cleanup of deleted resource")
					}
				} else {
					r.Log.Error(err, "error deleting resource")
					allErrors = append(allErrors, err)
				}
			}
		}
	}
	err := utilerrors.NewAggregate(allErrors)
	if len(manifests) > 0 {
		updateReconcileStatus(&newStatus.StatusType, err)
	} else {
		updateDeleteStatus(&newStatus.StatusType, err)
	}
	return newStatus, err
}

func (r *ControlPlaneReconciler) processObject(obj *unstructured.Unstructured, resourcesProcessed map[v1.ResourceKey]struct{},
	oldStatus *v1.ComponentStatus, newStatus *v1.ComponentStatus) error {
	origLogger := r.Log
	defer func() { r.Log = origLogger }()

	key := v1.NewResourceKey(obj, obj)
	r.Log = origLogger.WithValues("Resource", key)

	if obj.GetKind() == "List" {
		allErrors := []error{}
		list, err := obj.ToList()
		if err != nil {
			r.Log.Error(err, "error converting List object")
			return err
		}
		for _, item := range list.Items {
			err = r.processObject(&item, resourcesProcessed, oldStatus, newStatus)
			if err != nil {
				allErrors = append(allErrors, err)
			}
		}
		return utilerrors.NewAggregate(allErrors)
	}

	// Add owner ref
	if obj.GetNamespace() == r.Instance.GetNamespace() {
		obj.SetOwnerReferences(r.ownerRefs)
	} else {
		// XXX: can't set owner reference on cross-namespace or cluster resources
	}

	// add owner label
	common.SetLabel(obj, common.OwnerKey, r.Instance.GetNamespace())
	// add generation annotation
	common.SetAnnotation(obj, common.MeshGenerationKey, r.meshGeneration)

	r.Log.V(2).Info("beginning reconciliation of resource", "ResourceKey", key)

	resourcesProcessed[key] = seen
	status := oldStatus.FindResourceByKey(key)
	if status == nil {
		newResourceStatus := v1.NewStatus()
		status = &newResourceStatus
		status.Resource = string(key)
	}
	newStatus.Resources = append(newStatus.Resources, status)

	err := r.preprocessObject(obj)
	if err != nil {
		r.Log.Error(err, "error preprocessing object")
		updateReconcileStatus(status, err)
		return err
	}

	err = kubectl.CreateApplyAnnotation(obj, unstructured.UnstructuredJSONScheme)
	if err != nil {
		r.Log.Error(err, "unexpected error adding apply annotation to object")
	}

	receiver := key.ToUnstructured()
	objectKey, err := client.ObjectKeyFromObject(receiver)
	if err != nil {
		r.Log.Error(err, "client.ObjectKeyFromObject() failed for resource")
		// This can only happen if reciever isn't an unstructured.Unstructured
		// i.e. this should never happen
		updateReconcileStatus(status, err)
		return err
	}

	var patch common.Patch

	err = r.Client.Get(context.TODO(), objectKey, receiver)
	if err != nil {
		if errors.IsNotFound(err) {
			r.Log.Info("creating resource")
			err = r.Client.Create(context.TODO(), obj)
			if err == nil {
				status.ObservedGeneration = 1
				// special handling
				if err := r.processNewObject(obj); err != nil {
					// just log for now
					r.Log.Error(err, "unexpected error occurred during postprocessing of new resource")
				}
			} else {
				r.Log.Error(err, "unexpected error occurred during creation of new resource")
			}
		}
	} else if patch, err = r.PatchFactory.CreatePatch(receiver, obj); err == nil && patch != nil {
		r.Log.Info("updating existing resource")
		status.RemoveCondition(v1.ConditionTypeReconciled)
		_, err = patch.Apply()
	}
	r.Log.V(2).Info("resource reconciliation complete")
	updateReconcileStatus(status, err)
	if err != nil {
		r.Log.Error(err, "error occurred reconciling resource")
	}
	return err
}
