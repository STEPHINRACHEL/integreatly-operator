package grafana

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	grafanav1alpha1 "github.com/integr8ly/grafana-operator/v3/pkg/apis/integreatly/v1alpha1"
	integreatlyv1alpha1 "github.com/integr8ly/integreatly-operator/pkg/apis/integreatly/v1alpha1"
	"github.com/integr8ly/integreatly-operator/pkg/config"
	"github.com/integr8ly/integreatly-operator/pkg/resources"
	"github.com/integr8ly/integreatly-operator/pkg/resources/backup"
	"github.com/integr8ly/integreatly-operator/pkg/resources/constants"
	"github.com/integr8ly/integreatly-operator/pkg/resources/events"
	"github.com/integr8ly/integreatly-operator/pkg/resources/marketplace"
	"github.com/integr8ly/integreatly-operator/pkg/resources/owner"
	"github.com/integr8ly/integreatly-operator/version"
	routev1 "github.com/openshift/api/route/v1"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	defaultInstallationNamespace = "customer-monitoring"
	manifestPackage              = "integreatly-grafana"
	defaultGrafanaName           = "grafana"
	defaultRoutename             = defaultGrafanaName + "-route"
)

type Reconciler struct {
	*resources.Reconciler
	ConfigManager config.ConfigReadWriter
	Config        *config.Grafana
	installation  *integreatlyv1alpha1.RHMI
	mpm           marketplace.MarketplaceInterface
	logger        *logrus.Entry
	extraParams   map[string]string
	recorder      record.EventRecorder
}

func (r *Reconciler) GetPreflightObject(ns string) runtime.Object {
	return nil
}

func (r *Reconciler) VerifyVersion(installation *integreatlyv1alpha1.RHMI) bool {
	return version.VerifyProductAndOperatorVersion(
		installation.Status.Stages[integreatlyv1alpha1.ProductsStage].Products[integreatlyv1alpha1.ProductGrafana],
		string(integreatlyv1alpha1.VersionGrafana),
		string(integreatlyv1alpha1.OperatorVersionGrafana),
	)
}

func NewReconciler(configManager config.ConfigReadWriter, installation *integreatlyv1alpha1.RHMI, mpm marketplace.MarketplaceInterface, recorder record.EventRecorder) (*Reconciler, error) {
	ns := installation.Spec.NamespacePrefix + defaultInstallationNamespace
	config, err := configManager.ReadGrafana()
	if err != nil {
		return nil, fmt.Errorf("could not retrieve threescale config: %w", err)
	}
	if config.GetNamespace() == "" {
		config.SetNamespace(ns)
		configManager.WriteConfig(config)
	}
	if config.GetOperatorNamespace() == "" {
		if installation.Spec.OperatorsInProductNamespace {
			config.SetOperatorNamespace(config.GetNamespace())
		} else {
			config.SetOperatorNamespace(config.GetNamespace() + "-operator")
		}
	}

	logger := logrus.NewEntry(logrus.StandardLogger())
	return &Reconciler{
		ConfigManager: configManager,
		Config:        config,
		installation:  installation,
		mpm:           mpm,
		logger:        logger,
		Reconciler:    resources.NewReconciler(mpm),
		recorder:      recorder,
	}, nil
}

