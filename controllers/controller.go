package controllers

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/hashicorp/go-multierror"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/aiven/aiven-go-client"
	"github.com/aiven/aiven-operator/api/v1alpha1"
)

// formatIntBaseDecimal it is a base to format int64 to string
const formatIntBaseDecimal = 10

// requeueTimeout sets timeout to requeue controller
const requeueTimeout = 10 * time.Second

type (
	// Controller reconciles the Aiven objects
	Controller struct {
		client.Client

		Log      logr.Logger
		Scheme   *runtime.Scheme
		Recorder record.EventRecorder
	}

	// Handlers represents Aiven API handlers
	// It intended to be a layer between Kubernetes and Aiven API that handles all aspects
	// of the Aiven services lifecycle.
	Handlers interface {
		// create or updates an instance on the Aiven side.
		createOrUpdate(*aiven.Client, client.Object) error

		// delete removes an instance on Aiven side.
		// If an object is already deleted and cannot be found, it should not be an error. For other deletion
		// errors, return an error.
		delete(*aiven.Client, client.Object) (bool, error)

		// get retrieve an object and a secret (for example, connection credentials) that is generated on the
		// fly based on data from Aiven API.  When not applicable to service, it should return nil.
		get(*aiven.Client, client.Object) (*corev1.Secret, error)

		// checkPreconditions check whether all preconditions for creating (or updating) the resource are in place.
		// For example, it is applicable when a service needs to be running before this resource can be created.
		checkPreconditions(*aiven.Client, client.Object) (bool, error)
	}

	aivenManagedObject interface {
		client.Object

		AuthSecretRef() v1alpha1.AuthSecretReference
	}
)

const (
	// Lifecycle event types we expose to the user
	eventUnableToGetAuthSecret              = "UnableToGetAuthSecret"
	eventUnableToCreateClient               = "UnableToCreateClient"
	eventReconciliationStarted              = "ReconcilationStarted"
	eventTryingToDeleteAtAiven              = "TryingToDeleteAtAiven"
	eventUnableToDeleteAtAiven              = "UnableToDeleteAtAiven"
	eventUnableToDeleteFinalizer            = "UnableToDeleteFinalizer"
	eventSuccessfullyDeletedAtAiven         = "SuccessfullyDeletedAtAiven"
	eventAddedFinalizer                     = "InstanceFinalizerAdded"
	eventUnableToAddFinalizer               = "UnableToAddFinalizer"
	eventWaitingforPreconditions            = "WaitingForPreconditions"
	eventUnableToWaitForPreconditions       = "UnableToWaitForPreconditions"
	eventPreconditionsAreMet                = "PreconditionsAreMet"
	eventUnableToCreateOrUpdateAtAiven      = "UnableToCreateOrUpdateAtAiven"
	eventCreateOrUpdatedAtAiven             = "CreateOrUpdatedAtAiven"
	eventCreatedOrUpdatedAtAiven            = "CreatedOrUpdatedAtAiven"
	eventWaitingForTheInstanceToBeRunning   = "WaitingForInstanceToBeRunning"
	eventUnableToWaitForInstanceToBeRunning = "UnableToWaitForInstanceToBeRunning"
	eventInstanceIsRunning                  = "InstanceIsRunning"
)

// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;create;update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (c *Controller) reconcileInstance(ctx context.Context, req ctrl.Request, h Handlers, o aivenManagedObject) (ctrl.Result, error) {
	if err := c.Get(ctx, req.NamespacedName, o); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	instanceLogger := setupLogger(c.Log, o)
	instanceLogger.Info("setting up aiven client with instance secret")

	clientAuthSecret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: o.AuthSecretRef().Name, Namespace: req.Namespace}, clientAuthSecret); err != nil {
		c.Recorder.Eventf(o, corev1.EventTypeWarning, eventUnableToGetAuthSecret, err.Error())
		return ctrl.Result{}, fmt.Errorf("cannot get secret %q: %w", o.AuthSecretRef().Name, err)
	}
	avn, err := aiven.NewTokenClient(string(clientAuthSecret.Data[o.AuthSecretRef().Key]), "k8s-operator/")
	if err != nil {
		c.Recorder.Event(o, corev1.EventTypeWarning, eventUnableToCreateClient, err.Error())
		return ctrl.Result{}, fmt.Errorf("cannot initialize aiven client: %w", err)
	}

	return instanceReconcilerHelper{
		avn: avn,
		k8s: c.Client,
		h:   h,
		log: instanceLogger,
		s:   clientAuthSecret,
		rec: c.Recorder,
	}.reconcileInstance(ctx, o)
}

