package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	tunnelv1alpha1 "github.com/bovf/pangolin-operator/api/v1alpha1"
	"github.com/bovf/pangolin-operator/pkg/pangolin"
)

const (
	TunnelFinalizerName = "tunnel.pangolin.io/finalizer"
)

// PangolinTunnelReconciler reconciles a PangolinTunnel object
type PangolinTunnelReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=tunnel.pangolin.io,resources=pangolintunnels,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=tunnel.pangolin.io,resources=pangolintunnels/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=tunnel.pangolin.io,resources=pangolintunnels/finalizers,verbs=update
//+kubebuilder:rbac:groups=tunnel.pangolin.io,resources=pangolinorganizations,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

// Reconcile implements the reconciliation logic for PangolinTunnel.
//
// PangolinTunnel manages Pangolin sites and their associated tunnel clients.
// A site represents a location where resources can be deployed, and the tunnel
// provides the network connectivity between Pangolin and the site.
//
// Reconciliation Flow:
//  1. Fetch PangolinTunnel from Kubernetes
//  2. Handle deletion if tunnel is being deleted
//  3. Add finalizer if not present
//  4. Get referenced organization for API credentials
//  5. Wait for organization to be ready
//  6. Create Pangolin API client using organization credentials
//  7. Reconcile site (bind to existing or create new)
//  8. Reconcile Newt secret for tunnel authentication (if Newt type)
//  9. Reconcile Newt deployment for tunnel client (if enabled)
//  10. Update status with site information and tunnel state
//
// Site Binding Modes:
//
// Bind by Numeric ID (spec.siteId set):
//   - Binds to existing site using numeric site ID
//   - Site must exist in Pangolin
//   - Status shows bindingMode: "Bound"
//
// Bind by Nice ID (spec.niceId set):
//   - Binds to existing site using human-readable nice ID
//   - Site must exist in Pangolin
//   - Status shows bindingMode: "Bound"
//
// Create New Site (neither ID set):
//   - Creates new site with specified name and type
//   - Uses tunnel name as site name if not specified
//   - Defaults to "newt" type if not specified
//   - Status shows bindingMode: "Created"
//
// Idempotency:
//   - Checks status first to avoid duplicate site creation
//   - Verifies existing sites by name to prevent duplicates
//   - Handles "already exists" errors gracefully
//
// Newt Client Management:
//   - For "newt" type sites, can optionally deploy in-cluster client
//   - Creates Secret with Newt credentials
//   - Deploys Newt client as Kubernetes Deployment
//   - Tracks client status in tunnel status
func (r *PangolinTunnelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the PangolinTunnel instance
	tunnel := &tunnelv1alpha1.PangolinTunnel{}
	err := r.Get(ctx, req.NamespacedName, tunnel)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("PangolinTunnel resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get PangolinTunnel")
		return ctrl.Result{}, err
	}

	// Handle deletion if tunnel is being deleted
	if tunnel.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, tunnel)
	}

	// Add finalizer to ensure proper cleanup
	if !controllerutil.ContainsFinalizer(tunnel, TunnelFinalizerName) {
		controllerutil.AddFinalizer(tunnel, TunnelFinalizerName)
		return ctrl.Result{}, r.Update(ctx, tunnel)
	}

	// Get the referenced organization
	org, err := r.getOrganizationForTunnel(ctx, tunnel)
	if err != nil {
		logger.Error(err, "Failed to get referenced organization")
		return r.updateStatus(ctx, tunnel, "Error", err.Error())
	}

	// Wait for organization to be ready
	if org.Status.Status != "Ready" {
		logger.Info("Organization not ready yet, waiting", "organization", org.Name)
		return r.updateStatus(ctx, tunnel, "Waiting", "Waiting for organization to be ready")
	}

	// Create Pangolin API client using organization credentials
	apiClient, err := r.createPangolinClientFromOrganization(ctx, org)
	if err != nil {
		logger.Error(err, "Failed to create Pangolin API client")
		return r.updateStatus(ctx, tunnel, "Error", err.Error())
	}

	// Get organization ID from organization status
	orgID := org.Status.OrganizationID
	if orgID == "" {
		return r.updateStatus(ctx, tunnel, "Error", "Organization missing organization ID")
	}

	// Reconcile site with flexible binding (bind or create)
	site, err := r.reconcileSite(ctx, *apiClient, orgID, tunnel)
	if err != nil {
		logger.Error(err, "Failed to reconcile site")
		return r.updateStatus(ctx, tunnel, "Error", err.Error())
	}

	// Create Newt secret if needed (for Newt tunnel authentication)
	err = r.reconcileNewtSecret(ctx, tunnel, site)
	if err != nil {
		logger.Error(err, "Failed to reconcile Newt secret")
		return r.updateStatus(ctx, tunnel, "Error", err.Error())
	}

	// Create Newt deployment if needed (for in-cluster tunnel client)
	err = r.reconcileNewtDeployment(ctx, tunnel, site)
	if err != nil {
		logger.Error(err, "Failed to reconcile Newt deployment")
		return r.updateStatus(ctx, tunnel, "Error", err.Error())
	}

	return r.updateStatus(ctx, tunnel, "Ready", "Tunnel is ready")
}

