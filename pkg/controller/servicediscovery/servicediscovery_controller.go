package servicediscovery

import (
	"context"
	goerrors "errors"
	"fmt"
	"strconv"
	"strings"

	submarinerv1alpha1 "github.com/submariner-io/submariner-operator/pkg/apis/submariner/v1alpha1"
	"github.com/submariner-io/submariner-operator/pkg/controller/helpers"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller_servicediscovery")

const (
	componentName         = "submariner-lighthouse"
	deploymentName        = "submariner-lighthouse-agent"
	lighthouseCoreDNSName = "submariner-lighthouse-coredns"
)

const (
	serviceDiscoveryImage  = "lighthouse-agent"
	lighthouseCoreDNSImage = "lighthouse-coredns"
)

// Add creates a new ServiceDiscovery Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	k8sclient, _ := clientset.NewForConfig(mgr.GetConfig())
	return &ReconcileServiceDiscovery{
		client:       mgr.GetClient(),
		scheme:       mgr.GetScheme(),
		k8sClientSet: k8sclient}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("servicediscovery-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource ServiceDiscovery
	err = c.Watch(&source.Kind{Type: &submarinerv1alpha1.ServiceDiscovery{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for changes to secondary resource Deployment and requeue the owner ServiceDiscovery
	err = c.Watch(&source.Kind{Type: &appsv1.Deployment{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &submarinerv1alpha1.ServiceDiscovery{},
	})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileServiceDiscovery implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileServiceDiscovery{}

// ReconcileServiceDiscovery reconciles a ServiceDiscovery object
type ReconcileServiceDiscovery struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client       client.Client
	scheme       *runtime.Scheme
	k8sClientSet *clientset.Clientset
}

// Reconcile reads that state of the cluster for a ServiceDiscovery object and makes changes based on the state read
// and what is in the ServiceDiscovery.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileServiceDiscovery) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling ServiceDiscovery")

	// Fetch the ServiceDiscovery instance
	instance := &submarinerv1alpha1.ServiceDiscovery{}
	err := r.client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			deployment := &appsv1.Deployment{}
			opts := []client.DeleteAllOfOption{
				client.InNamespace(request.NamespacedName.Namespace),
				client.MatchingLabels{"app": deploymentName},
			}
			err := r.client.DeleteAllOf(context.TODO(), deployment, opts...)
			return reconcile.Result{}, err
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}
	lightHouseAgent := newLighthouseAgent(instance)
	if _, err = helpers.ReconcileDeployment(instance, lightHouseAgent, reqLogger,
		r.client, r.scheme); err != nil {
		return reconcile.Result{}, err
	}

	ligthhouseConfigMap := newLigthhouseConfigMap(instance)
	if _, err = helpers.ReconcileConfigMap(instance, ligthhouseConfigMap, reqLogger,
		r.client, r.scheme); err != nil {
		log.Error(err, "Error creating the lighthouseCoreDNS configMap")
		return reconcile.Result{}, err
	}

	ligthhouseCoreDNSDeployment := newLigthhouseCoreDNSDeployment(instance)
	if _, err = helpers.ReconcileDeployment(instance, ligthhouseCoreDNSDeployment, reqLogger,
		r.client, r.scheme); err != nil {
		log.Error(err, "Error creating the lighthouseCoreDNS deployment")
		return reconcile.Result{}, err
	}

	ligthhouseCoreDNSService := &corev1.Service{}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: lighthouseCoreDNSName, Namespace: instance.Namespace}, ligthhouseCoreDNSService)
	if errors.IsNotFound(err) {
		ligthhouseCoreDNSService = newLigthhouseCoreDNSService(instance)
		if _, err = helpers.ReconcileService(instance, ligthhouseCoreDNSService, reqLogger,
			r.client, r.scheme); err != nil {
			log.Error(err, "Error creating the lighthouseCoreDNS service")
			return reconcile.Result{}, err
		}
	}
	err = updateDNSConfigMap(r.client, r.k8sClientSet, instance)
	if err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func newLighthouseAgent(cr *submarinerv1alpha1.ServiceDiscovery) *appsv1.Deployment {
	replicas := int32(1)
	labels := map[string]string{
		"app":       deploymentName,
		"component": componentName,
	}
	matchLabels := map[string]string{
		"app": deploymentName,
	}

	terminationGracePeriodSeconds := int64(0)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cr.Namespace,
			Name:      deploymentName,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: matchLabels,
			},
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "submariner-lighthouse-agent",
							Image:           getImagePath(cr, serviceDiscoveryImage),
							ImagePullPolicy: "IfNotPresent",
							Env: []corev1.EnvVar{
								{Name: "SUBMARINER_NAMESPACE", Value: cr.Spec.Namespace},
								{Name: "SUBMARINER_CLUSTERID", Value: cr.Spec.ClusterID},
								{Name: "SUBMARINER_EXCLUDENS", Value: "submariner,kube-system,operators"},
								{Name: "SUBMARINER_DEBUG", Value: strconv.FormatBool(cr.Spec.Debug)},
								{Name: "BROKER_K8S_APISERVER", Value: cr.Spec.BrokerK8sApiServer},
								{Name: "BROKER_K8S_APISERVERTOKEN", Value: cr.Spec.BrokerK8sApiServerToken},
								{Name: "BROKER_K8S_REMOTENAMESPACE", Value: cr.Spec.BrokerK8sRemoteNamespace},
								{Name: "BROKER_K8S_CA", Value: cr.Spec.BrokerK8sCA},
							},
						},
					},

					ServiceAccountName:            "submariner-lighthouse",
					TerminationGracePeriodSeconds: &terminationGracePeriodSeconds,
				},
			},
		},
	}
}