func (r *Reconciler) Reconcile(ctx context.Context, installation *integreatlyv1alpha1.RHMI, product *integreatlyv1alpha1.RHMIProductStatus, client k8sclient.Client) (integreatlyv1alpha1.StatusPhase, error) {
	logrus.Infof("Start Grafana reconcile")

	operatorNamespace := r.Config.GetOperatorNamespace()
	productNamespace := r.Config.GetNamespace()

	phase, err := r.ReconcileFinalizer(ctx, client, installation, string(r.Config.GetProductName()), func() (integreatlyv1alpha1.StatusPhase, error) {
		phase, err := resources.RemoveNamespace(ctx, installation, client, productNamespace)
		if err != nil || phase != integreatlyv1alpha1.PhaseCompleted {
			return phase, err
		}

		phase, err = resources.RemoveNamespace(ctx, installation, client, operatorNamespace)
		if err != nil || phase != integreatlyv1alpha1.PhaseCompleted {
			return phase, err
		}

		return integreatlyv1alpha1.PhaseCompleted, nil
	})
	if err != nil || phase != integreatlyv1alpha1.PhaseCompleted {
		events.HandleError(r.recorder, installation, phase, "Failed to reconcile finalizer", err)
		return phase, err
	}

	phase, err = r.ReconcileNamespace(ctx, operatorNamespace, installation, client)
	if err != nil || phase != integreatlyv1alpha1.PhaseCompleted {
		events.HandleError(r.recorder, installation, phase, fmt.Sprintf("Failed to reconcile %s ns", operatorNamespace), err)
		return phase, err
	}

	phase, err = r.reconcileSecrets(ctx, client, installation, &grafanav1alpha1.Grafana{})
	if err != nil || phase != integreatlyv1alpha1.PhaseCompleted {
		events.HandleError(r.recorder, installation, phase, fmt.Sprintf("Failed to reconcile %s ns", productNamespace), err)
		return phase, err
	}

	phase, err = r.reconcileSubscription(ctx, client, installation, productNamespace, operatorNamespace)
	if err != nil || phase != integreatlyv1alpha1.PhaseCompleted {
		events.HandleError(r.recorder, installation, phase, fmt.Sprintf("Failed to reconcile %s subscription", constants.ThreeScaleSubscriptionName), err)
		return phase, err
	}

	phase, err = r.reconcileComponents(ctx, client, installation)
	if err != nil || phase != integreatlyv1alpha1.PhaseCompleted {
		events.HandleError(r.recorder, installation, phase, "Failed to create components", err)
		return phase, err
	}

	phase, err = r.reconcileHost(ctx, client)
	if err != nil || phase != integreatlyv1alpha1.PhaseCompleted {
		events.HandleError(r.recorder, installation, phase, "Failed to reconcile host", err)
		return phase, err
	}

	if string(r.Config.GetProductVersion()) != string(integreatlyv1alpha1.VersionGrafana) {
		r.Config.SetProductVersion(string(integreatlyv1alpha1.VersionGrafana))
		r.ConfigManager.WriteConfig(r.Config)
	}

	product.Host = r.Config.GetHost()
	product.Version = r.Config.GetProductVersion()
	product.OperatorVersion = r.Config.GetOperatorVersion()

	events.HandleProductComplete(r.recorder, installation, integreatlyv1alpha1.ProductsStage, r.Config.GetProductName())
	logrus.Infof("%s installation is reconciled successfully", r.Config.GetProductName())
	return integreatlyv1alpha1.PhaseCompleted, nil
}
func (r *Reconciler) reconcileSecrets(ctx context.Context, client k8sclient.Client, installation *integreatlyv1alpha1.RHMI, cr *grafanav1alpha1.Grafana) (integreatlyv1alpha1.StatusPhase, error) {
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "grafana-k8s-proxy",
			Namespace: r.Config.GetOperatorNamespace(),
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, client, secret, func() error {
		owner.AddIntegreatlyOwnerAnnotations(secret, installation)
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data["session_secret"] = []byte(populateSessionProxySecret())
		return nil
	})

	if err != nil {
		return integreatlyv1alpha1.PhaseFailed, err
	}

	return integreatlyv1alpha1.PhaseCompleted, nil
}

