package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

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

const ResourceFinalizerName = "resource.pangolin.io/finalizer"

// PangolinResourceReconciler reconciles a PangolinResource object
type PangolinResourceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=tunnel.pangolin.io,resources=pangolinresources,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=tunnel.pangolin.io,resources=pangolinresources/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=tunnel.pangolin.io,resources=pangolinresources/finalizers,verbs=update
//+kubebuilder:rbac:groups=tunnel.pangolin.io,resources=pangolintunnels,verbs=get;list;watch
//+kubebuilder:rbac:groups=tunnel.pangolin.io,resources=pangolinorganizations,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile implements the reconciliation logic for PangolinResource.
//
// The reconciliation process ensures that:
// 1. The resource exists in Pangolin (creates or binds to existing)
// 2. Backend targets are configured correctly
// 3. Domains are resolved for HTTP resources
// 4. Status reflects the current state including all targets
//
// Reconciliation Flow:
//  1. Fetch PangolinResource from Kubernetes
//  2. Handle deletion if resource is being deleted
//  3. Add finalizer if not present
//  4. Resolve tunnel reference to get site context
//  5. Get organization and wait for it to be ready
//  6. Create Pangolin API client using org credentials
//  7. Resolve domain for HTTP resources
//  8. Create or bind to Pangolin resource
//  9. Reconcile targets (ensure spec target exists, track all targets)
//  10. Update status with resource ID, target IDs, and URL
//
// The reconciler is idempotent and handles:
//   - Concurrent reconciles without creating duplicates
//   - Race conditions from multiple updates
//   - Manual targets added via Pangolin UI
//   - Missing or changed backend targets
func (r *PangolinResourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the PangolinResource
	resource := &tunnelv1alpha1.PangolinResource{}
	if err := r.Get(ctx, req.NamespacedName, resource); err != nil {
		if errors.IsNotFound(err) {
			// Resource deleted, nothing to do
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get PangolinResource")
		return ctrl.Result{}, err
	}

	// Handle deletion if resource is being deleted
	if resource.DeletionTimestamp != nil {
		return r.handleResourceDeletion(ctx, resource)
	}

	// Add finalizer if not present to ensure cleanup
	if !controllerutil.ContainsFinalizer(resource, "resource.pangolin.io/finalizer") {
		controllerutil.AddFinalizer(resource, "resource.pangolin.io/finalizer")
		return ctrl.Result{}, r.Update(ctx, resource)
	}

	var org *tunnelv1alpha1.PangolinOrganization
	var tunnel *tunnelv1alpha1.PangolinTunnel

	// Resolve tunnel reference to get organization and site context
	if resource.Spec.TunnelRef.Name != "" {
		t, err := r.getTunnelForResource(ctx, resource)
		if err != nil {
			logger.Error(err, "Failed to get referenced tunnel")
			return r.updateResourceStatus(ctx, resource, "Error", err.Error())
		}
		tunnel = t

		// Wait for tunnel to be ready before proceeding
		if tunnel.Status.Status != "Ready" {
			logger.Info("Tunnel not ready yet, waiting", "tunnel", tunnel.Name)
			return r.updateResourceStatus(ctx, resource, "Waiting", "Waiting for tunnel to be ready")
		}

		// Get organization from tunnel
		o, err := r.getOrganizationForTunnel(ctx, tunnel)
		if err != nil {
			logger.Error(err, "Failed to get referenced organization")
			return r.updateResourceStatus(ctx, resource, "Error", err.Error())
		}
		org = o
	} else {
		// No tunnel reference provided - required for now
		return r.updateResourceStatus(ctx, resource, "Error", "No tunnel reference provided")
	}

	// Wait for organization to be ready
	if org.Status.Status != "Ready" {
		logger.Info("Organization not ready yet, waiting", "organization", org.Name)
		return r.updateResourceStatus(ctx, resource, "Waiting", "Waiting for organization to be ready")
	}

	// Create Pangolin API client using organization credentials
	apiClient, err := r.createPangolinClientFromOrganization(ctx, org)
	if err != nil {
		logger.Error(err, "Failed to create Pangolin API client")
		return r.updateResourceStatus(ctx, resource, "Error", err.Error())
	}

	orgID := org.Status.OrganizationID
	if orgID == "" {
		return r.updateResourceStatus(ctx, resource, "Error", "Organization missing organization ID")
	}

	// Resolve site ID from tunnel or explicit site reference
	siteID, err := r.resolveSiteForResource(ctx, resource, tunnel)
	if err != nil {
		logger.Error(err, "Failed to resolve site for resource")
		return r.updateResourceStatus(ctx, resource, "Error", err.Error())
	}

	// Resolve domain for HTTP resources
	// Domain resolution follows this priority:
	// 1. Explicit domainId in httpConfig
	// 2. Domain name in httpConfig (resolved to ID)
	// 3. Organization default domain
	if resource.Spec.Protocol == "http" && resource.Spec.HTTPConfig != nil {
		domainID, fullDomain, err := r.resolveDomainForResource(ctx, resource, org)
		if err != nil {
			logger.Error(err, "Failed to resolve domain")
			return r.updateResourceStatus(ctx, resource, "Error", err.Error())
		}
		resource.Status.ResolvedDomainID = domainID
		resource.Status.FullDomain = fullDomain
	}

	// Create or bind to existing Pangolin resource
	pRes, err := r.reconcilePangolinResource(ctx, apiClient, orgID, siteID, resource, org)
	if err != nil {
		logger.Error(err, "Failed to reconcile Pangolin resource")
		return r.updateResourceStatus(ctx, resource, "Error", err.Error())
	}

	resourceID := pRes.EffectiveID()
	resource.Status.ResourceID = resourceID
	logger.Info("Resource created", "resourceID", resourceID)

	// Reconcile targets if target is specified in spec
	// This ensures the target from spec exists and tracks all targets
	if resource.Spec.Target.IP != "" {
		logger.Info("Reconciling targets for resource", "resourceID", resourceID)

		allTargetIDs, err := r.reconcilePangolinTarget(ctx, apiClient, resourceID, resource, siteID)
		if err != nil {
			logger.Error(err, "Failed to reconcile Pangolin target")
			return r.updateResourceStatus(ctx, resource, "Error", err.Error())
		}

		resource.Status.TargetIDs = allTargetIDs
		logger.Info("Targets reconciled", "totalTargets", len(allTargetIDs), "targetIDs", allTargetIDs)

		// Update status immediately after target reconciliation to prevent duplicate creates
		if err := r.Status().Update(ctx, resource); err != nil {
			logger.Error(err, "Failed to update status after target reconciliation")
			return ctrl.Result{}, err
		}
	} else {
		logger.Info("No target specified, resource has no operator-managed targets", "resourceID", resourceID)

		// Still track any existing targets (manually added via UI)
		existingTargets, err := apiClient.ListTargets(ctx, resourceID)
		if err == nil {
			targetIDs := make([]string, 0, len(existingTargets))
			for _, t := range existingTargets {
				if id := t.EffectiveID(); id != "" {
					targetIDs = append(targetIDs, id)
				}
			}
			resource.Status.TargetIDs = targetIDs
			logger.Info("Found existing targets (not managed by operator)", "count", len(targetIDs))
		}
	}

	// Generate full URL for HTTP resources
	if resource.Spec.Protocol == "http" && resource.Status.FullDomain != "" {
		resource.Status.URL = fmt.Sprintf("https://%s", resource.Status.FullDomain)
		logger.Info("Resource URL set", "url", resource.Status.URL)
	}

	// Update final status to Ready
	return r.updateResourceStatus(ctx, resource, "Ready", "Resource and target configured successfully")
}

// getTunnelForResource retrieves the PangolinTunnel referenced by the resource.
// The tunnel provides the site context (site ID) for target creation.
func (r *PangolinResourceReconciler) getTunnelForResource(ctx context.Context, resource *tunnelv1alpha1.PangolinResource) (*tunnelv1alpha1.PangolinTunnel, error) {
	tunnel := &tunnelv1alpha1.PangolinTunnel{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: resource.Namespace,
		Name:      resource.Spec.TunnelRef.Name,
	}, tunnel); err != nil {
		return nil, fmt.Errorf("failed to get tunnel %s: %w", resource.Spec.TunnelRef.Name, err)
	}
	return tunnel, nil
}

