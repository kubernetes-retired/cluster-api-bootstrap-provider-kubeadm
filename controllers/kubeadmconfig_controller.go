/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	bootstrapv1 "sigs.k8s.io/cluster-api-bootstrap-provider-kubeadm/api/v1alpha2"
	"sigs.k8s.io/cluster-api-bootstrap-provider-kubeadm/certs"
	"sigs.k8s.io/cluster-api-bootstrap-provider-kubeadm/cloudinit"
	kubeadmv1beta1 "sigs.k8s.io/cluster-api-bootstrap-provider-kubeadm/kubeadm/v1beta1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha2"
	capierrors "sigs.k8s.io/cluster-api/errors"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ControlPlaneReadyAnnotationKey identifies when the infrastructure is ready for use such as joining new nodes.
	// TODO move this into cluster-api to be imported by providers
	ControlPlaneReadyAnnotationKey = "cluster.x-k8s.io/control-plane-ready"
)

// KubeadmConfigReconciler reconciles a KubeadmConfig object
type KubeadmConfigReconciler struct {
	client.Client
	SecretsClientFactory SecretsClientFactory
	KubeadmInitLock      InitLocker
	Log                  logr.Logger
}

// InitLocker is a lock that is used around kubeadm init
type InitLocker interface {
	Lock(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine) bool
	Unlock(ctx context.Context, cluster *clusterv1.Cluster) bool
}

// SecretsClientFactory define behaviour for creating a secrets client
type SecretsClientFactory interface {
	// NewSecretsClient returns a new client supporting SecretInterface
	NewSecretsClient(client.Client, *clusterv1.Cluster) (typedcorev1.SecretInterface, error)
}

// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=kubeadmconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=kubeadmconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;machines,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets;events;configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile TODO
func (r *KubeadmConfigReconciler) Reconcile(req ctrl.Request) (_ ctrl.Result, rerr error) {

	ctx := context.Background()
	log := r.Log.WithValues("kubeadmconfig", req.NamespacedName)

	config := &bootstrapv1.KubeadmConfig{}
	if err := r.Get(ctx, req.NamespacedName, config); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get config")
		return ctrl.Result{}, err
	}

	// bail super early if it's already ready
	if config.Status.Ready {
		log.Info("ignoring an already ready config")
		return ctrl.Result{}, nil
	}

	machine, err := util.GetOwnerMachine(ctx, r.Client, config.ObjectMeta)
	if err != nil {
		log.Error(err, "could not get owner machine")
		return ctrl.Result{}, err
	}
	if machine == nil {
		log.Info("Waiting for Machine Controller to set OwnerRef on the KubeadmConfig")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	log = log.WithValues("machine-name", machine.Name)

	// Ignore machines that already have bootstrap data
	if machine.Spec.Bootstrap.Data != nil {
		return ctrl.Result{}, nil
	}

	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		log.Error(err, "could not get cluster by machine metadata")
		return ctrl.Result{}, err
	}

	// Check for infrastructure ready. If it's not ready then we will requeue the machine until it is.
	// The cluster-api machine controller set this value.
	if !cluster.Status.InfrastructureReady {
		log.Info("Infrastructure is not ready, requeing until ready.")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Initialize the patch helper
	patchHelper, err := patch.NewHelper(config, r)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Always attempt to Patch the KubeadmConfig object and status after each reconciliation.
	defer func() {
		if err := patchHelper.Patch(ctx, config); err != nil {
			log.Error(err, "failed to patch config")
			if rerr == nil {
				rerr = err
			}
		}
	}()

	holdLock := false
	defer func() {
		if !holdLock {
			r.KubeadmInitLock.Unlock(ctx, cluster)
		}
	}()

	// Check for control plane ready. If it's not ready then we will requeue the machine until it is.
	// The cluster-api machine controller set this value.
	if cluster.Annotations[ControlPlaneReadyAnnotationKey] != "true" {
		// if it's NOT a control plane machine, requeue
		if !util.IsControlPlaneMachine(machine) {
			log.Info(fmt.Sprintf("Machine is not a control plane. If it should be a control plane, add `%s: true` as a label to the Machine", clusterv1.MachineControlPlaneLabelName))
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// if the machine has not ClusterConfiguration and InitConfiguration, requeue
		if config.Spec.InitConfiguration == nil && config.Spec.ClusterConfiguration == nil {
			log.Info("Control plane is not ready, requeing joining control planes until ready.")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// acquire the init lock so that only the first machine configured
		// as control plane get processed here
		// if not the first, requeue
		if !r.KubeadmInitLock.Lock(ctx, cluster, machine) {
			log.Info("A control plane is already being initialized, requeing until control plane is ready")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		log.Info("Creating BootstrapData for the init control plane")

		// get both of ClusterConfiguration and InitConfiguration strings to pass to the cloud init control plane generator
		// kubeadm allows one of these values to be empty; CABPK replace missing values with an empty config, so the cloud init generation
		// should not handle special cases.

		if config.Spec.InitConfiguration == nil {
			config.Spec.InitConfiguration = &kubeadmv1beta1.InitConfiguration{
				TypeMeta: v1.TypeMeta{
					APIVersion: "kubeadm.k8s.io/v1beta1",
					Kind:       "InitConfiguration",
				},
			}
		}
		initdata, err := kubeadmv1beta1.ConfigurationToYAML(config.Spec.InitConfiguration)
		if err != nil {
			log.Error(err, "failed to marshal init configuration")
			return ctrl.Result{}, err
		}

		if config.Spec.ClusterConfiguration == nil {
			config.Spec.ClusterConfiguration = &kubeadmv1beta1.ClusterConfiguration{
				TypeMeta: v1.TypeMeta{
					APIVersion: "kubeadm.k8s.io/v1beta1",
					Kind:       "ClusterConfiguration",
				},
			}
		}

		// injects into config.ClusterConfiguration values from top level object
		r.reconcileTopLevelObjectSettings(cluster, machine, config)

		clusterdata, err := kubeadmv1beta1.ConfigurationToYAML(config.Spec.ClusterConfiguration)
		if err != nil {
			log.Error(err, "failed to marshal cluster configuration")
			return ctrl.Result{}, err
		}

		certificates, err := r.getOrCreateClusterCertificates(ctx, cluster.GetName(), config)
		if err != nil {
			log.Error(err, "unable to lookup or create cluster certificates")
			return ctrl.Result{}, err
		}

		err = r.createKubeconfigSecret(ctx, config.Spec.ClusterConfiguration.ClusterName, config.Spec.ClusterConfiguration.ControlPlaneEndpoint, req.Namespace, certificates)
		if err != nil {
			log.Error(err, "unable to create and write kubeconfig as a Secret")
			return ctrl.Result{}, err
		}

		cloudInitData, err := cloudinit.NewInitControlPlane(&cloudinit.ControlPlaneInput{
			BaseUserData: cloudinit.BaseUserData{
				AdditionalFiles:     config.Spec.Files,
				NTP:                 config.Spec.NTP,
				PreKubeadmCommands:  config.Spec.PreKubeadmCommands,
				PostKubeadmCommands: config.Spec.PostKubeadmCommands,
				Users:               config.Spec.Users,
			},
			InitConfiguration:    string(initdata),
			ClusterConfiguration: string(clusterdata),
			Certificates:         *certificates,
		})
		if err != nil {
			log.Error(err, "failed to generate cloud init for bootstrap control plane")
			return ctrl.Result{}, err
		}

		config.Status.BootstrapData = cloudInitData
		config.Status.Ready = true

		holdLock = true

		return ctrl.Result{}, nil
	}

	// Every other case it's a join scenario
	// Nb. in this case ClusterConfiguration and JoinConfiguration should not be defined by users, but in case of misconfigurations, CABPK simply ignore them

	// Unlock any locks that might have been set during init process
	r.KubeadmInitLock.Unlock(ctx, cluster)

	if config.Spec.JoinConfiguration == nil {
		return ctrl.Result{}, errors.New("Control plane already exists for the cluster, only KubeadmConfig objects with JoinConfiguration are allowed")
	}

	// ensure that joinConfiguration.Discovery is properly set for joining node on the current cluster
	if err := r.reconcileDiscovery(cluster, config); err != nil {
		if requeueErr, ok := errors.Cause(err).(capierrors.HasRequeueAfterError); ok {
			log.Info(err.Error())
			return ctrl.Result{RequeueAfter: requeueErr.GetRequeueAfter()}, nil
		}
		return ctrl.Result{}, err
	}

	joinBytes, err := kubeadmv1beta1.ConfigurationToYAML(config.Spec.JoinConfiguration)
	if err != nil {
		log.Error(err, "failed to marshal join configuration")
		return ctrl.Result{}, err
	}

	// it's a control plane join
	if util.IsControlPlaneMachine(machine) {
		if config.Spec.JoinConfiguration.ControlPlane == nil {
			return ctrl.Result{}, errors.New("Machine is a ControlPlane, but JoinConfiguration.ControlPlane is not set in the KubeadmConfig object")
		}

		certificates, err := r.getOrCreateClusterCertificates(ctx, cluster.GetName(), config)
		if err != nil {
			log.Error(err, "unable to locate or create cluster certificates")
			return ctrl.Result{}, err
		}

		joinData, err := cloudinit.NewJoinControlPlane(&cloudinit.ControlPlaneJoinInput{
			JoinConfiguration: string(joinBytes),
			Certificates:      *certificates,
			BaseUserData: cloudinit.BaseUserData{
				AdditionalFiles:     config.Spec.Files,
				NTP:                 config.Spec.NTP,
				PreKubeadmCommands:  config.Spec.PreKubeadmCommands,
				PostKubeadmCommands: config.Spec.PostKubeadmCommands,
				Users:               config.Spec.Users,
			},
		})
		if err != nil {
			log.Error(err, "failed to create a control plane join configuration")
			return ctrl.Result{}, err
		}

		config.Status.BootstrapData = joinData
		config.Status.Ready = true
		return ctrl.Result{}, nil
	}

	// otherwise it is a node
	if config.Spec.JoinConfiguration.ControlPlane != nil {
		return ctrl.Result{}, errors.New("Machine is a Worker, but JoinConfiguration.ControlPlane is set in the KubeadmConfig object")
	}

	joinData, err := cloudinit.NewNode(&cloudinit.NodeInput{
		BaseUserData: cloudinit.BaseUserData{
			AdditionalFiles:     config.Spec.Files,
			NTP:                 config.Spec.NTP,
			PreKubeadmCommands:  config.Spec.PreKubeadmCommands,
			PostKubeadmCommands: config.Spec.PostKubeadmCommands,
			Users:               config.Spec.Users,
		},
		JoinConfiguration: string(joinBytes),
	})
	if err != nil {
		log.Error(err, "failed to create a worker join configuration")
		return ctrl.Result{}, err
	}
	config.Status.BootstrapData = joinData
	config.Status.Ready = true
	return ctrl.Result{}, nil
}

// SetupWithManager TODO
func (r *KubeadmConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&bootstrapv1.KubeadmConfig{}).
		Complete(r)
}

// reconcileDiscovery ensure that config.JoinConfiguration.Discovery is properly set for the joining node.
// The implementation func respect user provided discovery configurations, but in case some of them are missing, a valid BootstrapToken object
// is automatically injected into config.JoinConfiguration.Discovery.
// This allows to simplify configuration UX, by providing the option to delegate to CABPK the configuration of kubeadm join discovery.
func (r *KubeadmConfigReconciler) reconcileDiscovery(cluster *clusterv1.Cluster, config *bootstrapv1.KubeadmConfig) error {
	log := r.Log.WithValues("kubeadmconfig", fmt.Sprintf("%s/%s", config.Namespace, config.Name))

	// if config already contains a file discovery configuration, respect it without further validations
	if config.Spec.JoinConfiguration.Discovery.File != nil {
		return nil
	}

	// otherwise it is necessary to ensure token discovery is properly configured
	if config.Spec.JoinConfiguration.Discovery.BootstrapToken == nil {
		config.Spec.JoinConfiguration.Discovery.BootstrapToken = &kubeadmv1beta1.BootstrapTokenDiscovery{}
	}

	// if BootstrapToken already contains an APIServerEndpoint, respect it; otherwise inject the APIServerEndpoint endpoint defined in cluster status
	//TODO(fp) might be we want to validate user provided APIServerEndpoint and warn/error if it doesn't match the api endpoint defined at cluster level
	apiServerEndpoint := config.Spec.JoinConfiguration.Discovery.BootstrapToken.APIServerEndpoint
	if apiServerEndpoint == "" {
		if len(cluster.Status.APIEndpoints) == 0 {
			return errors.Wrap(&capierrors.RequeueAfterError{RequeueAfter: 10 * time.Second}, "Waiting for Cluster Controller to set cluster.Status.APIEndpoints")
		}

		// NB. CABPK only uses the first APIServerEndpoint defined in cluster status if there are multiple defined.
		apiServerEndpoint = fmt.Sprintf("%s:%d", cluster.Status.APIEndpoints[0].Host, cluster.Status.APIEndpoints[0].Port)
		config.Spec.JoinConfiguration.Discovery.BootstrapToken.APIServerEndpoint = apiServerEndpoint
		log.Info("Altering JoinConfiguration.Discovery.BootstrapToken", "APIServerEndpoint", apiServerEndpoint)
	}

	// if BootstrapToken already contains a token, respect it; otherwise create a new bootstrap token for the node to join
	if config.Spec.JoinConfiguration.Discovery.BootstrapToken.Token == "" {
		// gets the remote secret interface client for the current cluster
		secretsClient, err := r.SecretsClientFactory.NewSecretsClient(r.Client, cluster)
		if err != nil {
			return err
		}

		token, err := createToken(secretsClient)
		if err != nil {
			return errors.Wrapf(err, "failed to create new bootstrap token")
		}

		config.Spec.JoinConfiguration.Discovery.BootstrapToken.Token = token
		log.Info("Altering JoinConfiguration.Discovery.BootstrapToken", "Token", token)
	}

	// if BootstrapToken already contains a CACertHashes or UnsafeSkipCAVerification, respect it; otherwise set for UnsafeSkipCAVerification
	// TODO: set CACertHashes instead of UnsafeSkipCAVerification
	if len(config.Spec.JoinConfiguration.Discovery.BootstrapToken.CACertHashes) == 0 && !config.Spec.JoinConfiguration.Discovery.BootstrapToken.UnsafeSkipCAVerification {
		config.Spec.JoinConfiguration.Discovery.BootstrapToken.UnsafeSkipCAVerification = true
		log.Info("Altering JoinConfiguration.Discovery.BootstrapToken", "UnsafeSkipCAVerification", true)
	}

	return nil
}

// reconcileTopLevelObjectSettings injects into config.ClusterConfiguration values from top level objects like cluster and machine.
// The implementation func respect user provided config values, but in case some of them are missing, values from top level objects are used.
func (r *KubeadmConfigReconciler) reconcileTopLevelObjectSettings(cluster *clusterv1.Cluster, machine *clusterv1.Machine, config *bootstrapv1.KubeadmConfig) {
	log := r.Log.WithValues("kubeadmconfig", fmt.Sprintf("%s/%s", config.Namespace, config.Name))

	// If there are no ControlPlaneEndpoint defined in ClusterConfiguration but there are APIEndpoints defined at cluster level (e.g. the load balancer endpoint),
	// then use cluster APIEndpoints as a control plane endpoint for the K8s cluster
	if config.Spec.ClusterConfiguration.ControlPlaneEndpoint == "" && len(cluster.Status.APIEndpoints) > 0 {
		// NB. CABPK only uses the first APIServerEndpoint defined in cluster status if there are multiple defined.
		config.Spec.ClusterConfiguration.ControlPlaneEndpoint = fmt.Sprintf("%s:%d", cluster.Status.APIEndpoints[0].Host, cluster.Status.APIEndpoints[0].Port)
		log.Info("Altering ClusterConfiguration", "ControlPlaneEndpoint", config.Spec.ClusterConfiguration.ControlPlaneEndpoint)
	}

	// If there are no ClusterName defined in ClusterConfiguration, use Cluster.Name
	if config.Spec.ClusterConfiguration.ClusterName == "" {
		config.Spec.ClusterConfiguration.ClusterName = cluster.Name
		log.Info("Altering ClusterConfiguration", "ClusterName", config.Spec.ClusterConfiguration.ClusterName)
	}

	// If there are no Network settings defined in ClusterConfiguration, use ClusterNetwork settings, if defined
	if cluster.Spec.ClusterNetwork != nil {
		if config.Spec.ClusterConfiguration.Networking.DNSDomain == "" && cluster.Spec.ClusterNetwork.ServiceDomain != "" {
			config.Spec.ClusterConfiguration.Networking.DNSDomain = cluster.Spec.ClusterNetwork.ServiceDomain
			log.Info("Altering ClusterConfiguration", "DNSDomain", config.Spec.ClusterConfiguration.Networking.DNSDomain)
		}
		if config.Spec.ClusterConfiguration.Networking.ServiceSubnet == "" && len(cluster.Spec.ClusterNetwork.Services.CIDRBlocks) > 0 {
			config.Spec.ClusterConfiguration.Networking.ServiceSubnet = strings.Join(cluster.Spec.ClusterNetwork.Services.CIDRBlocks, "")
			log.Info("Altering ClusterConfiguration", "ServiceSubnet", config.Spec.ClusterConfiguration.Networking.ServiceSubnet)
		}
		if config.Spec.ClusterConfiguration.Networking.PodSubnet == "" && len(cluster.Spec.ClusterNetwork.Pods.CIDRBlocks) > 0 {
			config.Spec.ClusterConfiguration.Networking.PodSubnet = strings.Join(cluster.Spec.ClusterNetwork.Pods.CIDRBlocks, "")
			log.Info("Altering ClusterConfiguration", "PodSubnet", config.Spec.ClusterConfiguration.Networking.PodSubnet)
		}
	}

	// If there are no KubernetesVersion settings defined in ClusterConfiguration, use Version from machine, if defined
	if config.Spec.ClusterConfiguration.KubernetesVersion == "" && machine.Spec.Version != nil {
		config.Spec.ClusterConfiguration.KubernetesVersion = *machine.Spec.Version
		log.Info("Altering ClusterConfiguration", "KubernetesVersion", config.Spec.ClusterConfiguration.KubernetesVersion)
	}
}

func (r *KubeadmConfigReconciler) getOrCreateClusterCertificates(ctx context.Context, clusterName string, config *bootstrapv1.KubeadmConfig) (*certs.Certificates, error) {
	certificates, err := r.getClusterCertificates(ctx, clusterName, config.GetNamespace())
	if err != nil {
		r.Log.Error(err, "unable to lookup cluster certificates")
		return nil, err
	}
	if certificates == nil {
		certificates, err = r.createClusterCertificates(ctx, clusterName, config)
		if err != nil {
			r.Log.Error(err, "unable to create cluster certificates")
			return nil, err
		}
	}
	return certificates, nil
}

func (r *KubeadmConfigReconciler) getClusterCertificates(ctx context.Context, clusterName, namespace string) (*certs.Certificates, error) {
	secrets := &corev1.SecretList{}

	err := r.Client.List(ctx, secrets, client.MatchingLabels{clusterv1.MachineClusterLabelName: clusterName})
	if err != nil {
		return nil, err
	}

	// TODO(moshloop) define the contract on what certificates can be created, some or all
	if len(secrets.Items) < 4 {
		return nil, nil
	}
	return certs.NewCertificatesFromSecrets(secrets)
}

func (r *KubeadmConfigReconciler) createClusterCertificates(ctx context.Context, clusterName string, config *bootstrapv1.KubeadmConfig) (*certs.Certificates, error) {
	certificates, err := certs.NewCertificates()
	if err != nil {
		return nil, err
	}

	for _, secret := range certs.NewSecretsFromCertificates(certificates) {
		secret.ObjectMeta.Namespace = config.GetNamespace()
		secret.ObjectMeta.OwnerReferences = createOwnerReferences(config)
		secret.ObjectMeta.Labels[clusterv1.MachineClusterLabelName] = clusterName
		secret.ObjectMeta.Name = prefixByClusterName(clusterName, secret.ObjectMeta.Name)
		r.Log.Info("Creating secret for certificate", "name", secret.ObjectMeta.Name)
		if err := r.Create(ctx, secret); err != nil {
			return nil, err
		}
	}
	return certificates, nil
}

func createOwnerReferences(config *bootstrapv1.KubeadmConfig) []v1.OwnerReference {
	return []v1.OwnerReference{
		{
			APIVersion: bootstrapv1.GroupVersion.String(),
			Kind:       "KubeadmConfig",
			Name:       config.GetName(),
			UID:        config.GetUID(),
		},
	}
}

func prefixByClusterName(clusterName, name string) string {
	return fmt.Sprintf("%s-%s", clusterName, name)
}

func (r *KubeadmConfigReconciler) createKubeconfigSecret(ctx context.Context, clusterName, endpoint, namespace string, certificates *certs.Certificates) error {
	if certificates.ClusterCA == nil {
		return errors.New("ClusterCA has not been created yet")
	}
	cert, err := certs.DecodeCertPEM(certificates.ClusterCA.Cert)
	if err != nil {
		return errors.Wrap(err, "failed to decode CA Cert")
	} else if cert == nil {
		return errors.New("certificate not found in config")
	}

	key, err := certs.DecodePrivateKeyPEM(certificates.ClusterCA.Key)
	if err != nil {
		return errors.Wrap(err, "failed to decode private key")
	} else if key == nil {
		return errors.New("CA private key not found")
	}

	server := fmt.Sprintf("https://%s", endpoint)
	cfg, err := certs.NewKubeconfig(clusterName, server, cert, key)
	if err != nil {
		return errors.Wrap(err, "failed to generate a kubeconfig")
	}

	yaml, err := clientcmd.Write(*cfg)
	if err != nil {
		return errors.Wrap(err, "failed to serialize config to yaml")
	}

	secret := &corev1.Secret{}
	secretName := fmt.Sprintf("%s-kubeconfig", clusterName)

	secret.ObjectMeta.Name = secretName
	secret.ObjectMeta.Namespace = namespace
	secret.StringData = map[string]string{"value": string(yaml)}

	return r.Create(ctx, secret)
}
