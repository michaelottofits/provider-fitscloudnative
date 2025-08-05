/*
Copyright 2025 The Crossplane Authors.

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

package cluster

import (
	"context"

	"fmt"

	"github.com/crossplane/crossplane-runtime/pkg/feature"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/pkg/connection"
	"github.com/crossplane/crossplane-runtime/pkg/controller"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/crossplane-runtime/pkg/statemetrics"

	"github.com/Nerzal/gocloak/v13"
	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/provider-fitscloudnative/apis/cluster/v1alpha1"
	apisv1alpha1 "github.com/crossplane/provider-fitscloudnative/apis/v1alpha1"
	"github.com/crossplane/provider-fitscloudnative/internal/features"
	cloudgo "github.com/fi-ts/cloud-go"
	cloudgoc "github.com/fi-ts/cloud-go/api/client"
	"github.com/fi-ts/cloud-go/api/client/cluster"
	"github.com/fi-ts/cloud-go/api/models"
)

const (
	errNotCluster   = "managed resource is not a Cluster custom resource"
	errTrackPCUsage = "cannot track ProviderConfig usage"
	errGetPC        = "cannot get ProviderConfig"
	errGetCreds     = "cannot get credentials"

	errNewClient = "cannot create new Service"
)

// A NoOpService does nothing.
type FitsCloudnativeService struct {
	fCli *cloudgoc.CloudAPI
}

type KeycloakAuth struct {
	KeycloakURL      string
	KeycloakUser     string
	KeycloakPassword string
	KeycloakClientID string
	KeycloakSecret   string
	KeycloakRealm    string
}

var (
	newFitsCloudnativeService = func(apiurl string, keycloakAuth KeycloakAuth) (*FitsCloudnativeService, error) {

		client := gocloak.NewClient(keycloakAuth.KeycloakURL)
		ctx := context.Background()

		usertoken, err := client.Login(ctx, keycloakAuth.KeycloakClientID, keycloakAuth.KeycloakSecret, keycloakAuth.KeycloakRealm, keycloakAuth.KeycloakUser, keycloakAuth.KeycloakPassword)

		if err != nil {
			return nil, err
		}

		c, err := cloudgo.NewClient(apiurl, usertoken.IDToken, "")

		return &FitsCloudnativeService{
			fCli: c,
		}, err
	}
)

// Setup adds a controller that reconciles Cluster managed resources.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	name := managed.ControllerName(v1alpha1.ClusterGroupKind)

	cps := []managed.ConnectionPublisher{managed.NewAPISecretPublisher(mgr.GetClient(), mgr.GetScheme())}
	if o.Features.Enabled(features.EnableAlphaExternalSecretStores) {
		cps = append(cps, connection.NewDetailsManager(mgr.GetClient(), apisv1alpha1.StoreConfigGroupVersionKind))
	}

	opts := []managed.ReconcilerOption{
		managed.WithExternalConnecter(&connector{
			kube:         mgr.GetClient(),
			usage:        resource.NewProviderConfigUsageTracker(mgr.GetClient(), &apisv1alpha1.ProviderConfigUsage{}),
			newServiceFn: newFitsCloudnativeService}),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
		managed.WithPollInterval(o.PollInterval),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))),
		managed.WithConnectionPublishers(cps...),
		managed.WithManagementPolicies(),
	}

	if o.Features.Enabled(feature.EnableAlphaChangeLogs) {
		opts = append(opts, managed.WithChangeLogger(o.ChangeLogOptions.ChangeLogger))
	}

	if o.MetricOptions != nil {
		opts = append(opts, managed.WithMetricRecorder(o.MetricOptions.MRMetrics))
	}

	if o.MetricOptions != nil && o.MetricOptions.MRStateMetrics != nil {
		stateMetricsRecorder := statemetrics.NewMRStateRecorder(
			mgr.GetClient(), o.Logger, o.MetricOptions.MRStateMetrics, &v1alpha1.ClusterList{}, o.MetricOptions.PollStateMetricInterval,
		)
		if err := mgr.Add(stateMetricsRecorder); err != nil {
			return errors.Wrap(err, "cannot register MR state metrics recorder for kind v1alpha1.ClusterList")
		}
	}

	r := managed.NewReconciler(mgr, resource.ManagedKind(v1alpha1.ClusterGroupVersionKind), opts...)

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		WithEventFilter(resource.DesiredStateChanged()).
		For(&v1alpha1.Cluster{}).
		Complete(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter))
}

// A connector is expected to produce an ExternalClient when its Connect method
// is called.
type connector struct {
	kube         client.Client
	usage        resource.Tracker
	newServiceFn func(apiUrl string, kc KeycloakAuth) (*FitsCloudnativeService, error)
}

// Connect typically produces an ExternalClient by:
// 1. Tracking that the managed resource is using a ProviderConfig.
// 2. Getting the managed resource's ProviderConfig.
// 3. Getting the credentials specified by the ProviderConfig.
// 4. Using the credentials to form a client.
func (c *connector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	cr, ok := mg.(*v1alpha1.Cluster)
	if !ok {
		return nil, errors.New(errNotCluster)
	}

	if err := c.usage.Track(ctx, mg); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	pc := &apisv1alpha1.ProviderConfig{}
	if err := c.kube.Get(ctx, types.NamespacedName{Name: cr.GetProviderConfigReference().Name}, pc); err != nil {
		return nil, errors.Wrap(err, errGetPC)
	}

	cd := pc.Spec.Credentials
	data, err := resource.CommonCredentialExtractor(ctx, cd.Source, c.kube, cd.CommonCredentialSelectors)
	if err != nil {
		return nil, errors.Wrap(err, errGetCreds)
	}

	//TODO get here cloudapi url user and password for keycloak to connect from provider cd
	kc := KeycloakAuth{
		KeycloakURL:      pc.Spec.KeycloakURL,
		KeycloakRealm:    pc.Spec.KeycloakRealm,
		KeycloakClientID: pc.Spec.KeycloakClientID,
		KeycloakSecret:   pc.Spec.KeycloakClientSecret,
		KeycloakUser:     pc.Spec.KeycloakUser,
		KeycloakPassword: string(data),
	}

	svc, err := c.newServiceFn(pc.Spec.ApiURL, kc)
	if err != nil {
		return nil, errors.Wrap(err, errNewClient)
	}

	return &external{service: svc}, nil
}

// An ExternalClient observes, then either creates, updates, or deletes an
// external resource to ensure it reflects the managed resource's desired state.
type external struct {
	// A 'client' used to connect to the external resource API. In practice this
	// would be something like an AWS SDK client.
	service *FitsCloudnativeService
}

func (c *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha1.Cluster)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotCluster)
	}

	var cfr = &models.V1ClusterFindRequest{}
	cfr.Name = &cr.Spec.ForProvider.Name
	cfr.Tenant = &cr.Spec.ForProvider.Tenant

	searchrequest := cluster.NewFindClustersParams()
	searchrequest.SetBody(cfr)

	response, err := c.service.fCli.Cluster.FindClusters(searchrequest, nil)

	//if we found one cluster with tenant and cluster name, one not more !
	if len(response.GetPayload()) != 1 {
		return managed.ExternalObservation{
			ResourceExists: false,
		}, nil
	}

	// TODO Check what state is cluster is available
	if response.GetPayload()[0].Status.LastOperation.State != nil {
		cr.Status.SetConditions(xpv1.Available())
	}

	fmt.Printf("Observing Error Find Cluster: %+v", err)

	// These fmt statements should be removed in the real implementation.
	fmt.Printf("Observing: %+v", cr)

	return managed.ExternalObservation{
		// Return false when the external resource does not exist. This lets
		// the managed resource reconciler know that it needs to call Create to
		// (re)create the resource, or that it has successfully been deleted.
		ResourceExists: true,

		// Return false when the external resource exists, but it not up to date
		// with the desired managed resource state. This lets the managed
		// resource reconciler know that it needs to call Update.
		ResourceUpToDate: true,

		// Return any details that may be required to connect to the external
		// resource. These will be stored as the connection secret.
		ConnectionDetails: managed.ConnectionDetails{},
	}, nil
}

func (c *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	cr, ok := mg.(*v1alpha1.Cluster)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotCluster)
	}

	fmt.Printf("Creating: %+v", cr)

	return managed.ExternalCreation{
		// Optionally return any details that may be required to connect to the
		// external resource. These will be stored as the connection secret.
		ConnectionDetails: managed.ConnectionDetails{},
	}, nil
}

func (c *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {
	cr, ok := mg.(*v1alpha1.Cluster)
	if !ok {
		return managed.ExternalUpdate{}, errors.New(errNotCluster)
	}

	fmt.Printf("Updating: %+v", cr)

	return managed.ExternalUpdate{
		// Optionally return any details that may be required to connect to the
		// external resource. These will be stored as the connection secret.
		ConnectionDetails: managed.ConnectionDetails{},
	}, nil
}

func (c *external) Delete(ctx context.Context, mg resource.Managed) (managed.ExternalDelete, error) {
	cr, ok := mg.(*v1alpha1.Cluster)
	if !ok {
		return managed.ExternalDelete{}, errors.New(errNotCluster)
	}

	fmt.Printf("Deleting: %+v", cr)

	return managed.ExternalDelete{}, nil
}

func (c *external) Disconnect(ctx context.Context) error {
	return nil
}
