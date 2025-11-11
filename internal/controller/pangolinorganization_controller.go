package controller

import (
	"context"
	"fmt"
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

const (
	OrganizationFinalizerName = "organization.pangolin.io/finalizer"
)

// PangolinOrganizationReconciler reconciles a PangolinOrganization object
type PangolinOrganizationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=tunnel.pangolin.io,resources=pangolinorganizations,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=tunnel.pangolin.io,resources=pangolinorganizations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=tunnel.pangolin.io,resources=pangolinorganizations/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile implements the reconciliation logic for PangolinOrganization.
//
// PangolinOrganization is the root resource that provides API credentials and
// organization-level configuration for all other Pangolin resources. It serves as
// the credential store and domain registry for the operator.
//
// Reconciliation Flow:
//  1. Fetch PangolinOrganization from Kubernetes
//  2. Handle deletion if organization is being deleted
//  3. Add finalizer if not present
//  4. Create Pangolin API client using credentials from secret
//  5. Reconcile organization (bind to existing or discover first available)
//  6. Discover and cache all available domains
//  7. Resolve default domain from spec or use first verified domain
//  8. Update status with organization info, domains, and default domain
//
// Organization Modes:
//
// Binding Mode (spec.organizationId set):
//   - Binds to a specific organization by ID
//   - Validates that the organization exists
//   - Status shows bindingMode: "Bound"
//
// Discovery Mode (spec.organizationId empty):
//   - Auto-discovers first available organization
//   - Useful when API key has access to only one organization
//   - Status shows bindingMode: "Discovered"
//
// Domain Management:
//   - Fetches all domains from Pangolin API
//   - Caches domains in status for use by resources
//   - Resolves default domain name to domain ID
//   - Falls back to first verified domain if no default specified
//
// The organization serves as a dependency for:
//   - PangolinTunnel (requires org for site creation)
//   - PangolinResource (requires org for domain resolution)
//   - PangolinBinding (requires org for service exposure)
func (r *PangolinOrganizationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the PangolinOrganization instance
	org := &tunnelv1alpha1.PangolinOrganization{}
	err := r.Get(ctx, req.NamespacedName, org)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("PangolinOrganization resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get PangolinOrganization")
		return ctrl.Result{}, err
	}

	// Handle deletion if organization is being deleted
	if org.DeletionTimestamp != nil {
		return r.handleOrganizationDeletion(ctx, org)
	}

	// Add finalizer to ensure proper cleanup
	if !controllerutil.ContainsFinalizer(org, OrganizationFinalizerName) {
		controllerutil.AddFinalizer(org, OrganizationFinalizerName)
		return ctrl.Result{}, r.Update(ctx, org)
	}

	// Create Pangolin API client using credentials from secret
	apiClient, err := r.createPangolinClient(ctx, org)
	if err != nil {
		logger.Error(err, "Failed to create Pangolin API client")
		return r.updateOrganizationStatus(ctx, org, "Error", err.Error())
	}

	// Reconcile organization (bind to existing or discover)
	err = r.reconcileOrganization(ctx, org, apiClient)
	if err != nil {
		logger.Error(err, "Failed to reconcile organization")
		return r.updateOrganizationStatus(ctx, org, "Error", err.Error())
	}

	// Discover and cache all available domains
	err = r.reconcileDomains(ctx, org, apiClient)
	if err != nil {
		logger.Error(err, "Failed to reconcile domains")
		return r.updateOrganizationStatus(ctx, org, "Error", err.Error())
	}

	return r.updateOrganizationStatus(ctx, org, "Ready", "Organization is ready")
}

// createPangolinClient creates a Pangolin API client from the organization spec.
//
// The API key is retrieved from a Kubernetes Secret referenced by spec.apiKeyRef.
// This allows secure storage of credentials separate from the CRD.
//
// Secret Format:
//
//	apiVersion: v1
//	kind: Secret
//	metadata:
//	  name: pangolin-credentials
//	data:
//	  apiKey: <base64-encoded-api-key>
//
// The function validates that:
//   - Secret exists in the same namespace as the organization
//   - Secret contains the specified key
//   - API key is not empty
func (r *PangolinOrganizationReconciler) createPangolinClient(ctx context.Context, org *tunnelv1alpha1.PangolinOrganization) (*pangolin.Client, error) {
	// Get API key from secret
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: org.Namespace,
		Name:      org.Spec.APIKeyRef.Name,
	}, secret)
	if err != nil {
		return nil, fmt.Errorf("failed to get API key secret: %w", err)
	}

	apiKey, ok := secret.Data[org.Spec.APIKeyRef.Key]
	if !ok {
		return nil, fmt.Errorf("API key not found in secret")
	}

	return pangolin.NewClient(org.Spec.APIEndpoint, string(apiKey)), nil
}