func newLigthhouseConfigMap(cr *submarinerv1alpha1.ServiceDiscovery) *corev1.ConfigMap {
	labels := map[string]string{
		"app":       lighthouseCoreDNSName,
		"component": componentName,
	}
	expectedCorefile := `supercluster.local:53 {
lighthouse
errors
health
ready
}
`
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lighthouseCoreDNSName,
			Namespace: cr.Namespace,
			Labels:    labels,
		},
		Data: map[string]string{
			"Corefile": expectedCorefile,
		},
	}
}

func newLigthhouseCoreDNSDeployment(cr *submarinerv1alpha1.ServiceDiscovery) *appsv1.Deployment {
	replicas := int32(2)
	labels := map[string]string{
		"app":       lighthouseCoreDNSName,
		"component": componentName,
	}
	matchLabels := map[string]string{
		"app": lighthouseCoreDNSName,
	}

	terminationGracePeriodSeconds := int64(0)
	defaultMode := int32(420)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cr.Namespace,
			Name:      lighthouseCoreDNSName,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: matchLabels,
			},
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            lighthouseCoreDNSName,
							Image:           getImagePath(cr, lighthouseCoreDNSImage),
							ImagePullPolicy: "IfNotPresent",
							Args: []string{
								"-conf",
								"/etc/coredns/Corefile",
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "config-volume", MountPath: "/etc/coredns", ReadOnly: true},
							},
						},
					},

					ServiceAccountName:            "submariner-lighthouse",
					TerminationGracePeriodSeconds: &terminationGracePeriodSeconds,
					Volumes: []corev1.Volume{
						{Name: "config-volume", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: lighthouseCoreDNSName},
							Items: []corev1.KeyToPath{
								{Key: "Corefile", Path: "Corefile"},
							},
							DefaultMode: &defaultMode,
						}}},
					},
				},
			},
		},
	}
}

func newLigthhouseCoreDNSService(cr *submarinerv1alpha1.ServiceDiscovery) *corev1.Service {
	labels := map[string]string{
		"app":       lighthouseCoreDNSName,
		"component": componentName,
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cr.Namespace,
			Name:      lighthouseCoreDNSName,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name:     "udp",
				Protocol: "UDP",
				Port:     53,
				TargetPort: intstr.IntOrString{Type: intstr.Int,
					IntVal: 53},
			}},
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				"app": lighthouseCoreDNSName,
			},
		},
	}
}

func updateDNSConfigMap(client client.Client, k8sclientSet *clientset.Clientset, cr *submarinerv1alpha1.ServiceDiscovery) error {
	configMaps := k8sclientSet.CoreV1().ConfigMaps("kube-system")
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		configMap, err := configMaps.Get("coredns", metav1.GetOptions{})
		if err != nil {
			return err
		}
		/* This entry will be added to config map
		# lighthouse
		supercluster.local:53 {
		    forward . 2.2.2.2:5353
		}
		*/
		corefile := configMap.Data["Corefile"]
		if strings.Contains(corefile, "lighthouse") {
			// Assume this means we've already set the ConfigMap up
			return nil
		}
		lighthouseDnsService := &corev1.Service{}
		err = client.Get(context.TODO(), types.NamespacedName{Name: lighthouseCoreDNSName, Namespace: cr.Namespace}, lighthouseDnsService)
		if err != nil || lighthouseDnsService.Spec.ClusterIP == "" {
			return goerrors.New("lighthouseDnsService ClusterIp should be available")
		}
		expectedCorefile := `#lighthouse
supercluster.local {
forward . `
		expectedCorefile = expectedCorefile + lighthouseDnsService.Spec.ClusterIP + "\n" + "}\n"
		coreFile := configMap.Data["Corefile"]
		if strings.Contains(coreFile, "supercluster") {
			// Assume this means we've already set the ConfigMap up
			return nil
		}
		coreFile = expectedCorefile + coreFile
		log.Info("Updated coredns CoreFile " + coreFile)
		configMap.Data["Corefile"] = coreFile
		// Potentially retried
		_, err = configMaps.Update(configMap)
		return err
	})
	return retryErr
}

func getImagePath(submariner *submarinerv1alpha1.ServiceDiscovery, componentImage string) string {
	var path string
	spec := submariner.Spec

	// If the repository is "local" we don't append it on the front of the image,
	// a local repository is used for development, testing and CI when we inject
	// images in the cluster, for example submariner:local, or submariner-route-agent:local
	if spec.Repository == "local" {
		path = componentImage
	} else {
		path = fmt.Sprintf("%s/%s", spec.Repository, componentImage)
	}

	path = fmt.Sprintf("%s:%s", path, spec.Version)
	return path
}