package config

import (
	"context"

	configsv1alpha1 "github.com/openshift/static-config-operator/pkg/apis/configs/v1alpha1"
	"github.com/openshift/static-config-operator/pkg/sync"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller_config")

const (
	staticConfigCRName      = "staticconfig"
	staticConfigCRNamespace = "openshift-static-config-operator"
)

// Add creates a new Config Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileConfig{
		client: mgr.GetClient(),
		scheme: mgr.GetScheme(),
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("config-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Config
	err = c.Watch(&source.Kind{Type: &configsv1alpha1.Config{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// TODO(user): Modify this to be the types you create that are owned by the primary resource
	// Watch for changes to secondary resource Pods and requeue the owner Config
	err = c.Watch(&source.Kind{Type: &corev1.Pod{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &configsv1alpha1.Config{},
	})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileConfig implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileConfig{}

// ReconcileConfig reconciles a Config object
type ReconcileConfig struct {
	client client.Client
	scheme *runtime.Scheme

	sync sync.Interface
}

// Reconcile reads that state of the cluster for a Config object and makes changes based on the state read
// and what is in the Config.Spec
func (r *ReconcileConfig) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	log.Info("Reconciling Config")
	instance := &configsv1alpha1.Config{}
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: staticConfigCRName, Namespace: staticConfigCRNamespace}, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}
	config := instance.DeepCopy()

	log.Info("Reconciling Config")

	// If this is first run, we iniciate sync and set it
	// We need config for it

	if r.sync == nil {
		log.Info("sync store is empty - initialize")
		sync, err := sync.New(config, r.client, r.scheme)
		if err != nil {
			return reconcile.Result{}, err
		}
		r.sync = sync
	}

	return r.sync.Sync(config)
}