// reconcileOrganization handles organization binding or discovery.
//
// Two Modes:
//
// 1. Binding Mode (spec.organizationId specified):
//   - Lists all accessible organizations from API
//   - Finds organization matching the specified ID
//   - Returns error if organization not found or not accessible
//   - Sets status.bindingMode = "Bound"
//
// 2. Discovery Mode (spec.organizationId empty):
//   - Lists all accessible organizations from API
//   - Uses the first organization found
//   - Useful when API key has access to exactly one organization
//   - Sets status.bindingMode = "Discovered"
//
// Status Updates:
//   - organizationId: Pangolin organization identifier
//   - organizationName: Human-readable organization name
//   - subnet: Organization's network subnet (if available)
//   - bindingMode: "Bound" or "Discovered"
func (r *PangolinOrganizationReconciler) reconcileOrganization(ctx context.Context, org *tunnelv1alpha1.PangolinOrganization, apiClient *pangolin.Client) error {
	if org.Spec.OrganizationID != "" {
		// BINDING MODE: Bind to existing organization
		orgs, err := apiClient.ListOrganizations(ctx)
		if err != nil {
			return fmt.Errorf("failed to list organizations: %w", err)
		}

		// Find the specified organization
		var targetOrg *pangolin.Organization
		for _, o := range orgs {
			if o.OrgID == org.Spec.OrganizationID {
				targetOrg = &o
				break
			}
		}

		if targetOrg == nil {
			return fmt.Errorf("organization %s not found", org.Spec.OrganizationID)
		}

		// Update status from API response
		org.Status.OrganizationID = targetOrg.OrgID
		org.Status.OrganizationName = targetOrg.Name
		org.Status.Subnet = targetOrg.Subnet
		org.Status.BindingMode = "Bound"

	} else {
		// DISCOVERY MODE: Auto-discover first available organization
		orgs, err := apiClient.ListOrganizations(ctx)
		if err != nil {
			return fmt.Errorf("failed to list organizations: %w", err)
		}

		if len(orgs) == 0 {
			return fmt.Errorf("no organizations found")
		}

		// Use first organization
		org.Status.OrganizationID = orgs[0].OrgID
		org.Status.OrganizationName = orgs[0].Name
		org.Status.Subnet = orgs[0].Subnet
		org.Status.BindingMode = "Discovered"
	}

	return nil
}

// reconcileDomains fetches and caches all available domains for the organization.
//
// Domain Discovery:
//   - Fetches all domains from Pangolin API
//   - Converts to CRD format and stores in status.domains
//   - Makes domains available for resources to resolve domain names
//
// Default Domain Resolution (priority order):
//  1. Explicit domain from spec.defaults.defaultDomain:
//     - Can be domain ID (e.g., "domain1")
//     - Can be domain name (e.g., "dobryops.com")
//     - Resolved to domain ID via domain list
//  2. First verified domain from API:
//     - Used if no default specified
//     - Ensures domain is verified and functional
//
// Status Updates:
//   - domains: Complete list of available domains with verification status
//   - defaultDomainId: Resolved default domain ID for resources to use
//
// Error Handling:
//   - Failed domain resolution doesn't fail reconciliation
//   - Logs warning but continues with other domains
func (r *PangolinOrganizationReconciler) reconcileDomains(ctx context.Context, org *tunnelv1alpha1.PangolinOrganization, apiClient *pangolin.Client) error {
	logger := log.FromContext(ctx)

	if org.Status.OrganizationID == "" {
		return fmt.Errorf("organization ID not available")
	}

	// Fetch domains from Pangolin API
	domains, err := apiClient.ListDomains(ctx, org.Status.OrganizationID)
	if err != nil {
		return fmt.Errorf("failed to list domains: %w", err)
	}

	logger.Info("Found domains for organization", "orgId", org.Status.OrganizationID, "domainCount", len(domains))

	// Convert API domains to CRD format
	var crdDomains []tunnelv1alpha1.Domain
	for _, d := range domains {
		crdDomains = append(crdDomains, tunnelv1alpha1.Domain{
			DomainID:      d.DomainID,
			BaseDomain:    d.BaseDomain,
			Verified:      d.Verified,
			Type:          d.Type,
			Failed:        d.Failed,
			Tries:         d.Tries,
			ConfigManaged: d.ConfigManaged,
		})
	}

	// Update status with complete domain list
	org.Status.Domains = crdDomains

	// Resolve default domain if specified in spec
	if org.Spec.Defaults != nil && org.Spec.Defaults.DefaultDomain != "" {
		defaultDomainID, err := r.resolveDomainToID(org.Spec.Defaults.DefaultDomain, crdDomains)
		if err != nil {
			logger.Info("Failed to resolve default domain", "defaultDomain", org.Spec.Defaults.DefaultDomain, "error", err)
			// Don't fail reconciliation for domain resolution errors
		} else {
			org.Status.DefaultDomainID = defaultDomainID
			logger.Info("Resolved default domain", "defaultDomain", org.Spec.Defaults.DefaultDomain, "domainId", defaultDomainID)
		}
	}

	// If no default domain specified or resolution failed, use first verified domain
	if org.Status.DefaultDomainID == "" && len(crdDomains) > 0 {
		for _, domain := range crdDomains {
			if domain.Verified {
				org.Status.DefaultDomainID = domain.DomainID
				logger.Info("Using first verified domain as default", "domainId", domain.DomainID)
				break
			}
		}
	}

	return nil
}

