package controllers

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/aiven/aiven-go-client"
)

const (
	conditionTypeRunning     = "Running"
	conditionTypeInitialized = "Initialized"

	secretProtectionFinalizer = "finalizers.aiven.io/needed-to-delete-services"
	instanceDeletionFinalizer = "finalizers.aiven.io/delete-remote-resource"

	processedGenerationAnnotation = "controllers.aiven.io/generation-was-processed"
	instanceIsRunningAnnotation   = "controllers.aiven.io/instance-is-running"
)

var (
	operatorUserAgent          = "k8s-operator/" + aiven.Version()
	errTerminationProtectionOn = errors.New("termination protection is on")
)

func checkServiceIsRunning(c *aiven.Client, project, serviceName string) (bool, error) {
	s, err := c.Services.Get(project, serviceName)
	if err != nil {
		// if service is not found, it is not running
		if aiven.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return s.State == "RUNNING", nil
}

func getInitializedCondition(reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:    conditionTypeInitialized,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: message,
	}
}

func getRunningCondition(status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:    conditionTypeRunning,
		Status:  status,
		Reason:  reason,
		Message: message,
	}
}

func isMarkedForDeletion(o client.Object) bool {
	return !o.GetDeletionTimestamp().IsZero()
}

func addFinalizer(ctx context.Context, client client.Client, o client.Object, f string) error {
	controllerutil.AddFinalizer(o, f)
	return client.Update(ctx, o)
}

func removeFinalizer(ctx context.Context, client client.Client, o client.Object, f string) error {
	controllerutil.RemoveFinalizer(o, f)
	return client.Update(ctx, o)
}

func isAlreadyProcessed(o client.Object) bool {
	return o.GetAnnotations()[processedGenerationAnnotation] == strconv.FormatInt(o.GetGeneration(), formatIntBaseDecimal)
}

// IsAlreadyRunning returns true if object is ready to use
func IsAlreadyRunning(o client.Object) bool {
	_, found := o.GetAnnotations()[instanceIsRunningAnnotation]
	return found
}

func optionalStringPointer(u string) *string {
	if len(u) == 0 {
		return nil
	}

	return &u
}

func isAivenServerError(err error) bool {
	e, ok := err.(aiven.Error)
	return ok && e.Status >= http.StatusInternalServerError
}
