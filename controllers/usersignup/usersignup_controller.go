package usersignup

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	toolchainv1alpha1 "github.com/codeready-toolchain/api/api/v1alpha1"
	"github.com/codeready-toolchain/host-operator/controllers/toolchainconfig"
	"github.com/codeready-toolchain/host-operator/pkg/counter"
	"github.com/codeready-toolchain/host-operator/pkg/metrics"
	"github.com/codeready-toolchain/host-operator/pkg/pending"
	"github.com/codeready-toolchain/host-operator/pkg/segment"
	"github.com/codeready-toolchain/host-operator/pkg/templates/notificationtemplates"
	commoncontrollers "github.com/codeready-toolchain/toolchain-common/controllers"
	"github.com/codeready-toolchain/toolchain-common/pkg/cluster"
	"github.com/codeready-toolchain/toolchain-common/pkg/condition"
	"github.com/codeready-toolchain/toolchain-common/pkg/hash"
	notify "github.com/codeready-toolchain/toolchain-common/pkg/notification"
	"github.com/codeready-toolchain/toolchain-common/pkg/spacebinding"
	"github.com/codeready-toolchain/toolchain-common/pkg/states"
	"github.com/codeready-toolchain/toolchain-common/pkg/usersignup"
	"github.com/redhat-cop/operator-utils/pkg/util"

	"github.com/go-logr/logr"
	errs "github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

type StatusUpdaterFunc func(userAcc *toolchainv1alpha1.UserSignup, message string) error

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr manager.Manager) error {
	unapprovedMapper := pending.NewUserSignupMapper(mgr.GetClient())
	return ctrl.NewControllerManagedBy(mgr).
		For(&toolchainv1alpha1.UserSignup{}, builder.WithPredicates(UserSignupChangedPredicate{})).
		Owns(&toolchainv1alpha1.MasterUserRecord{}).
		Watches(
			&source.Kind{Type: &toolchainv1alpha1.BannedUser{}},
			handler.EnqueueRequestsFromMapFunc(MapBannedUserToUserSignup(mgr.GetClient()))).
		Watches(
			&source.Kind{Type: &toolchainv1alpha1.Space{}},
			handler.EnqueueRequestsFromMapFunc(commoncontrollers.MapToOwnerByLabel(r.Namespace, toolchainv1alpha1.SpaceCreatorLabelKey))).
		Watches(
			&source.Kind{Type: &toolchainv1alpha1.SpaceBinding{}},
			handler.EnqueueRequestsFromMapFunc(commoncontrollers.MapToOwnerByLabel(r.Namespace, toolchainv1alpha1.SpaceCreatorLabelKey))).
		Watches(
			&source.Kind{Type: &toolchainv1alpha1.ToolchainStatus{}},
			handler.EnqueueRequestsFromMapFunc(unapprovedMapper.MapToOldestPending),
			builder.WithPredicates(&OnlyWhenAutomaticApprovalIsEnabled{
				client: mgr.GetClient(),
			})).
		Complete(r)
}

// Reconciler reconciles a UserSignup object
type Reconciler struct {
	*StatusUpdater
	Namespace         string
	Scheme            *runtime.Scheme
	GetMemberClusters cluster.GetMemberClustersFunc
	SegmentClient     *segment.Client
}

//+kubebuilder:rbac:groups=toolchain.dev.openshift.com,resources=usersignups,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=toolchain.dev.openshift.com,resources=usersignups/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=toolchain.dev.openshift.com,resources=usersignups/finalizers,verbs=update

//+kubebuilder:rbac:groups=toolchain.dev.openshift.com,resources=bannedusers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=toolchain.dev.openshift.com,resources=bannedusers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=toolchain.dev.openshift.com,resources=bannedusers/finalizers,verbs=update