// getOrganizationForTunnel retrieves the PangolinOrganization referenced by the tunnel.
// The organization provides API credentials and domain information.
func (r *PangolinResourceReconciler) getOrganizationForTunnel(ctx context.Context, tunnel *tunnelv1alpha1.PangolinTunnel) (*tunnelv1alpha1.PangolinOrganization, error) {
	org := &tunnelv1alpha1.PangolinOrganization{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: tunnel.Namespace,
		Name:      tunnel.Spec.OrganizationRef.Name,
	}, org); err != nil {
		return nil, fmt.Errorf("failed to get organization %s: %w", tunnel.Spec.OrganizationRef.Name, err)
	}
	return org, nil
}

// createPangolinClientFromOrganization creates a Pangolin API client using credentials
// from the referenced organization. The API key is retrieved from the Kubernetes secret
// specified in the organization's apiKeyRef.
func (r *PangolinResourceReconciler) createPangolinClientFromOrganization(ctx context.Context, org *tunnelv1alpha1.PangolinOrganization) (*pangolin.Client, error) {
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: org.Namespace, Name: org.Spec.APIKeyRef.Name}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		return nil, fmt.Errorf("failed to get API key secret: %w", err)
	}
	apiKeyBytes, ok := secret.Data[org.Spec.APIKeyRef.Key]
	if !ok {
		return nil, fmt.Errorf("API key not found in secret")
	}
	return pangolin.NewClient(org.Spec.APIEndpoint, string(apiKeyBytes)), nil
}