// resolveDomainToID resolves a domain input to a domain ID.
//
// The input can be:
//  1. Domain ID (e.g., "domain1") - returned as-is if found
//  2. Domain name (e.g., "dobryops.com") - resolved to corresponding domain ID
//
// Resolution Process:
//   - First checks if input matches a domain ID
//   - Then checks if input matches a base domain name
//   - Returns error if no match found
//
// This allows users to specify domains by either ID or name in the spec,
// with the controller handling the resolution automatically.
func (r *PangolinOrganizationReconciler) resolveDomainToID(domainInput string, domains []tunnelv1alpha1.Domain) (string, error) {
	// Check if it's already a domain ID
	for _, domain := range domains {
		if domain.DomainID == domainInput {
			return domain.DomainID, nil
		}
	}

	// Check if it's a domain name (base domain)
	for _, domain := range domains {
		if domain.BaseDomain == domainInput {
			return domain.DomainID, nil
		}
	}

	return "", fmt.Errorf("domain %s not found", domainInput)
}

// updateOrganizationStatus updates the status of a PangolinOrganization with the given status and message.
//
// Status values:
//   - "Ready": Organization is configured, domains are cached
//   - "Error": Reconciliation encountered an error
//
// The function also updates the Ready condition with appropriate reason and message.
// If status is not "Ready", the reconcile will be requeued after 1 minute.
func (r *PangolinOrganizationReconciler) updateOrganizationStatus(ctx context.Context, org *tunnelv1alpha1.PangolinOrganization, status, message string) (ctrl.Result, error) {
	org.Status.Status = status
	org.Status.ObservedGeneration = org.Generation

	// Create Ready condition
	conditionType := "Ready"
	conditionStatus := metav1.ConditionTrue
	reason := "ReconcileSuccess"

	if status != "Ready" {
		conditionStatus = metav1.ConditionFalse
		reason = "ReconcileError"
	}

	now := metav1.NewTime(time.Now())
	newCondition := metav1.Condition{
		Type:               conditionType,
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: org.Generation,
	}

	// Update existing condition or append new one
	conditionUpdated := false
	for i, condition := range org.Status.Conditions {
		if condition.Type == conditionType {
			// Preserve LastTransitionTime if status hasn't changed
			if condition.Status != conditionStatus {
				newCondition.LastTransitionTime = now
			} else {
				newCondition.LastTransitionTime = condition.LastTransitionTime
			}
			org.Status.Conditions[i] = newCondition
			conditionUpdated = true
			break
		}
	}

	if !conditionUpdated {
		org.Status.Conditions = append(org.Status.Conditions, newCondition)
	}

	// Update status
	err := r.Status().Update(ctx, org)
	if status != "Ready" {
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	return ctrl.Result{}, err
}

// handleOrganizationDeletion handles cleanup when a PangolinOrganization is being deleted.
//
// Cleanup Process:
//   - No API cleanup is performed (organization remains in Pangolin)
//   - Dependent resources (tunnels, resources) should be deleted first
//   - Finalizer is removed to allow organization deletion
//
// Note: Organizations are typically not deleted from Pangolin via API as they
// may contain resources not managed by this operator. Only the Kubernetes
// representation is cleaned up.
func (r *PangolinOrganizationReconciler) handleOrganizationDeletion(ctx context.Context, org *tunnelv1alpha1.PangolinOrganization) (ctrl.Result, error) {
	// Organizations are not deleted from Pangolin API
	// Only remove the finalizer to allow Kubernetes to delete the resource
	controllerutil.RemoveFinalizer(org, OrganizationFinalizerName)
	return ctrl.Result{}, r.Update(ctx, org)
}

// SetupWithManager sets up the controller with the Manager.
//
// Controller Configuration:
//   - Watches PangolinOrganization resources for changes
//   - Does not watch Secrets directly (manual trigger required for secret changes)
func (r *PangolinOrganizationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tunnelv1alpha1.PangolinOrganization{}).
		Complete(r)
}