// Reconcile reads that state of the cluster for a UserSignup object and makes changes based on the state read
// and what is in the UserSignup.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *Reconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling UserSignup")

	// Fetch the UserSignup instance
	userSignup := &toolchainv1alpha1.UserSignup{}
	err := r.Client.Get(context.TODO(), request.NamespacedName, userSignup)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}
	logger = logger.WithValues("username", userSignup.Spec.Username)

	if util.IsBeingDeleted(userSignup) {
		logger.Info("The UserSignup is being deleted")
		return reconcile.Result{}, nil
	}

	if userSignup.GetLabels() == nil {
		userSignup.Labels = make(map[string]string)
	}
	if userSignup.Labels[toolchainv1alpha1.UserSignupStateLabelKey] == "" {
		if err := r.setStateLabel(logger, userSignup, toolchainv1alpha1.UserSignupStateLabelValueNotReady); err != nil {
			return reconcile.Result{}, err
		}
	}

	config, err := toolchainconfig.GetToolchainConfig(r.Client)
	if err != nil {
		return reconcile.Result{}, err
	}

	banned, err := r.isUserBanned(logger, userSignup)
	if err != nil {
		return reconcile.Result{}, err
	}

	// If the usersignup is not banned and not deactivated then ensure the deactivated notification status is set to false.
	// This is especially important for cases when a user is deactivated and then reactivated because the status is used to
	// trigger sending of the notification. If a user is reactivated a notification should be sent to the user again.

	if !banned && !states.Deactivated(userSignup) {
		if err := r.updateStatus(logger, userSignup, r.setStatusDeactivationNotificationUserIsActive); err != nil {
			return reconcile.Result{}, err
		}
	}

	// If the usersignup is not banned and not within the pre-deactivation period then ensure the deactivation notification
	// status is set to false. This is especially important for cases when a user is deactivated and then reactivated
	// because the status is used to trigger sending of the notification. If a user is reactivated a notification should
	// be sent to the user again.
	if !banned && !states.Deactivating(userSignup) && !states.Deactivated(userSignup) {
		if err := r.updateStatus(logger, userSignup, r.setStatusDeactivatingNotificationNotInPreDeactivation); err != nil {
			return reconcile.Result{}, err
		}
	}

	if states.Deactivating(userSignup) && condition.IsNotTrue(userSignup.Status.Conditions,
		toolchainv1alpha1.UserSignupUserDeactivatingNotificationCreated) {

		if err := r.sendDeactivatingNotification(logger, config, userSignup); err != nil {
			logger.Error(err, "Failed to create user deactivating notification")

			// set the failed to create notification status condition
			return reconcile.Result{}, r.wrapErrorWithStatusUpdate(logger, userSignup,
				r.setStatusDeactivatingNotificationCreationFailed, err, "Failed to create user deactivating notification")
		}

		if err := r.updateStatus(logger, userSignup, r.setStatusDeactivatingNotificationCreated); err != nil {
			logger.Error(err, "Failed to update notification created status")
			return reconcile.Result{}, err
		}
	}

	if exists, err := r.checkIfMurAlreadyExists(logger, config, userSignup, banned); err != nil || exists {
		return reconcile.Result{}, err
	}

	// If there is no MasterUserRecord created, yet the UserSignup is Banned, simply set the status
	// and return
	if banned {
		// if the UserSignup doesn't have the state=banned label set, then update it
		if err := r.setStateLabel(logger, userSignup, toolchainv1alpha1.UserSignupStateLabelValueBanned); err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, r.updateStatus(logger, userSignup, r.setStatusBanned)
	}

	// Check if the user has been deactivated
	if states.Deactivated(userSignup) {
		return r.handleDeactivatedUserSignup(logger, config, userSignup)
	}

	return reconcile.Result{}, r.ensureNewMurIfApproved(logger, config, userSignup)
}