// getOrganizationForTunnel retrieves the PangolinOrganization referenced by the tunnel.
// The organization provides API credentials and organizational context.
func (r *PangolinTunnelReconciler) getOrganizationForTunnel(ctx context.Context, tunnel *tunnelv1alpha1.PangolinTunnel) (*tunnelv1alpha1.PangolinOrganization, error) {
	org := &tunnelv1alpha1.PangolinOrganization{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: tunnel.Namespace,
		Name:      tunnel.Spec.OrganizationRef.Name,
	}, org)
	if err != nil {
		return nil, fmt.Errorf("failed to get organization %s: %w", tunnel.Spec.OrganizationRef.Name, err)
	}
	return org, nil
}

// createPangolinClientFromOrganization creates a Pangolin API client using
// credentials from the referenced organization.
func (r *PangolinTunnelReconciler) createPangolinClientFromOrganization(ctx context.Context, org *tunnelv1alpha1.PangolinOrganization) (*pangolin.Client, error) {
	// Get API key from secret referenced by organization
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Namespace: org.Namespace,
		Name:      org.Spec.APIKeyRef.Name,
	}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		return nil, fmt.Errorf("failed to get API key secret: %w", err)
	}

	apiKeyBytes, ok := secret.Data[org.Spec.APIKeyRef.Key]
	if !ok {
		return nil, fmt.Errorf("API key not found in secret")
	}

	return pangolin.NewClient(org.Spec.APIEndpoint, string(apiKeyBytes)), nil
}