// a helper that closes over all instance specific fields
// to make reconciliation a little more ergonomic
type instanceReconcilerHelper struct {
	k8s client.Client

	// avn, Aiven client that is authorized with the instance token
	avn *aiven.Client

	// h, instance specific handler implementation
	h Handlers

	// s, secret that contains the aiven token for the instance
	s *corev1.Secret

	// log, logger setup with structured fields for the instance
	log logr.Logger

	// rec, recorder to record events for the object
	rec record.EventRecorder
}

func (i instanceReconcilerHelper) reconcileInstance(ctx context.Context, o client.Object) (ctrl.Result, error) {
	i.log.Info("reconciling instance")
	i.rec.Event(o, corev1.EventTypeNormal, eventReconciliationStarted, "starting reconciliation")

	if isMarkedForDeletion(o) {
		if controllerutil.ContainsFinalizer(o, instanceDeletionFinalizer) {
			return i.finalize(ctx, o)
		}
		return ctrl.Result{}, nil
	}

	// Add finalizers to an instance and associated secret, only if they haven't
	// been added in the previous reconciliation loops
	if !controllerutil.ContainsFinalizer(i.s, secretProtectionFinalizer) {
		i.log.Info("adding finalizer to secret")
		if err := addFinalizer(ctx, i.k8s, i.s, secretProtectionFinalizer); err != nil {
			return ctrl.Result{}, fmt.Errorf("unable to add finalizer to secret: %w", err)
		}
	}
	if !controllerutil.ContainsFinalizer(o, instanceDeletionFinalizer) {
		i.log.Info("adding finalizer to instance")
		if err := addFinalizer(ctx, i.k8s, o, instanceDeletionFinalizer); err != nil {
			i.rec.Eventf(o, corev1.EventTypeWarning, eventUnableToAddFinalizer, err.Error())
			return ctrl.Result{}, fmt.Errorf("unable to add finalizer to instance: %w", err)
		}
		i.rec.Event(o, corev1.EventTypeNormal, eventAddedFinalizer, "instance finalizer added")
	}

	// check instance preconditions, if not met - requeue
	i.log.Info("handling service update/creation")
	if requeue, result, err := i.checkPreconditions(o); requeue {
		return result, err
	}

	if !isAlreadyProcessed(o) {
		i.rec.Event(o, corev1.EventTypeNormal, eventCreateOrUpdatedAtAiven, "about to create instance at aiven")
		if err := i.createOrUpdateInstance(o); err != nil {
			i.rec.Event(o, corev1.EventTypeWarning, eventUnableToCreateOrUpdateAtAiven, err.Error())
			return ctrl.Result{}, fmt.Errorf("unable to create or update instance at aiven: %w", err)
		}

		i.rec.Event(o, corev1.EventTypeNormal, eventCreatedOrUpdatedAtAiven, "instance was created at aiven but may not be running yet")
	}

	i.rec.Event(o, corev1.EventTypeNormal, eventWaitingForTheInstanceToBeRunning, "waiting for the instance to be running")
	isRunning, err := i.updateInstanceStateAndSecretUntilRunning(ctx, o)
	if err != nil {
		if aiven.IsNotFound(err) {
			return ctrl.Result{
				Requeue:      true,
				RequeueAfter: requeueTimeout,
			}, nil
		}

		i.rec.Event(o, corev1.EventTypeWarning, eventUnableToWaitForInstanceToBeRunning, err.Error())
		return ctrl.Result{}, fmt.Errorf("unable to wait until instance is running: %w", err)
	}

	if !isRunning {
		i.log.Info("instance is not yet running, triggering requeue")
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: requeueTimeout,
		}, nil
	}

	i.rec.Event(o, corev1.EventTypeNormal, eventInstanceIsRunning, "instance is in a RUNNING state")
	i.log.Info("instance was successfully reconciled")

	return ctrl.Result{}, nil
}

