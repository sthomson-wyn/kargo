package project

import (
	"context"
	"fmt"

	"github.com/kelseyhightower/envconfig"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	kargoapi "github.com/akuity/kargo/api/v1alpha1"
	"github.com/akuity/kargo/internal/logging"
)

var (
	projectGroupKind = schema.GroupKind{
		Group: kargoapi.GroupVersion.Group,
		Kind:  "Project",
	}
	projectGroupResource = schema.GroupResource{
		Group:    kargoapi.GroupVersion.Group,
		Resource: "projects",
	}
)

type WebhookConfig struct {
	KargoNamespace string `envconfig:"KARGO_NAMESPACE" required:"true"`
}

func WebhookConfigFromEnv() WebhookConfig {
	cfg := WebhookConfig{}
	envconfig.MustProcess("", &cfg)
	return cfg
}

type webhook struct {
	cfg WebhookConfig

	// The following behaviors are overridable for testing purposes:

	validateSpecFn func(*field.Path, *kargoapi.ProjectSpec) field.ErrorList

	ensureNamespaceFn func(context.Context, *kargoapi.Project) error

	ensureSecretPermissionsFn func(
		context.Context,
		*kargoapi.Project,
	) error

	getNamespaceFn func(
		context.Context,
		types.NamespacedName,
		client.Object,
		...client.GetOption,
	) error

	createNamespaceFn func(
		context.Context,
		client.Object,
		...client.CreateOption,
	) error

	createRoleBindingFn func(
		context.Context,
		client.Object,
		...client.CreateOption,
	) error
}

func SetupWebhookWithManager(mgr ctrl.Manager, cfg WebhookConfig) error {
	w := newWebhook(mgr.GetClient(), cfg)
	return ctrl.NewWebhookManagedBy(mgr).
		For(&kargoapi.Project{}).
		WithValidator(w).
		Complete()
}

func newWebhook(kubeClient client.Client, cfg WebhookConfig) *webhook {
	w := &webhook{
		cfg: cfg,
	}
	w.validateSpecFn = w.validateSpec
	w.ensureNamespaceFn = w.ensureNamespace
	w.ensureSecretPermissionsFn = w.ensureSecretPermissions
	w.getNamespaceFn = kubeClient.Get
	w.createNamespaceFn = kubeClient.Create
	w.createRoleBindingFn = kubeClient.Create
	return w
}

func (w *webhook) ValidateCreate(
	ctx context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	project := obj.(*kargoapi.Project) // nolint: forcetypeassert

	if errs := w.validateSpecFn(field.NewPath("spec"), project.Spec); len(errs) > 0 {
		return nil, apierrors.NewInvalid(projectGroupKind, project.Name, errs)
	}

	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return nil, apierrors.NewInternalError(
			errors.Wrap(err, "error getting admission request from context"),
		)
	}

	if req.DryRun != nil && *req.DryRun {
		return nil, nil
	}

	// We synchronously ensure the existence of a namespace with the same name as
	// the Project because resources following the Project in a manifest are
	// likely to be scoped to that namespace.
	if err := w.ensureNamespaceFn(ctx, project); err != nil {
		return nil, err
	}

	// Ensure the Kargo API server receives permissions to manage secrets in the
	// Project namespace just in time. This prevents us from having to give the
	// Kargo API server carte blanche access to all secrets in the cluster. We do
	// this synchronously because Secrets are likely to follow the Project in a
	// manifest.
	return nil, w.ensureSecretPermissionsFn(ctx, project)
}

func (w *webhook) ValidateUpdate(
	_ context.Context,
	_ runtime.Object,
	newObj runtime.Object,
) (admission.Warnings, error) {
	project := newObj.(*kargoapi.Project) // nolint: forcetypeassert
	if errs := w.validateSpecFn(field.NewPath("spec"), project.Spec); len(errs) > 0 {
		return nil, apierrors.NewInvalid(projectGroupKind, project.Name, errs)
	}
	return nil, nil
}

func (w *webhook) ValidateDelete(
	context.Context,
	runtime.Object,
) (admission.Warnings, error) {
	return nil, nil
}

func (w *webhook) validateSpec(
	f *field.Path,
	spec *kargoapi.ProjectSpec,
) field.ErrorList {
	if spec == nil { // nil spec is valid
		return nil
	}
	return w.validatePromotionPolicies(
		f.Child("promotionPolicies"),
		spec.PromotionPolicies,
	)
}

