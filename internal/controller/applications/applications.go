package applications

import (
	"context"
	"fmt"

	argocd "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/fields"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	kargoapi "github.com/akuity/kargo/api/v1alpha1"
	"github.com/akuity/kargo/internal/controller"
	"github.com/akuity/kargo/internal/kubeclient"
	"github.com/akuity/kargo/internal/logging"
)

// reconciler reconciles Argo CD Application resources.
type reconciler struct {
	kubeClient client.Client
}

// SetupReconcilerWithManager initializes a reconciler for Argo CD Application
// resources and registers it with the provided Manager.
func SetupReconcilerWithManager(
	ctx context.Context,
	kargoMgr manager.Manager,
	argoMgr manager.Manager,
	shardName string,
) error {
	// Index Stages by Argo CD Applications
	if err := kubeclient.IndexStagesByArgoCDApplications(ctx, kargoMgr, shardName); err != nil {
		return errors.Wrap(err, "index Stages by Argo CD Applications")
	}
	return ctrl.NewControllerManagedBy(argoMgr).
		For(&argocd.Application{}).
		Complete(newReconciler(kargoMgr.GetClient()))
}

func indexStagesByApp(shardName string) func(client.Object) []string {
	return func(obj client.Object) []string {
		// Return early if:
		//
		// 1. This is the default controller, but the object is labeled for a
		//    specific shard.
		//
		// 2. This is a shard-specific controller, but the object is not labeled for
		//    this shard.
		objShardName, labeled := obj.GetLabels()[controller.ShardLabelKey]
		if (shardName == "" && labeled) ||
			(shardName != "" && shardName != objShardName) {
			return nil
		}

		stage := obj.(*kargoapi.Stage) // nolint: forcetypeassert
		if stage.Spec.PromotionMechanisms == nil ||
			len(stage.Spec.PromotionMechanisms.ArgoCDAppUpdates) == 0 {
			return nil
		}
		apps := make([]string, len(stage.Spec.PromotionMechanisms.ArgoCDAppUpdates))
		for i, appCheck := range stage.Spec.PromotionMechanisms.ArgoCDAppUpdates {
			apps[i] =
				fmt.Sprintf("%s:%s", appCheck.AppNamespaceOrDefault(), appCheck.AppName)
		}
		return apps
	}
}

func newReconciler(kubeClient client.Client) *reconciler {
	return &reconciler{
		kubeClient: kubeClient,
	}
}

// Reconcile is part of the main Kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *reconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	result := ctrl.Result{}

	logger := logging.LoggerFromContext(ctx).WithFields(log.Fields{
		"applicationNamespace": req.NamespacedName.Namespace,
		"application":          req.NamespacedName.Name,
	})
	logger.Debug("reconciling Argo CD Application")

	// Find all Stages associated with this Application
	stages := &kargoapi.StageList{}
	if err := r.kubeClient.List(
		ctx,
		stages,
		&client.ListOptions{
			FieldSelector: fields.OneTermEqualSelector(
				kubeclient.StagesByArgoCDApplicationsIndexField,
				fmt.Sprintf(
					"%s:%s",
					req.NamespacedName.Namespace,
					req.NamespacedName.Name,
				),
			),
		},
	); err != nil {
		return result, errors.Wrapf(
			err,
			"error listing Stages for Application %q in namespace %q",
			req.NamespacedName.Name,
			req.NamespacedName.Namespace,
		)
	}

	// Force associated Stages to reconcile by patching an annotation
	var errs []error
	for _, e := range stages.Items {
		stage := e // This is to sidestep implicit memory aliasing in this for loop
		objKey := client.ObjectKey{
			Namespace: stage.Namespace,
			Name:      stage.Name,
		}
		_, err := kargoapi.RefreshStage(ctx, r.kubeClient, objKey)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		logger.WithFields(log.Fields{
			"stageNamespace": stage.Namespace,
			"stage":          stage.Name,
		}).Debug("successfully patched Stage to force reconciliation")
	}
	if len(errs) > 0 {
		return result, errs[0]
	}

	return result, nil
}