func (r *Reconciler) reconcileComponents(ctx context.Context, client k8sclient.Client, installation *integreatlyv1alpha1.RHMI) (integreatlyv1alpha1.StatusPhase, error) {
	r.logger.Info("reconciling grafana custom resource")

	var annotations = map[string]string{}
	annotations["service.alpha.openshift.io/serving-cert-secret-name"] = "grafana-k8s-tls"

	var serviceAccountAnnotations = map[string]string{}
	serviceAccountAnnotations["serviceaccounts.openshift.io/oauth-redirectreference.primary"] = "{\"kind\":\"OAuthRedirectReference\",\"apiVersion\":\"v1\",\"reference\":{\"kind\":\"Route\",\"name\":\"grafana-route\"}}"

	grafana := &grafanav1alpha1.Grafana{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "grafana",
			Namespace: r.Config.GetOperatorNamespace(),
		}, Spec: grafanav1alpha1.GrafanaSpec{
			Config: grafanav1alpha1.GrafanaConfig{
				Log: &grafanav1alpha1.GrafanaConfigLog{
					Mode:  "console",
					Level: "warn",
				},
				Auth: &grafanav1alpha1.GrafanaConfigAuth{
					DisableLoginForm:   &[]bool{false}[0],
					DisableSignoutMenu: &[]bool{true}[0],
				},
				AuthBasic: &grafanav1alpha1.GrafanaConfigAuthBasic{
					Enabled: &[]bool{true}[0],
				},
				AuthAnonymous: &grafanav1alpha1.GrafanaConfigAuthAnonymous{
					Enabled: &[]bool{true}[0],
				},
			},
			Containers: []v1.Container{
				{Name: "grafana-proxy",
					Image: "quay.io/openshift/origin-oauth-proxy:4.2",
					VolumeMounts: []v1.VolumeMount{
						{MountPath: "/etc/tls/private",
							Name:     "secret-grafana-k8s-tls",
							ReadOnly: false,
						},
						{MountPath: "/etc/proxy/secrets",
							Name:     "secret-grafana-k8s-proxy",
							ReadOnly: false,
						},
					},
					Args: []string{
						"-provider=openshift",
						"-pass-basic-auth=false",
						"-https-address=:9091",
						"-http-address=",
						"-email-domain=*",
						"-upstream=http://localhost:3000",
						"-openshift-sar={\"resource\":\"namespaces\",\"verb\":\"get\"}",
						"-openshift-delegate-urls={\"/\":{\"resource\":\"namespaces\",\"verb\":\"get\"}}",
						"-tls-cert=/etc/tls/private/tls.crt",
						"-tls-key=/etc/tls/private/tls.key",
						"-client-secret-file=/var/run/secrets/kubernetes.io/serviceaccount/token",
						"-cookie-secret-file=/etc/proxy/secrets/session_secret",
						"-openshift-service-account=grafana-serviceaccount",
						"-openshift-ca=/etc/pki/tls/cert.pem",
						"-openshift-ca=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
						"-skip-auth-regex=^/metrics"},
					Ports: []v1.ContainerPort{
						{ContainerPort: 9091,
							Name: "grafana-proxy"},
					},
				},
			},
			Secrets: []string{"grafana-k8s-tls", "grafana-k8s-proxy"},
			Service: &grafanav1alpha1.GrafanaService{
				Ports: []v1.ServicePort{
					{Name: "grafana-proxy",
						Port:     9091,
						Protocol: v1.ProtocolTCP,
					},
				},
				Annotations: annotations,
			},
			Ingress: &grafanav1alpha1.GrafanaIngress{
				Enabled:     true,
				TargetPort:  "grafana-proxy",
				Termination: "reencrypt",
			},
			Client: &grafanav1alpha1.GrafanaClient{
				PreferService: true,
			},
			Compat: &grafanav1alpha1.GrafanaCompat{
				FixAnnotations: true,
			},
			ServiceAccount: &grafanav1alpha1.GrafanaServiceAccount{
				Annotations: serviceAccountAnnotations,
			},
		},
	}

	status, err := controllerutil.CreateOrUpdate(ctx, client, grafana, func() error {
		owner.AddIntegreatlyOwnerAnnotations(grafana, r.installation)

		return nil
	})

	if err != nil {
		return integreatlyv1alpha1.PhaseFailed, err
	}

	logrus.Info("Grafana Status: ", status)

	// if there are no errors, the phase is complete
	return integreatlyv1alpha1.PhaseCompleted, nil
}

func (r *Reconciler) reconcileSubscription(ctx context.Context, serverClient k8sclient.Client, inst *integreatlyv1alpha1.RHMI, productNamespace string, operatorNamespace string) (integreatlyv1alpha1.StatusPhase, error) {
	r.logger.Info("reconciling subscription")

	target := marketplace.Target{
		Pkg:       constants.GrafanaSubscriptionName,
		Namespace: operatorNamespace,
		Channel:   marketplace.IntegreatlyChannel,
	}
	catalogSourceReconciler := marketplace.NewConfigMapCatalogSourceReconciler(
		manifestPackage,
		serverClient,
		operatorNamespace,
		marketplace.CatalogSourceName,
	)
	return r.Reconciler.ReconcileSubscription(
		ctx,
		target,
		[]string{productNamespace},
		r.preUpgradeBackupExecutor(),
		serverClient,
		catalogSourceReconciler,
	)
}

func (r *Reconciler) preUpgradeBackupExecutor() backup.BackupExecutor {
	return backup.NewNoopBackupExecutor()
}

// PopulateSessionProxySecret generates a session secret
func populateSessionProxySecret() string {
	p, err := generatePassword(43)
	if err != nil {
		logrus.Info("Error executing PopulateSessionProxySecret")
	}
	return p
}

// GeneratePassword returns a base64 encoded securely random bytes.
func generatePassword(n int) (string, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(b), err
}

func (r *Reconciler) reconcileHost(ctx context.Context, serverClient k8sclient.Client) (integreatlyv1alpha1.StatusPhase, error) {
	grafanaRoute := &routev1.Route{}

	err := serverClient.Get(ctx, k8sclient.ObjectKey{Name: defaultRoutename, Namespace: r.Config.GetOperatorNamespace()}, grafanaRoute)
	if err != nil {
		return integreatlyv1alpha1.PhaseFailed, fmt.Errorf("Failed to get route for Grafana: %w", err)
	}

	r.Config.SetHost("https://" + grafanaRoute.Spec.Host)
	err = r.ConfigManager.WriteConfig(r.Config)
	if err != nil {
		return integreatlyv1alpha1.PhaseFailed, fmt.Errorf("Could not set Grafana route: %w", err)
	}

	return integreatlyv1alpha1.PhaseCompleted, nil
}