func (w *webhook) validatePromotionPolicies(
	f *field.Path,
	promotionPolicies []kargoapi.PromotionPolicy,
) field.ErrorList {
	stageNames := make(map[string]struct{}, len(promotionPolicies))
	for _, promotionPolicy := range promotionPolicies {
		if _, found := stageNames[promotionPolicy.Stage]; found {
			return field.ErrorList{
				field.Invalid(
					f,
					promotionPolicies,
					fmt.Sprintf(
						"multiple %s reference stage %s",
						f.String(),
						promotionPolicy.Stage,
					),
				),
			}
		}
		stageNames[promotionPolicy.Stage] = struct{}{}
	}
	return nil
}

// ensureNamespace is used to ensure the existence of a namespace with the same
// name as the Project. If the namespace does not exist, it is created. If the
// namespace exists, it is checked for any ownership conflicts with the Project
// and will return an error if any are found.
func (w *webhook) ensureNamespace(
	ctx context.Context,
	project *kargoapi.Project,
) error {
	logger := logging.LoggerFromContext(ctx).WithFields(log.Fields{
		"project": project.Name,
		"name":    project.Name,
	})

	ns := &corev1.Namespace{}
	err := w.getNamespaceFn(
		ctx,
		types.NamespacedName{Name: project.Name},
		ns,
	)
	if err == nil {
		// We found an existing namespace with the same name as the Project. Check
		// for possible conflicts before proceeding.
		//
		// No owner, but not a Project namespace:
		if (len(ns.OwnerReferences) == 0 &&
			(ns.Labels == nil || ns.Labels[kargoapi.ProjectLabelKey] != kargoapi.LabelTrueValue)) ||
			// Not owned by this Project:
			(len(ns.OwnerReferences) == 1 && ns.OwnerReferences[0].UID != project.UID) ||
			// Multiple owners:
			len(ns.OwnerReferences) > 1 {
			return apierrors.NewConflict(
				projectGroupResource,
				project.Name,
				errors.Errorf(
					"failed to initialize Project %q because namespace %q already exists",
					project.Name,
					project.Name,
				),
			)
		}
		logger.Debug("namespace exists but no conflict was found")
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return apierrors.NewInternalError(
			errors.Wrapf(err, "error getting namespace %q", project.Name),
		)
	}

	logger.Debug("namespace does not exist; creating it")

	ns = &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: project.Name,
			Labels: map[string]string{
				kargoapi.ProjectLabelKey: kargoapi.LabelTrueValue,
			},
			// Note: We no longer use an owner reference here. If we did, and too
			// much time were to pass between namespace creation and the completion of
			// this webhook, Kubernetes would notice the absence of the owner, mistake
			// the namespace for an orphan, and delete it. We do still want the
			// Project to own the namespace, but we rely on the Project reconciler in
			// the management controller to establish that relationship
			// asynchronously.
		},
	}
	// Project namespaces are owned by a Project. Deleting a Project
	// automatically deletes the namespace. But we also want this to work in the
	// other direction, where that behavior is not automatic. We add a finalizer
	// to the namespace and use our own namespace reconciler to clear it after
	// deleting the Project.
	controllerutil.AddFinalizer(ns, kargoapi.FinalizerName)
	if err := w.createNamespaceFn(ctx, ns); err != nil {
		return apierrors.NewInternalError(
			errors.Wrapf(err, "error creating namespace %q", project.Name),
		)
	}
	logger.Debug("created namespace")

	return nil
}

func (w *webhook) ensureSecretPermissions(
	ctx context.Context,
	project *kargoapi.Project,
) error {
	const roleBindingName = "kargo-api-server-manage-project-secrets"

	logger := logging.LoggerFromContext(ctx).WithFields(log.Fields{
		"project":     project.Name,
		"name":        project.Name,
		"namespace":   project.Name,
		"roleBinding": roleBindingName,
	})

	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleBindingName,
			Namespace: project.Name,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     "kargo-secret-manager",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "kargo-api",
				Namespace: w.cfg.KargoNamespace,
			},
		},
	}
	if err := w.createRoleBindingFn(ctx, roleBinding); err != nil {
		if apierrors.IsAlreadyExists(err) {
			logger.Debug("role binding already exists in project namespace")
			return nil
		}
		return apierrors.NewInternalError(
			errors.Wrapf(
				err,
				"error creating role binding %q in project namespace %q",
				roleBinding.Name,
				project.Name,
			),
		)
	}
	logger.Debug("granted API server access to manage project secrets")

	return nil
}