func (i instanceReconcilerHelper) checkPreconditions(o client.Object) (bool, ctrl.Result, error) {
	i.rec.Event(o, corev1.EventTypeNormal, eventWaitingforPreconditions, "waiting for preconditions of the instance")

	check, err := i.h.checkPreconditions(i.avn, o)
	if err != nil {
		i.rec.Event(o, corev1.EventTypeWarning, eventUnableToWaitForPreconditions, err.Error())
		return true, ctrl.Result{}, fmt.Errorf("unable to wait for preconditions: %w", err)
	}

	if !check {
		i.log.Info("preconditions are not met, requeue")
		return true, ctrl.Result{Requeue: true, RequeueAfter: requeueTimeout}, nil
	}

	i.rec.Event(o, corev1.EventTypeNormal, eventPreconditionsAreMet, "preconditions are met, proceeding to create or update")

	return false, ctrl.Result{}, nil
}

// finalize runs finalization logic. If the finalization logic fails, don't remove the finalizer so
// that we can retry during the next reconciliation. When applicable, it retrieves an associated object that
// has to be deleted from Kubernetes, and it could be a secret associated with an instance.
func (i instanceReconcilerHelper) finalize(ctx context.Context, o client.Object) (ctrl.Result, error) {
	i.rec.Event(o, corev1.EventTypeNormal, eventTryingToDeleteAtAiven, "trying to delete instance at aiven")

	finalised, err := i.h.delete(i.avn, o)
	if err != nil && !i.canBeDeleted(o, err) {
		i.rec.Event(o, corev1.EventTypeWarning, eventUnableToDeleteAtAiven, err.Error())
		return ctrl.Result{}, fmt.Errorf("unable to delete instance at aiven: %w", err)
	}

	// checking if instance was finalized, if not triggering a requeue
	if !finalised {
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: requeueTimeout,
		}, nil
	}

	i.log.Info("instance was successfully deleted at aiven, removing finalizer")
	i.rec.Event(o, corev1.EventTypeNormal, eventSuccessfullyDeletedAtAiven, "instance is gone at aiven now")

	// remove finalizer, once all finalizers have been removed, the object will be deleted.
	if err := removeFinalizer(ctx, i.k8s, o, instanceDeletionFinalizer); err != nil {
		i.rec.Event(o, corev1.EventTypeWarning, eventUnableToDeleteFinalizer, err.Error())
		return ctrl.Result{}, fmt.Errorf("unable to remove finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// canBeDeleted checks if an instance can be deleted despite error
func (i instanceReconcilerHelper) canBeDeleted(o client.Object, err error) bool {
	if err == nil {
		return true
	}

	// When an instance was created but pointing to an invalid API token
	// and no generation was ever processed, allow deleting such instance
	return !isAlreadyProcessed(o) && !isAlreadyRunning(o) &&
		strings.Contains(err.Error(), "Invalid token")
}

func (i instanceReconcilerHelper) createOrUpdateInstance(o client.Object) error {
	i.log.Info("generation wasn't processed, creation or updating instance on aiven side")
	a := o.GetAnnotations()
	delete(a, processedGenerationAnnotation)
	delete(a, instanceIsRunningAnnotation)

	if err := i.h.createOrUpdate(i.avn, o); err != nil {
		return fmt.Errorf("unable to create or update aiven instance: %w", err)
	}
	i.log.Info(
		"processed instance, updating annotations",
		"generation", o.GetGeneration(),
		"annotations", o.GetAnnotations(),
	)
	return nil
}

func (i instanceReconcilerHelper) updateInstanceStateAndSecretUntilRunning(ctx context.Context, o client.Object) (bool, error) {
	var err error

	i.log.Info("checking if instance is ready")

	defer func() {
		err = multierror.Append(err, i.k8s.Status().Update(ctx, o))
		err = multierror.Append(err, i.k8s.Update(ctx, o))
		err = err.(*multierror.Error).ErrorOrNil()
	}()

	serviceSecret, err := i.h.get(i.avn, o)
	if err != nil {
		return false, err
	} else if serviceSecret != nil {
		if err = i.createOrUpdateSecret(ctx, o, serviceSecret); err != nil {
			return false, fmt.Errorf("unable to create or update aiven secret: %w", err)
		}
	}
	return isAlreadyRunning(o), nil

}

func (i instanceReconcilerHelper) createOrUpdateSecret(ctx context.Context, owner client.Object, want *corev1.Secret) error {
	_, err := controllerutil.CreateOrUpdate(ctx, i.k8s, want, func() error {
		return ctrl.SetControllerReference(owner, want, i.k8s.Scheme())
	})
	return err
}

func setupLogger(log logr.Logger, o client.Object) logr.Logger {
	a := make(map[string]string)
	if r, ok := o.GetAnnotations()[instanceIsRunningAnnotation]; ok {
		a[instanceIsRunningAnnotation] = r
	}

	if g, ok := o.GetAnnotations()[processedGenerationAnnotation]; ok {
		a[processedGenerationAnnotation] = g
	}
	kind := strings.ToLower(o.GetObjectKind().GroupVersionKind().Kind)
	name := types.NamespacedName{Name: o.GetName(), Namespace: o.GetNamespace()}

	return log.WithValues("kind", kind, "name", name, "annotations", a)
}

// UserConfigurationToAPI converts UserConfiguration options structure
// to Aiven API compatible map[string]interface{}
func UserConfigurationToAPI(c interface{}) interface{} {
	result := make(map[string]interface{})

	v := reflect.ValueOf(c)

	// if its a pointer, resolve its value
	if v.Kind() == reflect.Ptr {
		v = reflect.Indirect(v)
	}

	if v.Kind() != reflect.Struct {
		switch v.Kind() {
		case reflect.Int64:
			return *c.(*int64)
		case reflect.Bool:
			return *c.(*bool)
		default:
			return c
		}
	}

	structType := v.Type()

	// convert UserConfig structure to a map
	for i := 0; i < structType.NumField(); i++ {
		name := strings.ReplaceAll(structType.Field(i).Tag.Get("json"), ",omitempty", "")

		if structType.Kind() == reflect.Struct {
			result[name] = UserConfigurationToAPI(v.Field(i).Interface())
		} else {
			result[name] = v.Elem().Field(i).Interface()
		}
	}

	// remove all the nil and empty map data
	for key, val := range result {
		if val == nil || isNil(val) || val == "" {
			delete(result, key)
		}

		if reflect.TypeOf(val).Kind() == reflect.Map {
			if len(val.(map[string]interface{})) == 0 {
				delete(result, key)
			}
		}
	}

	return result
}

func isNil(i interface{}) bool {
	if i == nil {
		return true
	}
	switch reflect.TypeOf(i).Kind() {
	case reflect.Ptr, reflect.Map, reflect.Array, reflect.Chan, reflect.Slice:
		return reflect.ValueOf(i).IsNil()
	}
	return false
}

func toOptionalStringPointer(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

func getMaintenanceWindow(dow, time string) *aiven.MaintenanceWindow {
	if dow != "" || time != "" {
		return &aiven.MaintenanceWindow{
			DayOfWeek: dow,
			TimeOfDay: time,
		}
	}

	return nil
}

func ensureSecretDataIsNotEmpty(log *logr.Logger, s *corev1.Secret) *corev1.Secret {
	if s == nil {
		return nil
	}

	for i, v := range s.StringData {
		if len(v) == 0 {
			if log != nil {
				log.Info("secret field is empty, deleting it from the secret",
					"field", v,
					"secret name", s.Name)
			}
			delete(s.StringData, i)
		}
	}

	return s
}
