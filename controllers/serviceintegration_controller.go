// Copyright (c) 2021 Aiven, Helsinki, Finland. https://aiven.io/

package controllers

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strconv"
	"strings"

	"github.com/aiven/aiven-go-client"
	k8soperatorv1alpha1 "github.com/aiven/aiven-kubernetes-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ServiceIntegrationReconciler reconciles a ServiceIntegration object
type ServiceIntegrationReconciler struct {
	Controller
}

type ServiceIntegrationHandler struct {
	Handlers
	client *aiven.Client
}

// +kubebuilder:rbac:groups=aiven.io,resources=serviceintegrations,verbs=get;list;watch;createOrUpdate;update;patch;delete
// +kubebuilder:rbac:groups=aiven.io,resources=serviceintegrations/status,verbs=get;update;patch

func (r *ServiceIntegrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("serviceintegration", req.NamespacedName)
	log.Info("reconciling aiven service integration")

	si := &k8soperatorv1alpha1.ServiceIntegration{}
	err := r.Get(ctx, req.NamespacedName, si)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	c, err := r.InitAivenClient(ctx, req, si.Spec.AuthSecretRef)
	if err != nil {
		return ctrl.Result{}, err
	}

	return r.reconcileInstance(ctx, &ServiceIntegrationHandler{
		client: c,
	}, si)
}

func (r *ServiceIntegrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&k8soperatorv1alpha1.ServiceIntegration{}).
		Complete(r)
}

func (h ServiceIntegrationHandler) createOrUpdate(i client.Object) (client.Object, error) {
	si, err := h.convert(i)
	if err != nil {
		return nil, err
	}

	var integration *aiven.ServiceIntegration

	if si.Status.ID == "" {
		_, err = h.client.ServiceIntegrations.Create(
			si.Spec.Project,
			aiven.CreateServiceIntegrationRequest{
				DestinationEndpointID: toOptionalStringPointer(si.Spec.DestinationEndpointID),
				DestinationService:    toOptionalStringPointer(si.Spec.DestinationServiceName),
				IntegrationType:       si.Spec.IntegrationType,
				SourceEndpointID:      toOptionalStringPointer(si.Spec.SourceEndpointID),
				SourceService:         toOptionalStringPointer(si.Spec.SourceServiceName),
				UserConfig:            h.getUserConfig(si),
			},
		)
		if err != nil {
			return nil, fmt.Errorf("cannot createOrUpdate service integration: %w", err)
		}
	} else {
		integration, err = h.client.ServiceIntegrations.Update(
			si.Spec.Project,
			si.Status.ID,
			aiven.UpdateServiceIntegrationRequest{
				UserConfig: h.getUserConfig(si),
			},
		)
		if err != nil {
			if strings.Contains(err.Error(), "user config not changed") {
				return nil, nil
			}
			return nil, err
		}
	}

	si.Status.ID = integration.ServiceIntegrationID

	meta.SetStatusCondition(&si.Status.Conditions,
		getInitializedCondition("CreatedOrUpdate",
			"Instance was created or update on Aiven side"))

	meta.SetStatusCondition(&si.Status.Conditions,
		getRunningCondition(metav1.ConditionUnknown, "CreatedOrUpdate",
			"Instance was created or update on Aiven side, status remains unknown"))

	metav1.SetMetaDataAnnotation(&si.ObjectMeta,
		processedGeneration, strconv.FormatInt(si.GetGeneration(), 10))

	return si, nil
}

func (h ServiceIntegrationHandler) delete(i client.Object) (bool, error) {
	si, err := h.convert(i)
	if err != nil {
		return false, err
	}

	err = h.client.ServiceIntegrations.Delete(si.Spec.Project, si.Status.ID)
	if err != nil && !aiven.IsNotFound(err) {
		return false, fmt.Errorf("aiven client delete service ingtegration error: %w", err)
	}

	return true, nil
}

func (h ServiceIntegrationHandler) get(i client.Object) (client.Object, *corev1.Secret, error) {
	si, err := h.convert(i)
	if err != nil {
		return nil, nil, err
	}

	meta.SetStatusCondition(&si.Status.Conditions,
		getRunningCondition(metav1.ConditionTrue, "Get",
			"Instance is running on Aiven side"))

	metav1.SetMetaDataAnnotation(&si.ObjectMeta, isRunning, "1")

	return si, nil, nil
}

func (h ServiceIntegrationHandler) checkPreconditions(i client.Object) bool {
	si, err := h.convert(i)
	if err != nil {
		return false
	}

	return checkServiceIsRunning(h.client, si.Spec.Project, si.Spec.SourceServiceName) &&
		checkServiceIsRunning(h.client, si.Spec.Project, si.Spec.DestinationServiceName)
}

func (h ServiceIntegrationHandler) convert(i client.Object) (*k8soperatorv1alpha1.ServiceIntegration, error) {
	si, ok := i.(*k8soperatorv1alpha1.ServiceIntegration)
	if !ok {
		return nil, fmt.Errorf("cannot convert object to ServiceIntegration")
	}

	return si, nil
}

func (h ServiceIntegrationHandler) getUserConfig(int *k8soperatorv1alpha1.ServiceIntegration) map[string]interface{} {
	if int.Spec.IntegrationType == "datadog" {
		return UserConfigurationToAPI(int.Spec.DatadogUserConfig).(map[string]interface{})
	}
	if int.Spec.IntegrationType == "kafka_connect" {
		return UserConfigurationToAPI(int.Spec.KafkaConnectUserConfig).(map[string]interface{})
	}
	if int.Spec.IntegrationType == "kafka_logs" {
		return UserConfigurationToAPI(int.Spec.KafkaLogsUserConfig).(map[string]interface{})
	}
	if int.Spec.IntegrationType == "metrics" {
		return UserConfigurationToAPI(int.Spec.MetricsUserConfig).(map[string]interface{})
	}

	return nil
}
