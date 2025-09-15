package controlplanecomponent

import (
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/util"

	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func AdaptPodDisruptionBudget() option {
	return WithAdaptFunction(func(cpContext WorkloadContext, pdb *policyv1.PodDisruptionBudget) error {
		var minAvailable *intstr.IntOrString
		var maxUnavailable *intstr.IntOrString
		switch cpContext.HCP.Spec.ControllerAvailabilityPolicy {
		case hyperv1.SingleReplica:
			minAvailable = ptr.To(intstr.FromInt32(1))
		case hyperv1.HighlyAvailable:
			maxUnavailable = ptr.To(intstr.FromInt32(1))
		}

		pdb.Spec.MinAvailable = minAvailable
		pdb.Spec.MaxUnavailable = maxUnavailable
		return nil
	})
}

// SetHostedClusterAnnotation is a helper function to set the HostedCluster annotation on a resource.
// This is useful for resources created by the HostedCluster controller, so external changes can be detected and reconciled.
func SetHostedClusterAnnotation() option {
	return func(ga *genericAdapter) {
		ga.adapt = func(cpContext WorkloadContext, resource client.Object) error {
			annotations := resource.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[util.HostedClusterAnnotation] = cpContext.HCP.Annotations[util.HostedClusterAnnotation]
			resource.SetAnnotations(annotations)
			return nil
		}
	}
}

// DisableIfAnnotationExist is a helper predicate for the common use case of disabling a resource when an annotation exists.
func DisableIfAnnotationExist(annotation string) option {
	return WithPredicate(func(cpContext WorkloadContext) bool {
		if _, exists := cpContext.HCP.Annotations[annotation]; exists {
			return false
		}
		return true
	})
}

// KeepManifestIfAnnotationExists can be used to prevent deletion of a disabled resource: DisableIfAnnotationExists().
// This is useful, for example, when PKI Reconciliation is disabled:
// Hypershift should not reconcile, but also should not delete user-created secrets.
func KeepManifestIfAnnotationExists(annotation string) option {
	return WithKeepManifest(func(cpContext WorkloadContext) bool {
		_, exists := cpContext.HCP.Annotations[annotation]
		return exists
	})
}

// EnableForPlatform is a helper predicate for the common use case of only enabling a resource for a specific platform.
func EnableForPlatform(platform hyperv1.PlatformType) option {
	return WithPredicate(func(cpContext WorkloadContext) bool {
		return cpContext.HCP.Spec.Platform.Type == platform
	})
}