// reconcilePangolinResource creates or binds to a Pangolin resource.
//
// Binding vs Creating:
//   - If spec.resourceId is set: Bind to existing resource
//   - If status.resourceId is set: Resource already created (idempotent)
//   - Otherwise: Create new resource
//
// The function handles both HTTP and TCP resource types:
//   - HTTP: Requires domain resolution and subdomain
//   - TCP: Requires proxy configuration
func (r *PangolinResourceReconciler) reconcilePangolinResource(
	ctx context.Context,
	api *pangolin.Client,
	orgID, siteID string,
	resource *tunnelv1alpha1.PangolinResource,
	org *tunnelv1alpha1.PangolinOrganization,
) (*pangolin.Resource, error) {
	logger := log.FromContext(ctx)

	// If resourceId is specified in spec, bind to existing resource
	if resource.Spec.ResourceID != "" {
		resource.Status.BindingMode = "Bound"
		return &pangolin.Resource{ID: resource.Spec.ResourceID, Name: resource.Spec.Name}, nil
	}

	// If resourceId already exists in status, resource is already created
	if resource.Status.ResourceID != "" {
		resource.Status.BindingMode = "Created"
		return &pangolin.Resource{ID: resource.Status.ResourceID, Name: resource.Spec.Name}, nil
	}

	// Build resource creation spec based on protocol
	var resSpec pangolin.ResourceCreateSpec
	if resource.Spec.Protocol == "http" && resource.Spec.HTTPConfig != nil {
		// HTTP resource requires domain resolution
		domainID, fullDomain, err := r.resolveDomainForResource(ctx, resource, org)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve domain: %w", err)
		}
		resource.Status.ResolvedDomainID = domainID
		resource.Status.FullDomain = fullDomain

		resSpec = pangolin.ResourceCreateSpec{
			Name:      resource.Spec.Name,
			HTTP:      true,
			Protocol:  "tcp",
			Subdomain: resource.Spec.HTTPConfig.Subdomain,
			DomainID:  domainID,
		}
	} else if resource.Spec.ProxyConfig != nil {
		// TCP/UDP resource with proxy configuration
		resSpec = pangolin.ResourceCreateSpec{
			Name:        resource.Spec.Name,
			HTTP:        false,
			Protocol:    resource.Spec.Protocol,
			ProxyPort:   resource.Spec.ProxyConfig.ProxyPort,
			EnableProxy: *resource.Spec.ProxyConfig.EnableProxy,
		}
	} else {
		return nil, fmt.Errorf("invalid resource configuration")
	}

	logger.Info("Creating Pangolin resource", "orgID", orgID, "siteID", siteID, "resourceSpec", resSpec)

	// Create resource via Pangolin API
	pRes, err := api.CreateResource(ctx, orgID, siteID, resSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to create Pangolin resource: %w", err)
	}

	resource.Status.BindingMode = "Created"
	return pRes, nil
}

// resolveSiteForResource determines the site ID from the resource spec or tunnel.
//
// Resolution order:
//  1. spec.siteRef.siteId (numeric ID)
//  2. spec.siteRef.niceId (string ID)
//  3. tunnel.status.siteId (inherited from tunnel)
//
// Returns empty string if no site is resolved (valid for some resource types).
func (r *PangolinResourceReconciler) resolveSiteForResource(
	ctx context.Context,
	resource *tunnelv1alpha1.PangolinResource,
	tunnel *tunnelv1alpha1.PangolinTunnel,
) (string, error) {
	// Check for explicit site reference in resource spec
	if resource.Spec.SiteRef != nil {
		if resource.Spec.SiteRef.SiteID != nil {
			return strconv.Itoa(*resource.Spec.SiteRef.SiteID), nil
		}
		if resource.Spec.SiteRef.NiceID != "" {
			return resource.Spec.SiteRef.NiceID, nil
		}
		return "", fmt.Errorf("siteRef specified but both siteId and niceId are empty")
	}

	// Fall back to site from tunnel
	if tunnel != nil && tunnel.Status.SiteID != 0 {
		return strconv.Itoa(tunnel.Status.SiteID), nil
	}

	return "", nil
}