// reconcileSite handles flexible site binding and creation with comprehensive idempotency.
//
// Site Resolution (priority order):
//  1. Status Check: If status.siteId is set, verify site still exists in API
//  2. Binding by ID: If spec.siteId is set, bind to existing site by numeric ID
//  3. Binding by Nice ID: If spec.niceId is set, bind to existing site by nice ID
//  4. Duplicate Check: Search existing sites by name to avoid duplicates
//  5. Creation: Create new site if no binding method specified and no duplicate found
//
// Idempotency Measures:
//   - Always checks status first to prevent duplicate creation on reconcile
//   - Lists existing sites to detect duplicates before creating
//   - Handles "already exists" API errors gracefully
//   - Re-uses existing sites with matching names instead of creating duplicates
//
// Status Updates:
//   - siteId: Numeric site identifier
//   - niceId: Human-readable site identifier
//   - siteName: Display name of the site
//   - siteType: Site type (newt, wireguard, etc.)
//   - subnet: Site's network subnet
//   - address: Site's network address
//   - online: Whether site is currently online
//   - endpoint: Site's connection endpoint
//   - bindingMode: "Bound" or "Created"
func (r *PangolinTunnelReconciler) reconcileSite(ctx context.Context, apiClient pangolin.Client, orgID string, tunnel *tunnelv1alpha1.PangolinTunnel) (*pangolin.Site, error) {
	logger := log.FromContext(ctx)

	// STEP 1: Check status first to avoid duplicate creations
	// If we already have a site ID in status, verify it exists
	if tunnel.Status.SiteID != 0 {
		logger.Info("Site already exists in status, verifying", "siteId", tunnel.Status.SiteID)

		// Verify the site still exists in the API
		site, err := apiClient.GetSiteByID(ctx, tunnel.Status.SiteID)
		if err == nil {
			// Site exists and is valid, return it
			return site, nil
		}

		// Site doesn't exist anymore, log warning and continue to recreate
		logger.Info("Site in status no longer exists in API, will recreate", "siteId", tunnel.Status.SiteID)
	}

	// STEP 2: BINDING MODE - Bind to existing site by ID
	if tunnel.Spec.SiteID != nil || tunnel.Spec.NiceID != "" {
		var site *pangolin.Site
		var err error

		if tunnel.Spec.SiteID != nil {
			// Bind by numeric site ID
			logger.Info("Binding to existing site by ID", "siteId", *tunnel.Spec.SiteID)
			site, err = apiClient.GetSiteByID(ctx, *tunnel.Spec.SiteID)
		} else {
			// Bind by nice ID (human-readable identifier)
			logger.Info("Binding to existing site by NiceID", "niceId", tunnel.Spec.NiceID)
			site, err = apiClient.GetSiteByNiceID(ctx, orgID, tunnel.Spec.NiceID)
		}

		if err != nil {
			return nil, fmt.Errorf("failed to bind to existing site: %w", err)
		}

		// Update status with bound site information
		tunnel.Status.SiteID = site.SiteID
		tunnel.Status.NiceID = site.NiceID
		tunnel.Status.SiteName = site.Name
		tunnel.Status.SiteType = site.Type
		tunnel.Status.Subnet = site.Subnet
		tunnel.Status.Address = site.Address
		tunnel.Status.Online = site.Online
		tunnel.Status.Endpoint = site.Endpoint
		tunnel.Status.BindingMode = "Bound"

		return site, nil
	}

	// STEP 3: CREATE MODE - Create new site only if not already created
	// Double-check by listing existing sites to avoid duplicates
	existingSites, err := apiClient.ListSites(ctx, orgID)
	if err != nil {
		logger.Error(err, "Failed to list existing sites")
	} else {
		// Check if a site with the same name already exists
		siteName := tunnel.Spec.SiteName
		if siteName == "" {
			siteName = tunnel.Name // Use tunnel name as default
		}

		for _, existingSite := range existingSites {
			if existingSite.Name == siteName {
				logger.Info("Found existing site with same name, using it instead of creating duplicate",
					"siteName", siteName, "siteId", existingSite.SiteID)

				// Update status with found site
				tunnel.Status.SiteID = existingSite.SiteID
				tunnel.Status.NiceID = existingSite.NiceID
				tunnel.Status.SiteName = existingSite.Name
				tunnel.Status.SiteType = existingSite.Type
				tunnel.Status.BindingMode = "Bound"

				return &existingSite, nil
			}
		}
	}

	// STEP 4: No existing site found, create new one
	siteName := tunnel.Spec.SiteName
	if siteName == "" {
		siteName = tunnel.Name
	}

	siteType := tunnel.Spec.SiteType
	if siteType == "" {
		siteType = "newt" // Default site type
	}

	logger.Info("Creating new site", "siteName", siteName, "siteType", siteType)
	site, err := apiClient.CreateSite(ctx, orgID, siteName, siteType)
	if err != nil {
		// Check if error is due to duplicate name (race condition)
		if strings.Contains(err.Error(), "already exists") {
			logger.Info("Site already exists, attempting to retrieve it")
			// Try to find it by name
			existingSites, listErr := apiClient.ListSites(ctx, orgID)
			if listErr == nil {
				for _, existingSite := range existingSites {
					if existingSite.Name == siteName {
						logger.Info("Successfully retrieved existing site")
						tunnel.Status.SiteID = existingSite.SiteID
						tunnel.Status.NiceID = existingSite.NiceID
						tunnel.Status.SiteName = existingSite.Name
						tunnel.Status.SiteType = existingSite.Type
						tunnel.Status.BindingMode = "Bound"
						return &existingSite, nil
					}
				}
			}
		}
		return nil, fmt.Errorf("failed to create site: %w", err)
	}

	// Populate status with created site information
	tunnel.Status.SiteID = site.SiteID
	tunnel.Status.NiceID = site.NiceID
	tunnel.Status.SiteName = site.Name
	tunnel.Status.SiteType = site.Type
	tunnel.Status.BindingMode = "Created"

	return site, nil
}

// reconcileNewtSecret ensures a Secret with Newt credentials is present if needed.
//
// Newt Authentication:
//   - Newt sites require authentication credentials for tunnel clients
//   - Credentials are fetched from Pangolin API (site.newtId and secret material)
//   - Stored in a Kubernetes Secret for use by Newt deployment
//
// Secret Name: <tunnel-name>-newt-credentials
// Secret Contents:
//   - newtId: Newt instance identifier
//   - authToken: Authentication token for Newt client
//
// TODO: Implement credential fetching from Pangolin API
func (r *PangolinTunnelReconciler) reconcileNewtSecret(ctx context.Context, tunnel *tunnelv1alpha1.PangolinTunnel, site *pangolin.Site) error {
	// Only create secret for Newt sites with enabled client
	if tunnel.Status.SiteType != "newt" || tunnel.Spec.NewtClient == nil || !tunnel.Spec.NewtClient.Enabled {
		return nil
	}

	// TODO: Implement secret creation:
	// 1. Fetch Newt credentials from Pangolin API
	// 2. Create or update Secret with credentials
	// 3. Set owner reference to tunnel for automatic cleanup

	return nil
}

