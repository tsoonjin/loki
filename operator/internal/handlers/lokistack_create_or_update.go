package handlers

import (
	"context"
	"fmt"
	"os"
	"time"

	lokiv1beta1 "github.com/grafana/loki/operator/apis/loki/v1beta1"
	"github.com/grafana/loki/operator/internal/external/k8s"
	"github.com/grafana/loki/operator/internal/handlers/internal/gateway"
	"github.com/grafana/loki/operator/internal/handlers/internal/rules"
	"github.com/grafana/loki/operator/internal/handlers/internal/storage"
	"github.com/grafana/loki/operator/internal/manifests"
	storageoptions "github.com/grafana/loki/operator/internal/manifests/storage"
	"github.com/grafana/loki/operator/internal/metrics"
	"github.com/grafana/loki/operator/internal/status"

	"github.com/ViaQ/logerr/v2/kverrors"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CreateOrUpdateLokiStack handles LokiStack create and update events.
func CreateOrUpdateLokiStack(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	k k8s.Client,
	s *runtime.Scheme,
	flags manifests.FeatureFlags,
) error {
	ll := log.WithValues("lokistack", req.NamespacedName, "event", "createOrUpdate")

	var stack lokiv1beta1.LokiStack
	if err := k.Get(ctx, req.NamespacedName, &stack); err != nil {
		if apierrors.IsNotFound(err) {
			// maybe the user deleted it before we could react? Either way this isn't an issue
			ll.Error(err, "could not find the requested loki stack", "name", req.NamespacedName)
			return nil
		}
		return kverrors.Wrap(err, "failed to lookup lokistack", "name", req.NamespacedName)
	}

	img := os.Getenv(manifests.EnvRelatedImageLoki)
	if img == "" {
		img = manifests.DefaultContainerImage
	}

	gwImg := os.Getenv(manifests.EnvRelatedImageGateway)
	if gwImg == "" {
		gwImg = manifests.DefaultLokiStackGatewayImage
	}

	var storageSecret corev1.Secret
	key := client.ObjectKey{Name: stack.Spec.Storage.Secret.Name, Namespace: stack.Namespace}
	if err := k.Get(ctx, key, &storageSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return &status.DegradedError{
				Message: "Missing object storage secret",
				Reason:  lokiv1beta1.ReasonMissingObjectStorageSecret,
				Requeue: false,
			}
		}
		return kverrors.Wrap(err, "failed to lookup lokistack storage secret", "name", key)
	}

	objStore, err := storage.ExtractSecret(&storageSecret, stack.Spec.Storage.Secret.Type)
	if err != nil {
		return &status.DegradedError{
			Message: fmt.Sprintf("Invalid object storage secret contents: %s", err),
			Reason:  lokiv1beta1.ReasonInvalidObjectStorageSecret,
			Requeue: false,
		}
	}

	storageSchemas, err := storageoptions.BuildSchemaConfig(
		time.Now().UTC(),
		stack.Spec.Storage,
		stack.Status.Storage,
	)
	if err != nil {
		return &status.DegradedError{
			Message: fmt.Sprintf("Invalid object storage schema contents: %s", err),
			Reason:  lokiv1beta1.ReasonInvalidObjectStorageSchema,
			Requeue: false,
		}
	}

	objStore.Schemas = storageSchemas

	if stack.Spec.Storage.TLS != nil {
		var cm corev1.ConfigMap
		key := client.ObjectKey{Name: stack.Spec.Storage.TLS.CA, Namespace: stack.Namespace}
		if err = k.Get(ctx, key, &cm); err != nil {
			if apierrors.IsNotFound(err) {
				return &status.DegradedError{
					Message: "Missing object storage CA config map",
					Reason:  lokiv1beta1.ReasonMissingObjectStorageCAConfigMap,
					Requeue: false,
				}
			}
			return kverrors.Wrap(err, "failed to lookup lokistack object storage CA config map", "name", key)
		}

		if !storage.IsValidCAConfigMap(&cm) {
			return &status.DegradedError{
				Message: "Invalid object storage CA configmap contents: missing key `service-ca.crt` or no contents",
				Reason:  lokiv1beta1.ReasonInvalidObjectStorageCAConfigMap,
				Requeue: false,
			}
		}

		objStore.TLS = &storageoptions.TLSConfig{CA: cm.Name}
	}

	var (
		baseDomain    string
		tenantSecrets []*manifests.TenantSecrets
		tenantConfigs map[string]manifests.TenantConfig
	)
	if flags.EnableGateway && stack.Spec.Tenants == nil {
		return &status.DegradedError{
			Message: "Invalid tenants configuration - TenantsSpec cannot be nil when gateway flag is enabled",
			Reason:  lokiv1beta1.ReasonInvalidTenantsConfiguration,
			Requeue: false,
		}
	} else if flags.EnableGateway && stack.Spec.Tenants != nil {
		if err = gateway.ValidateModes(stack); err != nil {
			return &status.DegradedError{
				Message: fmt.Sprintf("Invalid tenants configuration: %s", err),
				Reason:  lokiv1beta1.ReasonInvalidTenantsConfiguration,
				Requeue: false,
			}
		}

		if stack.Spec.Tenants.Mode != lokiv1beta1.OpenshiftLogging {
			tenantSecrets, err = gateway.GetTenantSecrets(ctx, k, req, &stack)
			if err != nil {
				return err
			}
		}

		if stack.Spec.Tenants.Mode == lokiv1beta1.OpenshiftLogging {
			baseDomain, err = gateway.GetOpenShiftBaseDomain(ctx, k, req)
			if err != nil {
				return err
			}
		}

		// extract the existing tenant's id, cookieSecret if exists, otherwise create new.
		tenantConfigs, err = gateway.GetTenantConfigMapData(ctx, k, req)
		if err != nil {
			ll.Error(err, "error in getting tenant config map data")
		}
	}

	var (
		alertingRules  []lokiv1beta1.AlertingRule
		recordingRules []lokiv1beta1.RecordingRule
		rulerConfig    *lokiv1beta1.RulerConfigSpec
		rulerSecret    *manifests.RulerSecret
	)
	if stack.Spec.Rules != nil && stack.Spec.Rules.Enabled {
		alertingRules, recordingRules, err = rules.List(ctx, k, req.Namespace, stack.Spec.Rules)
		if err != nil {
			log.Error(err, "failed to lookup rules", "spec", stack.Spec.Rules)
		}

		rulerConfig, err = rules.GetRulerConfig(ctx, k, req)
		if err != nil {
			log.Error(err, "failed to lookup ruler config", "key", req.NamespacedName)
		}

		if rulerConfig != nil && rulerConfig.RemoteWriteSpec != nil && rulerConfig.RemoteWriteSpec.ClientSpec != nil {
			var rs corev1.Secret
			key := client.ObjectKey{Name: rulerConfig.RemoteWriteSpec.ClientSpec.AuthorizationSecretName, Namespace: stack.Namespace}
			if err = k.Get(ctx, key, &rs); err != nil {
				if apierrors.IsNotFound(err) {
					return &status.DegradedError{
						Message: "Missing ruler remote write authorization secret",
						Reason:  lokiv1beta1.ReasonMissingRulerSecret,
						Requeue: false,
					}
				}
				return kverrors.Wrap(err, "failed to lookup lokistack ruler secret", "name", key)
			}

			rulerSecret, err = rules.ExtractRulerSecret(&rs, rulerConfig.RemoteWriteSpec.ClientSpec.AuthorizationType)
			if err != nil {
				return &status.DegradedError{
					Message: "Invalid ruler remote write authorization secret contents",
					Reason:  lokiv1beta1.ReasonInvalidRulerSecret,
					Requeue: false,
				}
			}
		}
	}

	// Here we will translate the lokiv1beta1.LokiStack options into manifest options
	opts := manifests.Options{
		Name:              req.Name,
		Namespace:         req.Namespace,
		Image:             img,
		GatewayImage:      gwImg,
		GatewayBaseDomain: baseDomain,
		Stack:             stack.Spec,
		Flags:             flags,
		ObjectStorage:     *objStore,
		AlertingRules:     alertingRules,
		RecordingRules:    recordingRules,
		Ruler: manifests.Ruler{
			Spec:   rulerConfig,
			Secret: rulerSecret,
		},
		Tenants: manifests.Tenants{
			Secrets: tenantSecrets,
			Configs: tenantConfigs,
		},
	}

	ll.Info("begin building manifests")

	if optErr := manifests.ApplyDefaultSettings(&opts); optErr != nil {
		ll.Error(optErr, "failed to conform options to build settings")
		return optErr
	}

	if flags.EnableGateway {
		if optErr := manifests.ApplyGatewayDefaultOptions(&opts); optErr != nil {
			ll.Error(optErr, "failed to apply defaults options to gateway settings ")
			return optErr
		}
	}

	objects, err := manifests.BuildAll(opts)
	if err != nil {
		ll.Error(err, "failed to build manifests")
		return err
	}

	ll.Info("manifests built", "count", len(objects))

	// The status is updated before the objects are actually created to
	// avoid the scenario in which the configmap is successfully created or
	// updated and another resource is not. This would cause the status to
	// be possibly misaligned with the configmap, which could lead to
	// a user possibly being unable to read logs.
	if err := status.SetStorageSchemaStatus(ctx, k, req, storageSchemas); err != nil {
		ll.Error(err, "failed to set storage schema status")
		return err
	}

	var errCount int32

	for _, obj := range objects {
		l := ll.WithValues(
			"object_name", obj.GetName(),
			"object_kind", obj.GetObjectKind(),
		)

		if isNamespaceScoped(obj) {
			obj.SetNamespace(req.Namespace)

			if err := ctrl.SetControllerReference(&stack, obj, s); err != nil {
				l.Error(err, "failed to set controller owner reference to resource")
				errCount++
				continue
			}
		}

		desired := obj.DeepCopyObject().(client.Object)
		mutateFn := manifests.MutateFuncFor(obj, desired)

		op, err := ctrl.CreateOrUpdate(ctx, k, obj, mutateFn)
		if err != nil {
			l.Error(err, "failed to configure resource")
			errCount++
			continue
		}

		l.Info(fmt.Sprintf("Resource has been %s", op))
	}

	if errCount > 0 {
		return kverrors.New("failed to configure lokistack resources", "name", req.NamespacedName)
	}

	// 1x.extra-small is used only for development, so the metrics will not
	// be collected.
	if opts.Stack.Size != lokiv1beta1.SizeOneXExtraSmall {
		metrics.Collect(&opts.Stack, opts.Name)
	}

	return nil
}

func isNamespaceScoped(obj client.Object) bool {
	switch obj.(type) {
	case *rbacv1.ClusterRole, *rbacv1.ClusterRoleBinding:
		return false
	default:
		return true
	}
}