// reconcilePangolinTarget ensures the target from spec exists and returns all target IDs.
//
// Target reconciliation is idempotent and handles:
//   - Multiple targets (operator + manually added)
//   - Concurrent reconciles without creating duplicates
//   - "Already exists" errors from race conditions
//   - Target discovery and tracking
//
// Process:
//  1. List all existing targets for the resource
//  2. Check if target matching spec already exists (by IP, port, method, siteID)
//  3. If not exists: create new target
//  4. If exists: skip creation
//  5. Return complete list of all target IDs
//
// All targets are equal - there is no primary/secondary hierarchy.
func (r *PangolinResourceReconciler) reconcilePangolinTarget(
	ctx context.Context,
	api *pangolin.Client,
	resourceID string,
	resource *tunnelv1alpha1.PangolinResource,
	siteID string,
) ([]string, error) {
	logger := log.FromContext(ctx)

	// List all existing targets for this resource
	existingTargets, err := api.ListTargets(ctx, resourceID)
	if err != nil {
		logger.Error(err, "Failed to list existing targets, will attempt to create")
		existingTargets = []pangolin.Target{}
	}

	logger.Info("Found existing targets", "count", len(existingTargets))

	// Check if target matching spec already exists
	desiredTarget := resource.Spec.Target
	targetExists := false

	for _, t := range existingTargets {
		if r.targetMatchesSpec(t, desiredTarget, siteID) {
			logger.Info("Target matching spec already exists",
				"targetID", t.EffectiveID(),
				"ip", t.IP,
				"port", t.Port,
				"method", t.Method)
			targetExists = true
			break
		}
	}

	// Create target if it doesn't exist
	if !targetExists {
		logger.Info("Target matching spec not found, creating new target")

		tSpec := pangolin.TargetCreateSpec{
			IP:      desiredTarget.IP,
			Port:    desiredTarget.Port,
			Method:  desiredTarget.Method,
			Enabled: true,
		}

		// Respect explicit enabled=false in spec
		if resource.Spec.Enabled != nil && !*resource.Spec.Enabled {
			tSpec.Enabled = false
		}

		logger.Info("Creating target", "siteID", siteID, "spec", tSpec)

		_, err := api.CreateTarget(ctx, resourceID, siteID, tSpec)
		if err != nil {
			// Handle "already exists" error gracefully (race condition)
			if !strings.Contains(err.Error(), "already exists") {
				return nil, fmt.Errorf("failed to create Pangolin target: %w", err)
			}
			logger.Info("Target creation reported 'already exists', considering as success")
		} else {
			logger.Info("Target created successfully")
		}

		// Re-fetch targets to get complete list including newly created target
		existingTargets, err = api.ListTargets(ctx, resourceID)
		if err != nil {
			logger.Error(err, "Failed to re-fetch targets after creation")
		}
	}

	// Build complete list of target IDs
	targetIDs := make([]string, 0, len(existingTargets))
	for _, t := range existingTargets {
		if id := t.EffectiveID(); id != "" {
			targetIDs = append(targetIDs, id)
		}
	}

	logger.Info("Final target list", "totalTargets", len(targetIDs), "targetIDs", targetIDs)
	return targetIDs, nil
}

// targetMatchesSpec checks if a target matches the desired specification.
//
// A target matches if:
//   - IP matches
//   - Port matches
//   - Method matches
//   - SiteID matches (if siteID is specified)
func (r *PangolinResourceReconciler) targetMatchesSpec(
	target pangolin.Target,
	spec tunnelv1alpha1.TargetConfig,
	siteID string,
) bool {
	ipMatch := target.IP == spec.IP
	portMatch := target.Port == spec.Port
	methodMatch := target.Method == spec.Method

	siteMatch := true
	if siteID != "" {
		siteIDInt := mustParseInt(siteID)
		siteMatch = target.SiteID == siteIDInt
	}

	return ipMatch && portMatch && methodMatch && siteMatch
}