// handleDeactivatedUserSignup defines the workflow for deactivated users
//
// If there is no MasterUserRecord created, yet the UserSignup is marked as Deactivated, set the status,
// send a notification to the user, and return
func (r *Reconciler) handleDeactivatedUserSignup(logger logr.Logger, config toolchainconfig.ToolchainConfig,
	userSignup *toolchainv1alpha1.UserSignup) (ctrl.Result, error) {

	// Only send the deactivated notification if the previous state was "approved", i.e. we will only send the
	// deactivated notification to the user if the account is currently active and is being deactivated
	if userSignup.Labels[toolchainv1alpha1.UserSignupStateLabelKey] == toolchainv1alpha1.UserSignupStateLabelValueApproved &&
		condition.IsNotTrue(userSignup.Status.Conditions, toolchainv1alpha1.UserSignupUserDeactivatedNotificationCreated) {
		if err := r.sendDeactivatedNotification(logger, config, userSignup); err != nil {
			logger.Error(err, "Failed to create user deactivation notification")

			// set the failed to create notification status condition
			return reconcile.Result{}, r.wrapErrorWithStatusUpdate(logger, userSignup, r.setStatusDeactivationNotificationCreationFailed, err, "Failed to create user deactivation notification")
		}

		if err := r.updateStatus(logger, userSignup, r.setStatusDeactivationNotificationCreated); err != nil {
			logger.Error(err, "Failed to update notification created status")
			return reconcile.Result{}, err
		}
	}

	// if the UserSignup doesn't have the state=deactivated label set, then update it
	if err := r.setStateLabel(logger, userSignup, toolchainv1alpha1.UserSignupStateLabelValueDeactivated); err != nil {
		return reconcile.Result{}, err
	}

	err := r.updateStatus(logger, userSignup, r.setStatusDeactivated)
	if err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// Is the user banned? To determine this we query the BannedUser resource for any matching entries.  The query
// is based on the user's emailHash value - if there is a match, and the e-mail addresses are equal, then the
// user is banned.
func (r *Reconciler) isUserBanned(reqLogger logr.Logger, userSignup *toolchainv1alpha1.UserSignup) (bool, error) {
	banned := false
	// Lookup the user email annotation
	if emailLbl, exists := userSignup.Annotations[toolchainv1alpha1.UserSignupUserEmailAnnotationKey]; exists {

		// Lookup the email hash label
		if emailHashLbl, exists := userSignup.Labels[toolchainv1alpha1.UserSignupUserEmailHashLabelKey]; exists {

			labels := map[string]string{toolchainv1alpha1.BannedUserEmailHashLabelKey: emailHashLbl}
			opts := client.MatchingLabels(labels)
			bannedUserList := &toolchainv1alpha1.BannedUserList{}

			// Query BannedUser for resources that match the same email hash
			if err := r.Client.List(context.TODO(), bannedUserList, opts); err != nil {
				return false, r.wrapErrorWithStatusUpdate(reqLogger, userSignup, r.setStatusFailedToReadBannedUsers, err, "Failed to query BannedUsers")
			}

			// One last check to confirm that the e-mail addresses match also (in case of the infinitesimal chance of a hash collision)
			for _, bannedUser := range bannedUserList.Items {
				if bannedUser.Spec.Email == emailLbl {
					banned = true
					break
				}
			}

			hashIsValid := validateEmailHash(emailLbl, emailHashLbl)
			if !hashIsValid {
				err := fmt.Errorf("hash is invalid")
				return banned, r.wrapErrorWithStatusUpdate(reqLogger, userSignup, r.setStatusInvalidEmailHash, err, "the email hash '%s' is invalid ", emailHashLbl)
			}
		} else {
			// If there isn't an email-hash label, then the state is invalid
			err := fmt.Errorf("missing label at usersignup")
			return banned, r.wrapErrorWithStatusUpdate(reqLogger, userSignup, r.setStatusMissingEmailHash, err,
				"the required label '%s' is not present", toolchainv1alpha1.UserSignupUserEmailHashLabelKey)
		}
	} else {
		err := fmt.Errorf("missing annotation at usersignup")
		return banned, r.wrapErrorWithStatusUpdate(reqLogger, userSignup, r.setStatusInvalidMissingUserEmailAnnotation, err,
			"the required annotation '%s' is not present", toolchainv1alpha1.UserSignupUserEmailAnnotationKey)
	}
	return banned, nil
}

// checkIfMurAlreadyExists checks if there is already a MUR for the given UserSignup.
// If there is already one then it returns 'true' as the first returned value, but before doing that it checks if the MUR should be deleted or not
// or if the MUR requires some migration changes or additional fixes.
// If no MUR for the given UserSignup is found, then it returns 'false' as the first returned value.
func (r *Reconciler) checkIfMurAlreadyExists(reqLogger logr.Logger, config toolchainconfig.ToolchainConfig, userSignup *toolchainv1alpha1.UserSignup,
	banned bool) (bool, error) {
	// List all MasterUserRecord resources that have an owner label equal to the UserSignup.Name
	murList := &toolchainv1alpha1.MasterUserRecordList{}
	labels := map[string]string{toolchainv1alpha1.MasterUserRecordOwnerLabelKey: userSignup.Name}
	opts := client.MatchingLabels(labels)
	if err := r.Client.List(context.TODO(), murList, opts); err != nil {
		return false, r.wrapErrorWithStatusUpdate(reqLogger, userSignup, r.setStatusInvalidMURState, err,
			"Failed to list MasterUserRecords by owner")
	}

	murs := murList.Items
	// If we found more than one MasterUserRecord, then die
	if len(murs) > 1 {
		err := fmt.Errorf("multiple matching MasterUserRecord resources found")
		return false, r.wrapErrorWithStatusUpdate(reqLogger, userSignup, r.setStatusInvalidMURState, err, "Multiple MasterUserRecords found")
	} else if len(murs) == 1 {
		mur := &murs[0]
		// If the user has been banned, then we need to delete the MUR
		if banned {
			// set the state label to banned
			if err := r.setStateLabel(reqLogger, userSignup, toolchainv1alpha1.UserSignupStateLabelValueBanned); err != nil {
				return true, err
			}

			reqLogger.Info("Deleting MasterUserRecord since user has been banned")
			return true, r.deleteMasterUserRecord(mur, userSignup, reqLogger, r.setStatusBanning)
		}

		// If the user has been deactivated, then we need to delete the MUR
		if states.Deactivated(userSignup) {
			// We set the inProgressStatusUpdater parameter here to setStatusDeactivationInProgress, as a temporary status before
			// the main reconcile function completes the deactivation process
			reqLogger.Info("Deleting MasterUserRecord since user has been deactivated")
			return true, r.deleteMasterUserRecord(mur, userSignup, reqLogger, r.setStatusDeactivationInProgress)
		}

		// if the UserSignup doesn't have the state=approved label set, then update it
		if err := r.setStateLabel(reqLogger, userSignup, toolchainv1alpha1.UserSignupStateLabelValueApproved); err != nil {
			return true, err
		}

		// look-up the UserTier to set it on the MUR if not set
		userTier, err := r.getUserTier(reqLogger, config, userSignup)
		if err != nil {
			return true, r.wrapErrorWithStatusUpdate(reqLogger, userSignup, r.setStatusNoUserTierAvailable, err, "")
		}

		// look-up the NSTemplateTier to set it on the MUR if not set
		spaceTier, err := r.getNSTemplateTier(reqLogger, config, userSignup)
		if err != nil {
			return true, r.wrapErrorWithStatusUpdate(reqLogger, userSignup, r.setStatusNoTemplateTierAvailable, err, "")
		}

		// check if anything in the MUR should be migrated/fixed
		if changed := migrateOrFixMurIfNecessary(mur, userTier, userSignup); changed {
			reqLogger.Info("Updating MasterUserRecord after it was migrated")
			if err := r.Client.Update(context.TODO(), mur); err != nil {
				return true, r.wrapErrorWithStatusUpdate(reqLogger, userSignup, r.setStatusInvalidMURState, err, "unable update MasterUserRecord to complete migration")
			}
			return true, nil
		}
		reqLogger.Info("MasterUserRecord exists", "Name", mur.Name)

		if shouldManageSpace(userSignup) {
			space, created, err := r.ensureSpace(reqLogger, userSignup, mur, spaceTier)
			// if there was an error or the space was created then return to complete the reconcile, another reconcile will occur when space is created since this controller watches spaces
			if err != nil || created {
				return true, err
			}

			if err = r.ensureSpaceBinding(reqLogger, userSignup, mur, space); err != nil {
				return true, err
			}

			// if the Space is not Ready, another reconcile will occur when space status is updated since this controller watches space (without a predicate)
			reqLogger.Info("Checking whether Space is Ready", "Space", space.Name)
			if !condition.IsTrueWithReason(space.Status.Conditions, toolchainv1alpha1.ConditionReady, toolchainv1alpha1.SpaceProvisionedReason) {
				return true, r.updateIncompleteStatus(userSignup, fmt.Sprintf("space %s was not ready", space.Name))
			}
		}

		reqLogger.Info("Setting UserSignup status to 'Complete'")
		return true, r.updateStatus(reqLogger, userSignup, r.updateCompleteStatus(reqLogger, mur.Name))
	}
	return false, nil
}

func (r *Reconciler) ensureNewMurIfApproved(reqLogger logr.Logger, config toolchainconfig.ToolchainConfig, userSignup *toolchainv1alpha1.UserSignup) error {
	// Check if the user requires phone verification, and do not proceed further if they do
	if states.VerificationRequired(userSignup) {
		return r.updateStatus(reqLogger, userSignup, r.setStatusVerificationRequired)
	}

	approved, targetCluster, err := getClusterIfApproved(r.Client, userSignup, r.GetMemberClusters)
	reqLogger.Info("ensuring MUR", "approved", approved, "target_cluster", targetCluster, "error", err)
	// if error was returned or no available cluster found
	if err != nil || targetCluster == notFound {
		// set the state label to pending
		if err := r.setStateLabel(reqLogger, userSignup, toolchainv1alpha1.UserSignupStateLabelValuePending); err != nil {
			return err
		}
		// if user was approved manually
		if states.ApprovedManually(userSignup) {
			if err == nil {
				err = fmt.Errorf("no suitable member cluster found - capacity was reached")
			}
			return r.wrapErrorWithStatusUpdate(reqLogger, userSignup, r.set(statusApprovedByAdmin, statusNoClustersAvailable), err, "no target clusters available")
		}

		// if an error was returned, then log it, set the status and return an error
		if err != nil {
			return r.wrapErrorWithStatusUpdate(reqLogger, userSignup, r.set(statusPendingApproval, statusNoClustersAvailable), err, "getting target clusters failed")
		}
		// in case no error was returned which means that no cluster was found, then just wait for next reconcile triggered by ToolchainStatus update
		return r.updateStatus(reqLogger, userSignup, r.set(statusPendingApproval, statusNoClustersAvailable))
	}

	if !approved {
		// set the state label to pending
		if err := r.setStateLabel(reqLogger, userSignup, toolchainv1alpha1.UserSignupStateLabelValuePending); err != nil {
			return err
		}
		return r.updateStatus(reqLogger, userSignup, r.set(statusPendingApproval, statusIncompletePendingApproval))
	}

	if states.ApprovedManually(userSignup) {
		if err := r.updateStatus(reqLogger, userSignup, r.set(statusApprovedByAdmin)); err != nil {
			return err
		}
	} else {
		if err := r.updateStatus(reqLogger, userSignup, r.setStatusApprovedAutomatically); err != nil {
			return err
		}
	}
	// set the state label to approved
	if err := r.setStateLabel(reqLogger, userSignup, toolchainv1alpha1.UserSignupStateLabelValueApproved); err != nil {
		return err
	}
	userTier, err := r.getUserTier(reqLogger, config, userSignup)
	if err != nil {
		return r.wrapErrorWithStatusUpdate(reqLogger, userSignup, r.setStatusNoUserTierAvailable, err, "")
	}

	// Provision the MasterUserRecord
	return r.provisionMasterUserRecord(reqLogger, config, userSignup, targetCluster, userTier)
}

func (r *Reconciler) getUserTier(reqLogger logr.Logger, config toolchainconfig.ToolchainConfig, userSignup *toolchainv1alpha1.UserSignup) (*toolchainv1alpha1.UserTier, error) {
	tierName := config.Tiers().DefaultUserTier()
	if event, err := r.getSocialEvent(userSignup); err != nil {
		return nil, err
	} else if event != nil {
		tierName = event.Spec.UserTier
	}
	reqLogger.Info("looking-up UserTier", "name", tierName)
	userTier := &toolchainv1alpha1.UserTier{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{Namespace: userSignup.Namespace, Name: tierName}, userTier)
	return userTier, err
}

func (r *Reconciler) getNSTemplateTier(reqLogger logr.Logger, config toolchainconfig.ToolchainConfig, userSignup *toolchainv1alpha1.UserSignup) (*toolchainv1alpha1.NSTemplateTier, error) {
	tierName := config.Tiers().DefaultSpaceTier()
	if event, err := r.getSocialEvent(userSignup); err != nil {
		return nil, err
	} else if event != nil {
		tierName = event.Spec.SpaceTier
	}
	reqLogger.Info("looking-up NSTemplateTier", "name", tierName)
	nstemplateTier := &toolchainv1alpha1.NSTemplateTier{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{Namespace: userSignup.Namespace, Name: tierName}, nstemplateTier)
	return nstemplateTier, err
}

func (r *Reconciler) getSocialEvent(userSignup *toolchainv1alpha1.UserSignup) (*toolchainv1alpha1.SocialEvent, error) {
	eventName, found := userSignup.Labels[toolchainv1alpha1.SocialEventUserSignupLabelKey]
	if !found {
		return nil, nil
	}
	event := &toolchainv1alpha1.SocialEvent{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{Namespace: userSignup.Namespace, Name: eventName}, event)
	switch {
	case err != nil && errors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	default:
		return event, nil
	}
}

func (r *Reconciler) setStateLabel(logger logr.Logger, userSignup *toolchainv1alpha1.UserSignup, state string) error {
	oldState := userSignup.Labels[toolchainv1alpha1.UserSignupStateLabelKey]
	if oldState == state {
		// skipping
		return nil
	}
	userSignup.Labels[toolchainv1alpha1.UserSignupStateLabelKey] = state
	activations := 0
	if state == toolchainv1alpha1.UserSignupStateLabelValueApproved {
		activations = r.updateActivationCounterAnnotation(logger, userSignup)
	}
	if err := r.Client.Update(context.TODO(), userSignup); err != nil {
		return r.wrapErrorWithStatusUpdate(logger, userSignup, r.setStatusFailedToUpdateStateLabel, err,
			"unable to update state label at UserSignup resource")
	}
	r.updateUserSignupMetricsByState(logger, userSignup, oldState, state)
	// increment the counter *only if the client update did not fail*
	domain := metrics.GetEmailDomain(userSignup)
	counter.UpdateUsersPerActivationCounters(logger, activations, domain) // will ignore if `activations == 0`
	return nil
}

func (r *Reconciler) updateUserSignupMetricsByState(logger logr.Logger, userSignup *toolchainv1alpha1.UserSignup, oldState string, newState string) {
	if oldState == "" {
		metrics.UserSignupUniqueTotal.Inc()
	}
	switch newState {
	case toolchainv1alpha1.UserSignupStateLabelValueApproved:
		metrics.UserSignupApprovedTotal.Inc()
		// track activation in Segment
		if r.SegmentClient != nil {
			r.SegmentClient.TrackAccountActivation(userSignup.Spec.Username)
		} else {
			logger.Info("segment client not configure to track account activations")
		}
	case toolchainv1alpha1.UserSignupStateLabelValueDeactivated:
		if oldState == toolchainv1alpha1.UserSignupStateLabelValueApproved {
			metrics.UserSignupDeactivatedTotal.Inc()
		}
	case toolchainv1alpha1.UserSignupStateLabelValueBanned:
		metrics.UserSignupBannedTotal.Inc()
	}
}

func (r *Reconciler) generateCompliantUsername(config toolchainconfig.ToolchainConfig, instance *toolchainv1alpha1.UserSignup) (string, error) {
	replaced := usersignup.TransformUsername(instance.Spec.Username)

	// Check for any forbidden prefixes
	for _, prefix := range config.Users().ForbiddenUsernamePrefixes() {
		if strings.HasPrefix(replaced, prefix) {
			replaced = fmt.Sprintf("%s%s", "crt-", replaced)
			break
		}
	}

	// Check for any forbidden suffixes
	for _, suffix := range config.Users().ForbiddenUsernameSuffixes() {
		if strings.HasSuffix(replaced, suffix) {
			replaced = fmt.Sprintf("%s%s", replaced, "-crt")
			break
		}
	}

	validationErrors := validation.IsQualifiedName(replaced)
	if len(validationErrors) > 0 {
		return "", fmt.Errorf(fmt.Sprintf("transformed username [%s] is invalid", replaced))
	}

	transformed := replaced

	for i := 2; i < 101; i++ { // No more than 100 attempts to find a vacant name
		mur := &toolchainv1alpha1.MasterUserRecord{}
		// Check if a MasterUserRecord exists with the same transformed name
		namespacedMurName := types.NamespacedName{Namespace: instance.Namespace, Name: transformed}
		err := r.Client.Get(context.TODO(), namespacedMurName, mur)
		if err != nil {
			if !errors.IsNotFound(err) {
				return "", err
			}
			// If there was a NotFound error looking up the mur, it means we found an available name
			return transformed, nil
		} else if mur.Labels[toolchainv1alpha1.MasterUserRecordOwnerLabelKey] == instance.Name {
			// If the found MUR has the same UserID as the UserSignup, then *it* is the correct MUR -
			// Return an error here and allow the reconcile() function to pick it up on the next loop
			return "", fmt.Errorf(fmt.Sprintf("INFO: could not generate compliant username as MasterUserRecord with the same name [%s] and user id [%s] already exists. The next reconcile loop will pick it up.", mur.Name, instance.Name))
		}

		transformed = fmt.Sprintf("%s-%d", replaced, i)
	}

	return "", fmt.Errorf(fmt.Sprintf("unable to transform username [%s] even after 100 attempts", instance.Spec.Username))
}

// provisionMasterUserRecord does the work of provisioning the MasterUserRecord
func (r *Reconciler) provisionMasterUserRecord(logger logr.Logger, config toolchainconfig.ToolchainConfig, userSignup *toolchainv1alpha1.UserSignup, targetCluster targetCluster,
	userTier *toolchainv1alpha1.UserTier) error {

	// Set the last-target-cluster annotation so that if the user signs up again later on, they can be provisioned to the same cluster
	userSignup.Annotations[toolchainv1alpha1.UserSignupLastTargetClusterAnnotationKey] = targetCluster.getClusterName()
	if err := r.Client.Update(context.TODO(), userSignup); err != nil {
		return r.wrapErrorWithStatusUpdate(logger, userSignup, r.setStatusFailedToUpdateAnnotation, err,
			"unable to update last target cluster annotation on UserSignup resource")
	}

	compliantUsername, err := r.generateCompliantUsername(config, userSignup)
	if err != nil {
		return r.wrapErrorWithStatusUpdate(logger, userSignup, r.setStatusFailedToCreateMUR, err,
			"Error generating compliant username for %s", userSignup.Spec.Username)
	}

	mur := newMasterUserRecord(userSignup, targetCluster.getClusterName(), userTier.Name, compliantUsername)

	if err := controllerutil.SetControllerReference(userSignup, mur, r.Scheme); err != nil {
		return r.wrapErrorWithStatusUpdate(logger, userSignup, r.setStatusFailedToCreateMUR, err,
			"Error setting controller reference for MasterUserRecord %s", mur.Name)
	}

	logger.Info("Creating MasterUserRecord", "Name", mur.Name)
	err = r.Client.Create(context.TODO(), mur)
	if err != nil {
		return r.wrapErrorWithStatusUpdate(logger, userSignup, r.setStatusFailedToCreateMUR, err,
			"error creating MasterUserRecord")
	}
	// increment the counter of MasterUserRecords
	domain := metrics.GetEmailDomain(mur)
	counter.IncrementMasterUserRecordCount(logger, domain)

	logger.Info("Created MasterUserRecord", "Name", mur.Name, "TargetCluster", targetCluster)
	return nil
}

// ensureSpace does the work of provisioning the Space
func (r *Reconciler) ensureSpace(logger logr.Logger, userSignup *toolchainv1alpha1.UserSignup, mur *toolchainv1alpha1.MasterUserRecord, spaceTier *toolchainv1alpha1.NSTemplateTier) (*toolchainv1alpha1.Space, bool, error) {
	logger.Info("Ensuring Space", "UserSignup", userSignup.Name, "MUR", mur.Name, "NSTemplateTier", spaceTier.Name)

	space := &toolchainv1alpha1.Space{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{
		Namespace: userSignup.Namespace,
		Name:      mur.Name,
	}, space)
	if err == nil {
		if util.IsBeingDeleted(space) {
			return nil, false, fmt.Errorf("cannot create space because it is currently being deleted")
		}
		logger.Info("Space exists")
		return space, false, nil
	}

	if !errors.IsNotFound(err) {
		return nil, false, errs.Wrap(err, fmt.Sprintf(`failed to get Space associated with mur "%s"`, mur.Name))
	}

	if len(mur.Spec.UserAccounts) == 0 || mur.Spec.UserAccounts[0].TargetCluster == "" {
		return nil, false, fmt.Errorf("unable to get target cluster from masteruserrecord for space creation")
	}
	tCluster := targetCluster(mur.Spec.UserAccounts[0].TargetCluster)

	space = newSpace(userSignup, tCluster, mur.Name, spaceTier.Name)

	err = r.Client.Create(context.TODO(), space)
	if err != nil {
		return nil, false, r.wrapErrorWithStatusUpdate(logger, userSignup, r.setStatusFailedToCreateSpace, err,
			"error creating Space")
	}

	logger.Info("Created Space", "name", space.Name, "target_cluster", tCluster, "NSTemplateTier", spaceTier.Name)
	return space, true, nil
}

// ensureSpaceBinding creates a SpaceBinding for the provided MUR and Space if one does not exist
func (r *Reconciler) ensureSpaceBinding(logger logr.Logger, userSignup *toolchainv1alpha1.UserSignup, mur *toolchainv1alpha1.MasterUserRecord, space *toolchainv1alpha1.Space) error {
	logger.Info("Ensuring SpaceBinding", "MUR", mur.Name, "Space", space.Name)

	spaceBindings := &toolchainv1alpha1.SpaceBindingList{}
	labels := map[string]string{
		toolchainv1alpha1.SpaceBindingMasterUserRecordLabelKey: mur.Name,
		toolchainv1alpha1.SpaceBindingSpaceLabelKey:            space.Name,
	}
	opts := client.MatchingLabels(labels)
	if err := r.Client.List(context.TODO(), spaceBindings, opts); err != nil {
		return errs.Wrap(err, fmt.Sprintf(`attempt to list SpaceBinding associated with mur '%s' and space '%s' failed`, mur.Name, space.Name))
	}

	if len(spaceBindings.Items) == 1 {
		spaceBinding := spaceBindings.Items[0]
		if util.IsBeingDeleted(&spaceBinding) {
			return fmt.Errorf("cannot create SpaceBinding because it is currently being deleted")
		}
		logger.Info("SpaceBinding already exists")
		return nil
	} else if len(spaceBindings.Items) > 1 {
		return fmt.Errorf(`unable to proceed because there are multiple SpaceBindings associated with MasterUserRecord '%s' and Space '%s'`, mur.Name, space.Name)
	}

	spaceBinding := spacebinding.NewSpaceBinding(mur, space, userSignup.Name)

	if err := r.Client.Create(context.TODO(), spaceBinding); err != nil {
		return r.wrapErrorWithStatusUpdate(logger, userSignup, r.setStatusFailedToCreateSpaceBinding, err,
			"error creating SpaceBinding")
	}

	logger.Info("Created SpaceBinding", "MUR", mur.Name, "Space", space.Name)
	return nil
}

// updateActivationCounterAnnotation increments the 'toolchain.dev.openshift.com/activation-counter' annotation value on the given UserSignup
func (r *Reconciler) updateActivationCounterAnnotation(logger logr.Logger, userSignup *toolchainv1alpha1.UserSignup) int {
	if activations, exists := userSignup.Annotations[toolchainv1alpha1.UserSignupActivationCounterAnnotationKey]; exists {
		logger.Info("updating 'toolchain.dev.openshift.com/activation-counter' on active user")
		activations, err := strconv.Atoi(activations)
		if err == nil {
			// increment the value of the annotation
			activations++
			userSignup.Annotations[toolchainv1alpha1.UserSignupActivationCounterAnnotationKey] = strconv.Itoa(activations)
			return activations
		}
		logger.Error(err, "The 'toolchain.dev.openshift.com/activation-counter' annotation value was not an integer and was reset to '1'.", "value", activations)
		// "best effort": reset number of activations to 1 for this user
		userSignup.Annotations[toolchainv1alpha1.UserSignupActivationCounterAnnotationKey] = "1"
		return 1
	}
	// annotation was missing so assume it's the first activation
	logger.Info("setting 'toolchain.dev.openshift.com/activation-counter' on new active user")
	userSignup.Annotations[toolchainv1alpha1.UserSignupActivationCounterAnnotationKey] = "1" // first activation, annotation did not exist
	return 1
}

// deleteMasterUserRecord deletes the specified MasterUserRecord
func (r *Reconciler) deleteMasterUserRecord(mur *toolchainv1alpha1.MasterUserRecord,
	userSignup *toolchainv1alpha1.UserSignup, logger logr.Logger,
	inProgressStatusUpdater StatusUpdaterFunc) error {

	logger.Info("Deleting MasterUserRecord", "MUR", mur.Name)
	err := r.updateStatus(logger, userSignup, inProgressStatusUpdater)
	if err != nil {
		return err
	}

	err = r.Client.Delete(context.TODO(), mur)
	if err != nil {
		return r.wrapErrorWithStatusUpdate(logger, userSignup, r.setStatusFailedToDeleteMUR, err,
			"error deleting MasterUserRecord")
	}
	logger.Info("Deleted MasterUserRecord", "MUR", mur.Name)
	return nil
}

func (r *Reconciler) sendDeactivatingNotification(logger logr.Logger, config toolchainconfig.ToolchainConfig, userSignup *toolchainv1alpha1.UserSignup) error {
	labels := map[string]string{
		toolchainv1alpha1.NotificationUserNameLabelKey: userSignup.Status.CompliantUsername,
		toolchainv1alpha1.NotificationTypeLabelKey:     toolchainv1alpha1.NotificationTypeDeactivating,
	}
	opts := client.MatchingLabels(labels)
	notificationList := &toolchainv1alpha1.NotificationList{}
	if err := r.Client.List(context.TODO(), notificationList, opts); err != nil {
		return err
	}

	// if there is no existing notification with these labels
	if len(notificationList.Items) == 0 {

		keysAndVals := map[string]string{
			toolchainconfig.NotificationContextRegistrationURLKey: config.RegistrationService().RegistrationServiceURL(),
		}

		notification, err := notify.NewNotificationBuilder(r.Client, userSignup.Namespace).
			WithTemplate(notificationtemplates.UserDeactivating.Name).
			WithNotificationType(toolchainv1alpha1.NotificationTypeDeactivating).
			WithControllerReference(userSignup, r.Scheme).
			WithUserContext(userSignup).
			WithKeysAndValues(keysAndVals).
			Create(userSignup.Annotations[toolchainv1alpha1.UserSignupUserEmailAnnotationKey])

		if err != nil {
			logger.Error(err, "Failed to create deactivating notification resource")
			return err
		}

		logger.Info(fmt.Sprintf("Deactivating notification resource [%s] created", notification.Name))
	}
	return nil
}

func (r *Reconciler) sendDeactivatedNotification(logger logr.Logger, config toolchainconfig.ToolchainConfig, userSignup *toolchainv1alpha1.UserSignup) error {
	labels := map[string]string{
		toolchainv1alpha1.NotificationUserNameLabelKey: userSignup.Status.CompliantUsername,
		toolchainv1alpha1.NotificationTypeLabelKey:     toolchainv1alpha1.NotificationTypeDeactivated,
	}
	opts := client.MatchingLabels(labels)
	notificationList := &toolchainv1alpha1.NotificationList{}
	if err := r.Client.List(context.TODO(), notificationList, opts); err != nil {
		return err
	}

	// if there is no existing notification with these labels
	if len(notificationList.Items) == 0 {
		keysAndVals := map[string]string{
			toolchainconfig.NotificationContextRegistrationURLKey: config.RegistrationService().RegistrationServiceURL(),
		}

		notification, err := notify.NewNotificationBuilder(r.Client, userSignup.Namespace).
			WithTemplate(notificationtemplates.UserDeactivated.Name).
			WithNotificationType(toolchainv1alpha1.NotificationTypeDeactivated).
			WithControllerReference(userSignup, r.Scheme).
			WithUserContext(userSignup).
			WithKeysAndValues(keysAndVals).
			Create(userSignup.Annotations[toolchainv1alpha1.UserSignupUserEmailAnnotationKey])

		if err != nil {
			logger.Error(err, "Failed to create deactivated notification resource")
			return err
		}

		logger.Info(fmt.Sprintf("Deactivated notification resource [%s] created", notification.Name))
	}
	return nil
}

// validateEmailHash calculates an md5 hash value for the provided userEmail string, and compares it to the provided
// userEmailHash.  If the values are the same the function returns true, otherwise it will return false
func validateEmailHash(userEmail, userEmailHash string) bool {
	return hash.EncodeString(userEmail) == userEmailHash
}

func shouldManageSpace(userSignup *toolchainv1alpha1.UserSignup) bool {
	return userSignup.Annotations[toolchainv1alpha1.SkipAutoCreateSpaceAnnotationKey] != "true"
}