// reconcileNewtDeployment ensures the Newt client Deployment exists if enabled.
//
// Newt Client Deployment:
//   - Deploys in-cluster Newt client for tunnel connectivity
//   - Uses credentials from reconcileNewtSecret
//   - Configurable replica count and resources from spec.newtClient
//
// Deployment Configuration:
//   - Name: <tunnel-name>-newt-client
//   - Image: Newt client image from spec or default
//   - Replicas: From spec.newtClient.replicas or default 1
//   - Resources: From spec.newtClient.resources
//   - Env: Newt credentials from secret
//
// Status Tracking:
//   - readyReplicas: Number of ready Newt client replicas
//   - Used to determine overall tunnel health
//
// TODO: Implement deployment creation and status tracking
func (r *PangolinTunnelReconciler) reconcileNewtDeployment(ctx context.Context, tunnel *tunnelv1alpha1.PangolinTunnel, site *pangolin.Site) error {
	// Only create deployment for Newt sites with enabled client
	if tunnel.Status.SiteType != "newt" || tunnel.Spec.NewtClient == nil || !tunnel.Spec.NewtClient.Enabled {
		return nil
	}

	// TODO: Implement deployment creation:
	// 1. Build Deployment spec with Newt client configuration
	// 2. Create or update Deployment
	// 3. Set owner reference to tunnel for automatic cleanup
	// 4. Watch deployment status and update tunnel.status.readyReplicas

	return nil
}

// handleDeletion handles cleanup when a PangolinTunnel is being deleted.
//
// Cleanup Process:
//   - Optionally delete site from Pangolin API (if created by operator)
//   - Delete owned Newt deployment and secret (automatic via owner references)
//   - Remove finalizer to allow tunnel deletion
//
// Considerations:
//   - Sites created by operator (bindingMode: "Created") could be deleted
//   - Sites bound by operator (bindingMode: "Bound") should not be deleted
//   - Must be idempotent in case of failures or retries
//
// TODO: Implement optional API cleanup based on binding mode
func (r *PangolinTunnelReconciler) handleDeletion(ctx context.Context, tunnel *tunnelv1alpha1.PangolinTunnel) (ctrl.Result, error) {
	// TODO: Implement cleanup logic:
	// 1. Check if site was created (bindingMode: "Created")
	// 2. If created, optionally delete from Pangolin API
	// 3. Handle deletion errors gracefully
	// 4. Ensure idempotency (don't fail if already deleted)

	// Owned resources (Secret, Deployment) are automatically deleted by Kubernetes
	controllerutil.RemoveFinalizer(tunnel, TunnelFinalizerName)
	return ctrl.Result{}, r.Update(ctx, tunnel)
}

// updateStatus updates the status of a PangolinTunnel with the given status and message.
//
// Status values:
//   - "Ready": Tunnel is active, site is configured
//   - "Error": Reconciliation encountered an error
//   - "Waiting": Waiting for dependencies (organization)
//
// The function also updates the Ready condition with appropriate reason and message.
// Always requeues after 1 minute for status updates.
func (r *PangolinTunnelReconciler) updateStatus(ctx context.Context, tunnel *tunnelv1alpha1.PangolinTunnel, status, message string) (ctrl.Result, error) {
	tunnel.Status.Status = status
	tunnel.Status.ObservedGeneration = tunnel.Generation

	// Create Ready condition with proper timestamp
	newCondition := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "ReconcileError",
		Message:            message,
		LastTransitionTime: metav1.NewTime(time.Now()),
		ObservedGeneration: tunnel.Generation,
	}

	if status == "Ready" {
		newCondition.Status = metav1.ConditionTrue
		newCondition.Reason = "ReconcileSuccess"
	}

	tunnel.Status.Conditions = []metav1.Condition{newCondition}

	err := r.Status().Update(ctx, tunnel)
	return ctrl.Result{RequeueAfter: time.Minute}, err
}

// SetupWithManager sets up the controller with the Manager.
//
// Controller Configuration:
//   - Watches PangolinTunnel resources for changes
//   - Owns Secret resources (Newt credentials)
//   - Owns Deployment resources (Newt client)
//   - Does not watch Organizations directly (manual trigger required)
func (r *PangolinTunnelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tunnelv1alpha1.PangolinTunnel{}).
		Owns(&corev1.Secret{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}