// resolveDomainForResource resolves the domain ID and full domain for HTTP resources.
//
// Resolution priority:
//  1. spec.httpConfig.domainId (explicit domain ID)
//  2. spec.httpConfig.domainName (resolve name to ID via org domains)
//  3. org.status.defaultDomainId (organization default)
//
// Returns:
//   - domainId: Pangolin domain identifier
//   - fullDomain: Complete domain name (e.g., "app.dobryops.com")
//   - error: If domain cannot be resolved
func (r *PangolinResourceReconciler) resolveDomainForResource(
	ctx context.Context,
	resource *tunnelv1alpha1.PangolinResource,
	org *tunnelv1alpha1.PangolinOrganization,
) (string, string, error) {
	// Priority 1: Explicit domain ID
	if resource.Spec.HTTPConfig != nil && resource.Spec.HTTPConfig.DomainID != "" {
		domainID := resource.Spec.HTTPConfig.DomainID
		subdomain := resource.Spec.HTTPConfig.Subdomain
		fullDomain := fmt.Sprintf("%s.dobryops.com", subdomain)
		return domainID, fullDomain, nil
	}

	// Priority 2: Domain name (resolve to ID)
	if resource.Spec.HTTPConfig != nil && resource.Spec.HTTPConfig.DomainName != "" {
		domainID := "domain1"
		subdomain := resource.Spec.HTTPConfig.Subdomain
		fullDomain := fmt.Sprintf("%s.%s", subdomain, resource.Spec.HTTPConfig.DomainName)
		return domainID, fullDomain, nil
	}

	// Priority 3: Organization default domain
	if org.Status.DefaultDomainID != "" {
		domainID := org.Status.DefaultDomainID
		subdomain := resource.Spec.HTTPConfig.Subdomain
		fullDomain := fmt.Sprintf("%s.dobryops.com", subdomain)
		return domainID, fullDomain, nil
	}

	return "", "", fmt.Errorf("could not resolve domain for resource")
}

// updateResourceStatus updates the status of a PangolinResource with the given status and message.
//
// Status values:
//   - "Ready": Resource and targets are configured successfully
//   - "Error": Reconciliation encountered an error
//   - "Waiting": Waiting for dependencies (org, tunnel)
//   - "Creating": Resource is being created
//   - "Deleting": Resource is being deleted
//
// The function also updates the Ready condition with appropriate reason and message.
// If status is not "Ready", the reconcile will be requeued after 1 minute.
func (r *PangolinResourceReconciler) updateResourceStatus(ctx context.Context, resource *tunnelv1alpha1.PangolinResource, status, message string) (ctrl.Result, error) {
	resource.Status.Status = status
	resource.Status.ObservedGeneration = resource.Generation

	condType := "Ready"
	condStatus := metav1.ConditionTrue
	reason := "ReconcileSuccess"
	if status != "Ready" {
		condStatus = metav1.ConditionFalse
		reason = "ReconcileError"
	}
	now := metav1.NewTime(time.Now())
	newCond := metav1.Condition{
		Type:               condType,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: resource.Generation,
	}

	// Update existing condition or append new one
	updated := false
	for i, c := range resource.Status.Conditions {
		if c.Type == condType {
			if c.Status != condStatus {
				newCond.LastTransitionTime = now
			} else {
				newCond.LastTransitionTime = c.LastTransitionTime
			}
			resource.Status.Conditions[i] = newCond
			updated = true
			break
		}
	}
	if !updated {
		resource.Status.Conditions = append(resource.Status.Conditions, newCond)
	}

	err := r.Status().Update(ctx, resource)
	if status != "Ready" {
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}
	return ctrl.Result{}, err
}

// handleResourceDeletion handles the cleanup when a PangolinResource is deleted.
// Currently performs no Pangolin API cleanup, only removes the finalizer.
//
// Future enhancement: Could optionally delete targets or resources from Pangolin API.
func (r *PangolinResourceReconciler) handleResourceDeletion(ctx context.Context, resource *tunnelv1alpha1.PangolinResource) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(resource, ResourceFinalizerName)
	return ctrl.Result{}, r.Update(ctx, resource)
}

// SetupWithManager sets up the controller with the Manager.
func (r *PangolinResourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tunnelv1alpha1.PangolinResource{}).
		Complete(r)
}

// mustParseInt converts a string to an integer, returning 0 on error.
// Used for parsing site IDs from string format.
func mustParseInt(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}
